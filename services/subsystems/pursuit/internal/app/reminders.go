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
	// 负责人此刻不满足写入准入（不在团队中，或以本人判断不能更新、授权同步与授予版本未满足）时转给决策者（C-11：与 getTask 的
	// viewer_escalated 同一口径，见 assigneeAdmitted）
	recipient := t.Assignee
	admitted, err := s.assigneeAdmitted(ctx, tenant, cell, p, t.Assignee)
	if err != nil {
		return false, err
	}
	escalated := !admitted
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

// assigneeAdmitted 以任务负责人本人判断其此刻能否更新跟进（与写命令同一准入 admitWrite：能否更新、授权同步与本人当前角色的授予版本）；
// 负责人已不在团队中（失效的负责人、被移出的协作者）为 false。判断不可用时返回错误（失败关闭：不当作能更新，也不当作不能）。
func (s *Service) assigneeAdmitted(ctx context.Context, tenant, cell string, p *Pursuit, assignee string) (bool, error) {
	if !p.involves(assignee) {
		return false, nil
	}
	mctx := asMember(ctx, tenant, cell, assignee)
	mrc, _ := reqctx.From(mctx)
	r, err := s.rights(mctx, mrc, p, nil)
	if err != nil {
		return false, err
	}
	return s.admitWrite(p, r, assignee) == nil, nil
}

// escalatedFor 判断任务是否已转给调用者（决策者）处理（C-11，getTask 的 viewer_escalated 与 listMyTasks 的 escalated 同一口径）：
// 调用者在单元上能更新跟进，且任务负责人已不在团队中；或负责人仍在团队中、但当前截止版本已有一条转给决策者的提醒发出（handedOver）
// 且负责人此刻仍不满足写入准入（以负责人本人实时判断，不以提醒发出时为准）。判断不可用时返回错误。
func (s *Service) escalatedFor(ctx context.Context, rc reqctx.Context, p *Pursuit, t *Task, viewer Rights, handedOver bool) (bool, error) {
	if p.State != "open" || !viewer.UnitEdit || (t.Assignee == rc.Principal.ID && p.involves(rc.Principal.ID)) {
		return false, nil
	}
	if !p.involves(t.Assignee) {
		return true, nil
	}
	if !handedOver {
		return false, nil
	}
	admitted, err := s.assigneeAdmitted(ctx, rc.Tenant, rc.Cell, p, t.Assignee)
	return !admitted, err
}

// handedOver 判断任务的当前截止版本是否已有一条转给决策者的提醒发出。
func (s *Service) handedOver(ctx context.Context, t *Task) (bool, error) {
	var n int
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pur_reminders WHERE task_id = $1 AND deadline_version = $2 AND state = 'sent' AND escalated`,
			t.ID, t.DeadlineVersion).Scan(&n)
	})
	return n > 0, err
}
