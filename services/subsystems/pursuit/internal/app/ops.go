package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 操作与尝试（PRD 第6段 F11）：操作标识（operation_id）固定一次用户意图，提交后的确定结果（已创建、已存在、成功、无变化与确定失败）
// 固定在操作标识上，同一操作标识再次提交返回已固定的结果（他人或不同请求使用同一操作标识为 409 operation_conflict）；幂等键是一次尝试，
// 同键重发返回该尝试的结果，包括失败与暂时失败（503，未提交，只记在尝试上；同一操作标识的新尝试用新键照常执行）。
// 固定的是结果本身（标识、结果与未写入项），不是投影：每次返回前按调用者当前的权限重新投影（command.go 的 project）。

// Fixed 是固定在操作标识上的结果。
type Fixed struct {
	OperationID       string   `json:"operation_id"`
	Result            string   `json:"result"`
	PursuitID         string   `json:"pursuit_id,omitempty"`
	TaskID            string   `json:"task_id,omitempty"`
	OutcomeID         string   `json:"outcome_id,omitempty"`
	NotWritten        []string `json:"not_written,omitempty"`
	SignalRestoration string   `json:"signal_restoration,omitempty"`
}

// op 是一次要执行的操作。
type op struct {
	id, fp, action, channel string
	pursuit, task           string
}

type stored struct {
	actor, fp, outcome string
	status             int
	response           []byte
}

func readOp(ctx context.Context, tx pgx.Tx, id string) (*stored, error) {
	var o stored
	err := tx.QueryRow(ctx, `SELECT actor, fingerprint, outcome, status, response FROM pur_operations WHERE operation_id = $1`, id).
		Scan(&o.actor, &o.fp, &o.outcome, &o.status, &o.response)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// problem 是失败的固定响应（契约 Problem 的 code、current 与 signal_current）。
type problem struct {
	Status        int             `json:"status"`
	Code          string          `json:"code"`
	Current       json.RawMessage `json:"current,omitempty"`
	SignalCurrent json.RawMessage `json:"signal_current,omitempty"`
}

func problemOf(e *api.Error) []byte {
	raw, _ := json.Marshal(problem{Status: e.Status, Code: e.Code, Current: e.Current, SignalCurrent: e.SignalCurrent})
	return raw
}

func problemError(status int, raw []byte) *api.Error {
	var p problem
	_ = json.Unmarshal(raw, &p)
	return &api.Error{Status: status, Code: p.Code, Current: p.Current, SignalCurrent: p.SignalCurrent}
}

// replay 返回已固定的结果（含确定失败）。
func replay(o *stored, actor, fp string) (*Fixed, error) {
	if o.actor != actor || o.fp != fp {
		return nil, failure(409, "operation_conflict")
	}
	if o.outcome == "failed" {
		return nil, problemError(o.status, o.response)
	}
	var out Fixed
	if err := json.Unmarshal(o.response, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func insertOp(ctx context.Context, tx pgx.Tx, rc reqctx.Context, o op, f *Fixed, e *api.Error, parts any, s *Service) error {
	outcome, status, response := "", 200, []byte(nil)
	var code *string
	if e != nil {
		outcome, status, response, code = "failed", e.Status, problemOf(e), &e.Code
	} else {
		outcome = f.Result
		response, _ = json.Marshal(f)
	}
	rawParts := []byte(`{}`)
	if parts != nil {
		rawParts, _ = json.Marshal(parts)
	}
	_, err := tx.Exec(ctx, `INSERT INTO pur_operations (tenant_id, operation_id, actor, fingerprint, action, pursuit_id, task_id, channel, outcome, status, code,
 response, parts, recorded_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		rc.Tenant, o.id, rc.Principal.ID, o.fp, o.action, optional(o.pursuit), optional(o.task), o.channel, outcome, status, code, response, rawParts, s.businessNow())
	return err
}

func lockOp(ctx context.Context, tx pgx.Tx, rc reqctx.Context, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "pur-op/"+rc.Tenant+"/"+id)
	return err
}

// lookup 返回操作标识上已固定的结果（没有为 nil, nil）。
func (s *Service) lookup(ctx context.Context, rc reqctx.Context, o op) (*Fixed, bool, error) {
	var prev *stored
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		prev, err = readOp(ctx, tx, o.id)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if prev == nil {
		return nil, false, nil
	}
	f, err := replay(prev, rc.Principal.ID, o.fp)
	return f, true, err
}

// fix 把操作开始后、写入之前得到的确定失败固定在操作标识上（单独的事务）；并发的同一操作已固定结果时以它为准。
// 暂时失败（503）、授权同步中（403 authorization_syncing：同步完成后同一操作可以成功）与非 *api.Error 的错误不固定。
func (s *Service) fix(ctx context.Context, rc reqctx.Context, o op, err error) (*Fixed, error) {
	var e *api.Error
	if !errors.As(err, &e) || e.Status >= 500 || e.Code == "authorization_syncing" {
		return nil, err
	}
	var out *Fixed
	terr := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockOp(ctx, tx, rc, o.id); err != nil {
			return err
		}
		prev, err := readOp(ctx, tx, o.id)
		if err != nil {
			return err
		}
		if prev != nil {
			out, err = replay(prev, rc.Principal.ID, o.fp)
			return err
		}
		return insertOp(ctx, tx, rc, o, nil, e, nil, s)
	})
	if terr != nil {
		return nil, terr
	}
	if out != nil {
		return out, nil
	}
	return nil, e
}

// commit 在一个事务中执行写入：加操作锁、再查已固定的结果，然后执行 fn。fn 返回确定失败（*api.Error，非 5xx）时与操作一起固定，
// 写入回滚到保存点；返回结果时连同撤销所需的前值（parts）固定。
func (s *Service) commit(ctx context.Context, rc reqctx.Context, o op, fn func(context.Context, pgx.Tx) (*Fixed, any, error)) (*Fixed, error) {
	var out *Fixed
	var definite *api.Error
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockOp(ctx, tx, rc, o.id); err != nil {
			return err
		}
		prev, err := readOp(ctx, tx, o.id)
		if err != nil {
			return err
		}
		if prev != nil {
			out, err = replay(prev, rc.Principal.ID, o.fp)
			if err != nil {
				var e *api.Error
				if errors.As(err, &e) {
					definite = e
					return nil
				}
			}
			return err
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		f, parts, err := fn(ctx, sp)
		var e *api.Error
		if err != nil && errors.As(err, &e) && e.Status < 500 {
			if rerr := sp.Rollback(ctx); rerr != nil {
				return rerr
			}
			definite = e
			return insertOp(ctx, tx, rc, o, nil, e, nil, s)
		}
		if err != nil {
			return err
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		f.OperationID = o.id
		out = f
		return insertOp(ctx, tx, rc, o, f, nil, parts, s)
	})
	if err != nil {
		return nil, err
	}
	if definite != nil {
		return nil, definite
	}
	return out, nil
}

// attempt 以（调用者、接口、幂等键）记录一次尝试：同键同请求返回该尝试的结果（含失败），同键不同请求 409 idempotency_conflict。
func (s *Service) attempt(ctx context.Context, rc reqctx.Context, scope, key, fp string, fn func() (*Fixed, error)) (*Fixed, error) {
	var prev []byte
	var prevStatus int
	var prevFP string
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT fingerprint, status, response FROM pur_attempts WHERE actor = $1 AND scope = $2 AND idem_key = $3`,
			rc.Principal.ID, scope, key).Scan(&prevFP, &prevStatus, &prev)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if prev != nil {
		return replayAttempt(prevFP, fp, prevStatus, prev)
	}
	out, runErr := fn()
	var body []byte
	code := 200
	var safe *api.Error
	switch {
	case runErr == nil:
		body, _ = json.Marshal(out)
	case errors.As(runErr, &safe):
		code, body = safe.Status, problemOf(safe)
	case errors.Is(runErr, reqctx.ErrMissing):
		return nil, runErr
	default:
		s.logger().WarnContext(ctx, "跟进命令失败", "scope", scope, "error", runErr.Error())
		safe = unavailable()
		code, body, runErr = 503, problemOf(safe), safe
	}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO pur_attempts (tenant_id, actor, scope, idem_key, fingerprint, status, response) VALUES ($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT DO NOTHING`, rc.Tenant, rc.Principal.ID, scope, key, fp, code, body)
		if err != nil || tag.RowsAffected() == 1 {
			return err
		}
		return tx.QueryRow(ctx, `SELECT fingerprint, status, response FROM pur_attempts WHERE actor = $1 AND scope = $2 AND idem_key = $3`,
			rc.Principal.ID, scope, key).Scan(&prevFP, &prevStatus, &prev)
	})
	if err != nil {
		s.logger().WarnContext(ctx, "尝试结果未能记录", "scope", scope)
	}
	if prev != nil {
		return replayAttempt(prevFP, fp, prevStatus, prev)
	}
	if runErr != nil {
		return nil, runErr
	}
	return out, nil
}

func replayAttempt(prevFP, fp string, status int, body []byte) (*Fixed, error) {
	if prevFP != fp {
		return nil, failure(409, "idempotency_conflict")
	}
	if status >= 400 {
		return nil, problemError(status, body)
	}
	var out Fixed
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
