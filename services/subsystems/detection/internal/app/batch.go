package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 批量处理（PRD 第3段 F6）：选中时固定信号标识清单（1–200），每项的操作标识为“批次标识:信号标识”，逐项按单条的规则执行。
// M2 在请求内逐项执行（不经工作流）：执行者以租约持有作业，每项之前检查取消并续租；进程中断时租约过期，未处理项可经重试继续。

// BatchRequest 是批量动作（契约 TriageSignalsRequest）。
type BatchRequest struct {
	Action  string           `json:"action"`
	Reasons []string         `json:"reasons,omitempty"`
	Note    string           `json:"note,omitempty"`
	Dismiss *bool            `json:"dismiss,omitempty"`
	Items   []BatchItemInput `json:"items"`
	Channel string           `json:"channel,omitempty"`
}

// BatchItem 是逐项结果（契约 BatchItem）。
type BatchItem struct {
	SignalID    string          `json:"signal_id"`
	OperationID string          `json:"operation_id"`
	Result      string          `json:"result"`
	Failure     string          `json:"failure,omitempty"`
	Code        string          `json:"code,omitempty"`
	Parts       json.RawMessage `json:"parts,omitempty"`
	Attempts    int             `json:"attempts"`
}

// Summary 是逐项结果的汇总。
type Summary struct {
	Total           int `json:"total"`
	Succeeded       int `json:"succeeded"`
	NoChange        int `json:"no_change"`
	FailedDefinite  int `json:"failed_definite"`
	FailedTransient int `json:"failed_transient"`
	Unprocessed     int `json:"unprocessed"`
}

// Batch 是批量作业（契约 SignalBatch）。
type Batch struct {
	ID          string      `json:"id"`
	ObjectiveID string      `json:"objective_id"`
	Action      string      `json:"action"`
	Reasons     []string    `json:"reasons,omitempty"`
	Note        string      `json:"note,omitempty"`
	Dismiss     *bool       `json:"dismiss,omitempty"`
	State       string      `json:"state"`
	Summary     Summary     `json:"summary"`
	Items       []BatchItem `json:"items"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`

	createdBy string
}

// itemRequest 把批量动作转换为一项的处理动作。
func itemRequest(b *Batch, it BatchItemInput, channel string) Request {
	req := Request{OperationID: b.ID + ":" + it.SignalID, SeenContent: it.SeenContent, StateVersion: it.StateVersion,
		FeedbackVersion: it.FeedbackVersion, Channel: channel}
	switch b.Action {
	case "dismiss", "restore":
		req.Action = b.Action
	default:
		req.Action = "feedback"
		req.Feedback = &FeedbackInput{Result: b.Action, Reasons: b.Reasons, Note: b.Note}
		req.Dismiss = b.Dismiss != nil && *b.Dismiss
	}
	return req
}

func batchFingerprint(batch, signal string) string { return fingerprint("batch", batch, signal) }

// readBatch 读取作业与逐项结果；只有发起人可读（其他人、其他租户与不存在同为 404 signal_batch_not_found）。
func readBatch(ctx context.Context, tx pgx.Tx, id, actor string) (*Batch, error) {
	b := &Batch{}
	var note, leaseToken *string
	var dismiss, cancel, leased bool
	var reasons []string
	err := tx.QueryRow(ctx, `SELECT id, objective_id, created_by, action, reasons, note, dismiss, cancel_requested, lease_token,
 COALESCE(lease_until > now(), false), created_at, updated_at FROM det_signal_batches WHERE id = $1`, id).
		Scan(&b.ID, &b.ObjectiveID, &b.createdBy, &b.Action, &reasons, &note, &dismiss, &cancel, &leaseToken, &leased, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b.createdBy != actor) {
		return nil, failure(404, "signal_batch_not_found")
	}
	if err != nil {
		return nil, err
	}
	if len(reasons) > 0 {
		b.Reasons = reasons
	}
	if note != nil {
		b.Note = *note
	}
	if b.Action == "irrelevant" {
		b.Dismiss = &dismiss
	}
	b.CreatedAt, b.UpdatedAt = b.CreatedAt.UTC(), b.UpdatedAt.UTC()
	rows, err := tx.Query(ctx, `SELECT signal_id, result, failure, code, parts, attempts FROM det_signal_batch_items WHERE batch_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b.Items = []BatchItem{}
	for rows.Next() {
		var it BatchItem
		var fail, code *string
		var parts []byte
		if err := rows.Scan(&it.SignalID, &it.Result, &fail, &code, &parts, &it.Attempts); err != nil {
			return nil, err
		}
		it.OperationID = b.ID + ":" + it.SignalID
		if fail != nil {
			it.Failure = *fail
		}
		if code != nil {
			it.Code = *code
		}
		if parts != nil {
			it.Parts = parts
		}
		b.Items = append(b.Items, it)
		b.Summary.Total++
		switch {
		case it.Result == "succeeded":
			b.Summary.Succeeded++
		case it.Result == "no_change":
			b.Summary.NoChange++
		case it.Result == "unprocessed":
			b.Summary.Unprocessed++
		case it.Failure == "definite":
			b.Summary.FailedDefinite++
		default:
			b.Summary.FailedTransient++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sm := b.Summary
	switch {
	case sm.Unprocessed > 0 && leased:
		b.State = "running"
	case sm.Unprocessed > 0 && cancel:
		b.State = "cancelled"
	case sm.Unprocessed > 0:
		b.State = "interrupted"
	case sm.FailedDefinite+sm.FailedTransient == 0:
		b.State = "completed"
	case sm.Succeeded+sm.NoChange == 0:
		b.State = "failed"
	default:
		b.State = "partial"
	}
	return b, nil
}

func (s *Service) getBatch(ctx context.Context, id, actor string) (*Batch, error) {
	var out *Batch
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = readBatch(ctx, tx, id, actor)
		return err
	})
	return out, err
}

// GetSignalBatch 读取作业（契约 getSignalBatch）。
func (s *Service) GetSignalBatch(ctx context.Context, id string) (*Batch, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.getBatch(ctx, id, rc.Principal.ID)
}

// errLeaseLost 表示执行者的租约已被他人取得或作业已不存在：停止执行。
var errLeaseLost = errors.New("批量作业租约丢失")

// execute 以租约逐项执行作业中的未处理项与暂时失败项。另一执行者持有有效租约时 409 signal_batch_running。
func (s *Service) execute(ctx context.Context, rc reqctx.Context, id string) error {
	token := ids.New("evt")
	ttl := fmt.Sprintf("%d milliseconds", s.leaseTTL().Milliseconds())
	var b *Batch
	var channel string
	var pending []int
	var inputs map[int]BatchItemInput
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE det_signal_batches SET lease_token = $3, lease_until = now() + $4::interval, cancel_requested = false, updated_at = $5
 WHERE id = $1 AND created_by = $2 AND (lease_until IS NULL OR lease_until <= now())`, id, rc.Principal.ID, token, ttl, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if _, err := readBatch(ctx, tx, id, rc.Principal.ID); err != nil {
				return err
			}
			return failure(409, "signal_batch_running")
		}
		if b, err = readBatch(ctx, tx, id, rc.Principal.ID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT channel FROM det_signal_batches WHERE id = $1`, id).Scan(&channel); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT position, signal_id, seen_content_version, state_version, feedback_version FROM det_signal_batch_items
 WHERE batch_id = $1 AND (result = 'unprocessed' OR failure = 'transient') ORDER BY position`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		inputs = map[int]BatchItemInput{}
		for rows.Next() {
			var pos int
			var it BatchItemInput
			if err := rows.Scan(&pos, &it.SignalID, &it.SeenContent, &it.StateVersion, &it.FeedbackVersion); err != nil {
				return err
			}
			pending, inputs[pos] = append(pending, pos), it
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, pos := range pending {
		// 每项之前：续租并检查取消（取消后不再发起新的尝试，进行中的尝试照常结束）
		var cancelled bool
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `UPDATE det_signal_batches SET lease_until = now() + $3::interval WHERE id = $1 AND lease_token = $2 RETURNING cancel_requested`,
				id, token, ttl).Scan(&cancelled)
			if errors.Is(err, pgx.ErrNoRows) {
				return errLeaseLost
			}
			return err
		})
		if errors.Is(err, errLeaseLost) {
			return nil
		}
		if err != nil {
			return err
		}
		if cancelled {
			break
		}
		if s.Hook != nil {
			if err := s.Hook(ctx, id, pos); err != nil {
				return nil // 模拟进程中断：不释放租约，未处理项保持未处理
			}
		}
		it := inputs[pos]
		res, runErr := s.run(ctx, rc, op{id: id + ":" + it.SignalID, fingerprint: batchFingerprint(id, it.SignalID), signal: it.SignalID,
			objective: b.ObjectiveID, batch: id, req: itemRequest(b, it, channel)})
		result, failureKind, code := "", "", ""
		var parts []byte
		var safe *api.Error
		switch {
		case runErr == nil:
			result = res.Result
			parts, _ = json.Marshal(res.Parts)
		case errors.As(runErr, &safe) && safe.Status < 500:
			result, failureKind, code = "failed", "definite", safe.Code
		case errors.As(runErr, &safe):
			result, failureKind, code = "failed", "transient", safe.Code
		default:
			result, failureKind, code = "failed", "transient", "detection_unavailable"
		}
		var fk, cd *string
		if failureKind != "" {
			fk, cd = &failureKind, &code
		}
		if err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE det_signal_batch_items SET result = $3, failure = $4, code = $5, parts = $6, attempts = attempts + 1
 WHERE batch_id = $1 AND position = $2`, id, pos, result, fk, cd, parts)
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE det_signal_batches SET updated_at = $2 WHERE id = $1`, id, s.now())
			}
			return err
		}); err != nil {
			return err
		}
	}
	return s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE det_signal_batches SET lease_token = NULL, lease_until = NULL, updated_at = $3 WHERE id = $1 AND lease_token = $2`,
			id, token, s.now())
		return err
	})
}

// TriageSignals 创建并执行批量作业（契约 triageSignals）。同一幂等键重放返回同一作业（当前状态）。
func (s *Service) TriageSignals(ctx context.Context, objective, key string, req BatchRequest) (*Batch, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	fp := fingerprint("batch", objective, req)
	body, err := s.attempt(ctx, rc, "batch", key, fp, 202, func() (any, error) { return s.createBatch(ctx, rc, objective, req) })
	if err != nil {
		return nil, err
	}
	var ref struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, err
	}
	return s.getBatch(ctx, ref.ID, rc.Principal.ID)
}

func (s *Service) createBatch(ctx context.Context, rc reqctx.Context, objective string, req BatchRequest) (*Batch, error) {
	if len(req.Items) == 0 || len(req.Items) > batchLimit {
		return nil, failure(400, "invalid_request")
	}
	seen := map[string]bool{}
	for _, it := range req.Items {
		if seen[it.SignalID] {
			return nil, failure(400, "invalid_request")
		}
		seen[it.SignalID] = true
	}
	dismiss := false
	switch req.Action {
	case "relevant", "irrelevant", "exclusion_correct", "exclusion_wrong":
		if req.Action == "irrelevant" {
			dismiss = req.Dismiss == nil || *req.Dismiss // 不相关缺省同时忽略（PRD 第3段第13节第1项）
		}
		var reasons *FeedbackReasons
		var code string
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `SELECT objective_code FROM det_signals WHERE objective_id = $1 LIMIT 1`, objective).Scan(&code)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		if s.Packs != nil {
			reasons = s.Packs.Feedback[code]
		}
		excluded := req.Action == "exclusion_correct" || req.Action == "exclusion_wrong"
		if code != "" {
			if e := validateFeedback(&FeedbackInput{Result: req.Action, Reasons: req.Reasons, Note: req.Note}, excluded, reasons); e != nil {
				return nil, e
			}
		}
	case "dismiss", "restore":
		if len(req.Reasons) > 0 || req.Note != "" {
			return nil, failure(400, "invalid_request")
		}
	default:
		return nil, failure(400, "invalid_request")
	}
	visible, granted, err := s.access(ctx, objective, relViewSignals, relFeedback)
	if err != nil {
		return nil, err
	}
	if !visible {
		return nil, failure(404, "objective_not_found")
	}
	if !has(granted, relFeedback) {
		return nil, failure(403, "forbidden")
	}
	id, at := ids.New("sgb"), s.now()
	channel := req.Channel
	if channel == "" {
		channel = "web"
	}
	reasons := req.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	var note *string
	if req.Note != "" {
		note = &req.Note
	}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO det_signal_batches (id, tenant_id, objective_id, created_by, action, reasons, note, dismiss, channel, created_at, updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`, id, rc.Tenant, objective, rc.Principal.ID, req.Action, reasons, note, dismiss, channel, at); err != nil {
			return err
		}
		for i, it := range req.Items {
			if _, err := tx.Exec(ctx, `INSERT INTO det_signal_batch_items (tenant_id, batch_id, position, signal_id, seen_content_version, state_version, feedback_version)
 VALUES ($1,$2,$3,$4,$5,$6,$7)`, rc.Tenant, id, i, it.SignalID, it.SeenContent, it.StateVersion, it.FeedbackVersion); err != nil {
				return err
			}
		}
		return recorder.Record(ctx, tx, audit.Entry{Action: "detection.signal_batch.create", Resource: fmt.Sprintf("%s/%s/%s/items:%d", objective, id, req.Action, len(req.Items)),
			Result: "succeeded", Basis: relFeedback})
	})
	if err != nil {
		return nil, err
	}
	if err := s.execute(ctx, rc, id); err != nil {
		var safe *api.Error
		if !errors.As(err, &safe) || safe.Code != "signal_batch_running" {
			s.logger().WarnContext(ctx, "批量作业执行中断", "batch", id, "error", err)
		}
	}
	return &Batch{ID: id}, nil
}

// CancelSignalBatch 取消作业（契约 cancelSignalBatch）：之后不再发起新的尝试；作业已结束时为无操作。
func (s *Service) CancelSignalBatch(ctx context.Context, id, key string) (*Batch, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	_, err = s.attempt(ctx, rc, "batch-cancel", key, fingerprint("cancel", id), 200, func() (any, error) {
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := readBatch(ctx, tx, id, rc.Principal.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE det_signal_batches SET cancel_requested = true, updated_at = $2 WHERE id = $1
 AND EXISTS (SELECT 1 FROM det_signal_batch_items WHERE batch_id = $1 AND result = 'unprocessed')`, id, s.now()); err != nil {
				return err
			}
			return recorder.Record(ctx, tx, audit.Entry{Action: "detection.signal_batch.cancel", Resource: id, Result: "succeeded", Basis: "batch_creator"})
		})
		return map[string]string{"id": id}, err
	})
	if err != nil {
		return nil, err
	}
	return s.getBatch(ctx, id, rc.Principal.ID)
}

// RetrySignalBatchItems 为暂时失败项与未处理项发起新的尝试（契约 retrySignalBatchItems）：同一操作标识、新的尝试，
// 已提交的项不会重复执行，确定失败项不在其中。
func (s *Service) RetrySignalBatchItems(ctx context.Context, id, key string) (*Batch, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	_, err = s.attempt(ctx, rc, "batch-retry", key, fingerprint("retry", id), 202, func() (any, error) {
		if _, err := s.getBatch(ctx, id, rc.Principal.ID); err != nil {
			return nil, err
		}
		if err := s.execute(ctx, rc, id); err != nil {
			return nil, err
		}
		return map[string]string{"id": id}, nil
	})
	if err != nil {
		return nil, err
	}
	return s.getBatch(ctx, id, rc.Principal.ID)
}
