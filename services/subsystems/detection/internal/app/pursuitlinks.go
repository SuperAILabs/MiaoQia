package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
)

// 跟进关联事件（PRD 第6段 F3 第1条）。
const (
	PursuitSignalLinked   = "pursuit.pursuit.signal_linked.v1"
	PursuitSignalUnlinked = "pursuit.pursuit.signal_unlinked.v1"
)

// baseline 是转换前的处理状态与人工处置。
type baseline struct {
	Status string  `json:"status"`
	Manual *string `json:"manual_disposition,omitempty"`
	Reason *string `json:"manual_reason,omitempty"`
}

type linkEvent struct {
	LinkID    string `json:"link_id"`
	PursuitID string `json:"pursuit_id"`
	SignalID  string `json:"signal_id"`
}

// lockedSignal 读取并锁定信号的处理状态（他租户或不存在时返回 found 为假）。
func lockedSignal(ctx context.Context, tx pgx.Tx, id string) (status string, manual, reason *string, stateVersion int64, base []byte, conv *int64, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT status, manual_disposition, manual_reason, state_version, conversion_baseline, conversion_state_version FROM det_signals
 WHERE id = $1 FOR UPDATE`, id).Scan(&status, &manual, &reason, &stateVersion, &base, &conv)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil, 0, nil, nil, false, nil
	}
	return status, manual, reason, stateVersion, base, conv, err == nil, err
}

// PursuitLinked 处理 pursuit.pursuit.signal_linked.v1：关联加入有效关联集合；集合从空变为非空时记录转换基线并转为已转跟进
// （信号已关闭时只记录关联，状态保持已关闭）。同一关联标识只生效一次，已移除的不再加入。
func (s *Service) PursuitLinked(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var ev linkEvent
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		return err
	}
	status, manual, reason, sv, _, _, found, err := lockedSignal(ctx, tx, ev.SignalID)
	if err != nil || !found {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO det_pursuit_links (link_id, tenant_id, signal_id, pursuit_id, state) VALUES ($1, current_setting('app.tenant_id', true), $2, $3, 'active')
 ON CONFLICT (link_id) DO NOTHING`, ev.LinkID, ev.SignalID, ev.PursuitID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM det_pursuit_links WHERE signal_id = $1 AND state = 'active' AND link_id <> $2`, ev.SignalID, ev.LinkID).Scan(&others); err != nil {
		return err
	}
	if others > 0 || status == "closed" {
		return nil
	}
	base, _ := json.Marshal(baseline{Status: status, Manual: manual, Reason: reason})
	if _, err := tx.Exec(ctx, `UPDATE det_signals SET status = 'converted', state_version = state_version + 1, conversion_baseline = $2, conversion_state_version = $3
 WHERE id = $1`, ev.SignalID, base, sv+1); err != nil {
		return err
	}
	return recorder.Record(ctx, tx, audit.Entry{Action: "detection.signal.converted", Resource: fmt.Sprintf("%s/link:%s/pursuit:%s/state:%d", ev.SignalID, ev.LinkID,
		ev.PursuitID, sv+1), Result: "succeeded", Basis: "pursuit_link"})
}

// PursuitUnlinked 处理 pursuit.pursuit.signal_unlinked.v1：关联移出集合（先于加入到达时记为墓碑）；集合变空时，只有状态版本仍为转换时的
// V 才恢复转换基线，否则保持当前值；之后清除基线。
func (s *Service) PursuitUnlinked(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var ev linkEvent
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		return err
	}
	_, _, _, sv, base, conv, found, err := lockedSignal(ctx, tx, ev.SignalID)
	if err != nil || !found {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO det_pursuit_links (link_id, tenant_id, signal_id, pursuit_id, state) VALUES ($1, current_setting('app.tenant_id', true), $2, $3, 'removed')
 ON CONFLICT (link_id) DO UPDATE SET state = 'removed' WHERE det_pursuit_links.state = 'active'`, ev.LinkID, ev.SignalID, ev.PursuitID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM det_pursuit_links WHERE signal_id = $1 AND state = 'active'`, ev.SignalID).Scan(&active); err != nil {
		return err
	}
	if active > 0 || base == nil {
		return nil
	}
	if conv != nil && *conv == sv {
		var b baseline
		if err := json.Unmarshal(base, &b); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE det_signals SET status = $2, manual_disposition = $3, manual_reason = $4, state_version = state_version + 1,
 conversion_baseline = NULL, conversion_state_version = NULL WHERE id = $1`, ev.SignalID, b.Status, b.Manual, b.Reason); err != nil {
			return err
		}
		return recorder.Record(ctx, tx, audit.Entry{Action: "detection.signal.conversion_restored", Resource: fmt.Sprintf("%s/link:%s/state:%d", ev.SignalID, ev.LinkID, sv+1),
			Result: "succeeded", Basis: "pursuit_link"})
	}
	_, err = tx.Exec(ctx, `UPDATE det_signals SET conversion_baseline = NULL, conversion_state_version = NULL WHERE id = $1`, ev.SignalID)
	return err
}
