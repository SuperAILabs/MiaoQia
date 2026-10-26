package app

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// RunDue 是 Reminders.RunDue（PRD 第6段 F5 第4–5条、F12 第4条）。每条提醒在自己的事务中处理（加行锁并跳过已被并发执行者锁住的），
// 发布前按当前值再检查，不以计划时的负责人或权限为准。
func (s *Service) RunDue(ctx context.Context, now time.Time, limit int) (int, error) {
	rc, err := serviceContext(ctx)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var due []string
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM pur_reminders WHERE state = 'planned' AND fire_at <= $1 ORDER BY fire_at, id LIMIT $2`, now, limit)
		if err != nil {
			return err
		}
		due, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, id := range due {
		var fired bool
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			fired, err = s.fire(ctx, tx, rc.Tenant, rc.Cell, id, now)
			return err
		})
		if err != nil {
			s.logger().WarnContext(ctx, "任务提醒暂未发送，稍后再试", "reminder", id, "error", err.Error())
			continue
		}
		if fired {
			sent++
		}
	}
	return sent, nil
}

// fire 处理一条到时的提醒；返回是否已发布。保持为已计划（稍后再试）的情况：参与版本尚未同步、判断失败。
func (s *Service) fire(ctx context.Context, tx pgx.Tx, tenant, cell, id string, now time.Time) (bool, error) {
	var taskID, kind string
	var dv int64
	var fireAt time.Time
	err := tx.QueryRow(ctx, `SELECT task_id, kind, deadline_version, fire_at FROM pur_reminders WHERE id = $1 AND state = 'planned' FOR UPDATE SKIP LOCKED`, id).
		Scan(&taskID, &kind, &dv, &fireAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // 已由并发的执行者处理或已取消
	}
	if err != nil {
		return false, err
	}
	cancel := func(reason string) (bool, error) {
		_, err := tx.Exec(ctx, `UPDATE pur_reminders SET state = 'cancelled', cancelled_by = $2 WHERE id = $1`, id, reason)
		return false, err
	}
	t, err := loadTask(ctx, tx, taskID, false)
	if err != nil {
		return false, err
	}
	p, err := loadPursuit(ctx, tx, t.PursuitID, false)
	if err != nil {
		return false, err
	}
	if p.State != "open" || t.Status != "todo" || t.DeadlineVersion != dv {
		return cancel("recheck")
	}
	// 同一任务、同一截止版本有更晚的已到时提醒：只发最晚的一条（“已过的只发一次”）
	var later int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pur_reminders WHERE task_id = $1 AND deadline_version = $2 AND state = 'planned' AND fire_at <= $3 AND fire_at > $4`,
		taskID, dv, now, fireAt).Scan(&later); err != nil {
		return false, err
	}
	if later > 0 {
		return cancel("superseded")
	}
	if p.Applied < p.Participation {
		return false, nil // 授权同步完成后再发（F12 第4条）
	}
	recipient, escalated := t.Assignee, false
	if p.involves(t.Assignee) {
		mctx := asMember(ctx, tenant, cell, t.Assignee)
		mrc, _ := reqctx.From(mctx)
		r, err := s.rights(mctx, mrc, p, nil)
		if err != nil {
			return false, err
		}
		escalated = !r.Edit
	} else {
		escalated = true
	}
	data := map[string]any{"reminder_id": id, "task_id": t.ID, "pursuit_id": p.ID, "objective_id": p.ObjectiveID, "org_unit_id": p.OrgUnitID, "kind": kind,
		"deadline_version": dv, "due": t.Due, "escalated": escalated, "fire_at": ts(fireAt)}
	if escalated {
		recipient = ""
	} else {
		data["recipient"] = recipient
	}
	if _, err := tx.Exec(ctx, `UPDATE pur_reminders SET state = 'sent', sent_at = $2, escalated = $3, recipient = $4 WHERE id = $1`, id, s.businessNow(), escalated,
		optional(recipient)); err != nil {
		return false, err
	}
	_, err = writer.Append(ctx, tx, outbox.Event{Type: TaskDue, Subject: t.ID, PartitionKey: p.ID, Data: data})
	return err == nil, err
}
