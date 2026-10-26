package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 撤销（PRD 第3段 F9 第6条）：撤销是新的操作，只能由原操作者发起，撤销不能再撤销。每项只补偿原操作实际改变的部分，补偿前逐部分
// 核对当前版本仍等于原操作写入后的版本；一项内各部分在同一事务中提交。

// UndoRequest 是撤销请求（契约 UndoRequest）。
type UndoRequest struct {
	OperationID string `json:"operation_id"`
	TargetOp    string `json:"target_operation_id,omitempty"`
	TargetBatch string `json:"target_batch_id,omitempty"`
	Channel     string `json:"channel,omitempty"` // web（缺省）/ wecom（简报卡片上的撤销，PRD 第5段 F4 第5条）
}

// UndoItem 是一项的撤销结果（契约 UndoItem）。
type UndoItem struct {
	SignalID    string          `json:"signal_id"`
	TargetOp    string          `json:"target_operation_id"`
	OperationID string          `json:"operation_id"`
	Result      string          `json:"result"`
	Failure     string          `json:"failure,omitempty"`
	Code        string          `json:"code,omitempty"`
	Parts       *Parts          `json:"parts,omitempty"`
	Current     json.RawMessage `json:"current,omitempty"`
}

// UndoResult 是撤销的逐项结果（契约 UndoResult）。
type UndoResult struct {
	OperationID string     `json:"operation_id"`
	Items       []UndoItem `json:"items"`
}

// target 是被撤销的一个操作。
type target struct {
	id, signal string
	content    int64
	parts      Parts
}

// maxUndoID 是撤销操作标识的最大长度：每项的操作标识为“撤销操作标识:信号标识”，须在操作标识的长度上限（192）内。
const maxUndoID = 160

func (s *Service) targets(ctx context.Context, rc reqctx.Context, req UndoRequest) ([]target, error) {
	var out []target
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if req.TargetOp != "" {
			var actor, action, outcome string
			err := tx.QueryRow(ctx, `SELECT actor, action, outcome FROM det_triage_ops WHERE operation_id = $1`, req.TargetOp).Scan(&actor, &action, &outcome)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (actor != rc.Principal.ID || outcome != "succeeded")) {
				return failure(404, "triage_operation_not_found")
			}
			if err != nil {
				return err
			}
			if action == "undo" {
				return failure(422, "not_applicable") // 撤销不能再撤销
			}
			rows, err = tx.Query(ctx, `SELECT operation_id, signal_id, content_version, parts FROM det_triage_ops WHERE operation_id = $1`, req.TargetOp)
			if err != nil {
				return err
			}
		} else {
			if _, err := readBatch(ctx, tx, req.TargetBatch, rc.Principal.ID); err != nil {
				return err
			}
			// 撤销范围：本批中已提交且至少一个部分改变的全部项（含“状态无变化，反馈已记录”的项）
			rows, err = tx.Query(ctx, `SELECT o.operation_id, o.signal_id, o.content_version, o.parts FROM det_triage_ops o
 JOIN det_signal_batch_items i ON i.batch_id = o.batch_id AND i.signal_id = o.signal_id
 WHERE o.batch_id = $1 AND o.outcome = 'succeeded' AND o.action <> 'undo' AND o.actor = $2 ORDER BY i.position`, req.TargetBatch, rc.Principal.ID)
			if err != nil {
				return err
			}
		}
		defer rows.Close()
		for rows.Next() {
			var t target
			var parts []byte
			if err := rows.Scan(&t.id, &t.signal, &t.content, &parts); err != nil {
				return err
			}
			if err := json.Unmarshal(parts, &t.parts); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// UndoSignalTriage 撤销一次单条操作或一个批次（契约 undoSignalTriage）。
func (s *Service) UndoSignalTriage(ctx context.Context, key string, req UndoRequest) (*UndoResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if (req.TargetOp == "") == (req.TargetBatch == "") || len(req.OperationID) > maxUndoID {
		return nil, failure(400, "invalid_request")
	}
	fp := fingerprint("undo", req)
	body, err := s.attempt(ctx, rc, "undo", key, fp, 200, func() (any, error) {
		targets, err := s.targets(ctx, rc, req)
		if err != nil {
			return nil, err
		}
		out := &UndoResult{OperationID: req.OperationID, Items: []UndoItem{}}
		for _, t := range targets {
			out.Items = append(out.Items, s.undoOne(ctx, rc, req.OperationID, req.Channel, t))
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	var out UndoResult
	return &out, json.Unmarshal(body, &out)
}

func failedItem(item UndoItem, err error) UndoItem {
	var safe *api.Error
	item.Result = "failed"
	if errors.As(err, &safe) {
		item.Code, item.Current = safe.Code, safe.Current
		item.Failure = "definite"
		if safe.Status >= 500 {
			item.Failure = "transient"
		}
		return item
	}
	item.Failure, item.Code = "transient", "detection_unavailable"
	return item
}

// undoOne 撤销一项。操作标识为“撤销操作标识:信号标识”，结果（成功或确定失败）固定在它上面。
func (s *Service) undoOne(ctx context.Context, rc reqctx.Context, undoID, channel string, t target) UndoItem {
	o := op{id: undoID + ":" + t.signal, fingerprint: fingerprint("undo", undoID, t.id), signal: t.signal,
		req: Request{OperationID: undoID + ":" + t.signal, Action: "undo", SeenContent: t.content, Channel: channel}}
	item := UndoItem{SignalID: t.signal, TargetOp: t.id, OperationID: o.id}
	undoOf := t.id
	var prev *stored
	var objective string
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if prev, err = readOp(ctx, tx, o.id); err != nil || prev != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT objective_id FROM det_signals WHERE id = $1`, t.signal).Scan(&objective)
	})
	if err != nil {
		return failedItem(item, err)
	}
	if prev != nil {
		out, err := replay[UndoItem](prev, rc.Principal.ID, o.fingerprint)
		if err != nil {
			return failedItem(item, err)
		}
		return *out
	}
	o.objective = objective
	visible, granted, err := s.access(ctx, objective, relViewSignals, relFeedback)
	if err != nil {
		return failedItem(item, err)
	}
	if !visible || !has(granted, relViewSignals) || !has(granted, relFeedback) {
		code := definite(403, "forbidden")
		if !visible || !has(granted, relViewSignals) {
			code = definite(404, "signal_not_found")
		}
		out, err := fixFailure[UndoItem](ctx, s, rc, o, code, &undoOf)
		if err != nil {
			return failedItem(item, err)
		}
		return *out
	}
	var out *UndoItem
	var fixed *api.Error
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "det-op/"+rc.Tenant+"/"+o.id); err != nil {
			return err
		}
		prev, err := readOp(ctx, tx, o.id)
		if err != nil {
			return err
		}
		if prev != nil {
			out, err = replay[UndoItem](prev, rc.Principal.ID, o.fingerprint)
			return err
		}
		c, err := lockSignal(ctx, tx, t.signal, rc.Principal.ID)
		if err != nil {
			return err
		}
		at := s.now()
		p, e := compensate(c, t)
		if e != nil {
			fixed = e
			return insertOp(ctx, tx, rc, o, "failed", e.Status, &e.Code, problemOf(e), nil, c.contentVersion, &undoOf, at)
		}
		if err := apply(ctx, tx, rc, c, p, at); err != nil {
			return err
		}
		res := item
		res.Result, res.Parts = "succeeded", &p.parts
		response, _ := json.Marshal(res)
		parts, _ := json.Marshal(p.parts)
		after, err := lockSignal(ctx, tx, t.signal, rc.Principal.ID)
		if err != nil {
			return err
		}
		if err := insertOp(ctx, tx, rc, o, "succeeded", 200, nil, response, parts, c.contentVersion, &undoOf, at); err != nil {
			return err
		}
		if err := publish(ctx, tx, o, c, p.parts, c.contentVersion, after.stateVersion, undoOf, at); err != nil {
			return err
		}
		out = &res
		return nil
	})
	if err != nil {
		return failedItem(item, err)
	}
	if fixed != nil {
		return failedItem(item, fixed)
	}
	return *out
}

// compensate 计算补偿：原操作之后内容已更新为 signal_updated，信号已关闭为 signal_closed；被改变的部分的当前版本不等于原操作
// 写入后的版本（之后有人改过，包括本人）为 conflict。
func compensate(c *current, t target) (*plan, *api.Error) {
	if c.status == "closed" {
		return nil, conflict("signal_closed", c)
	}
	if c.contentVersion != t.content {
		return nil, conflict("signal_updated", c)
	}
	p := &plan{status: c.status, manual: c.manual, fb: c.fb}
	if f := t.parts.Feedback; f != nil && f.Change == "changed" {
		if c.fbVersion != f.Version {
			return nil, conflict("conflict", c)
		}
		p.fb, p.fbChange = f.Before, true
		p.parts.Feedback = &FeedbackPart{Change: "changed", Before: c.fb, After: f.Before, Version: c.fbVersion + 1}
	}
	st, mp := t.parts.Status, t.parts.Manual
	statusChanged, manualChanged := st != nil && st.Change == "changed", mp != nil && mp.Change == "changed"
	if statusChanged || manualChanged {
		version := int64(0)
		if statusChanged {
			version = st.Version
		} else {
			version = mp.Version
		}
		if c.stateVersion != version {
			return nil, conflict("conflict", c)
		}
		p.stateChange = true
		next := c.stateVersion + 1
		if manualChanged {
			p.manual = mp.Before
			if p.manual == "none" {
				p.manual = ""
			}
			p.parts.Manual = &ManualPart{Change: "changed", Before: manualValue(c.manual), After: mp.Before, Version: next}
		}
		if statusChanged {
			p.status = st.Before
			p.parts.Status = &StatusPart{Change: "changed", Before: c.status, After: st.Before, Version: next}
		}
		if p.status == "triaged" && p.manual == "" {
			p.status = "open"
		}
	}
	return p, nil
}
