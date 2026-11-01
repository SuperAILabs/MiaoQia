package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 授权同步告警（PRD 第6段 F12 第2、6条；C-17）与转交提醒的转回（F12 第4条）。
//
// 告警：期望参与版本高于 identity 已应用的版本的跟进处于“待处理期间”（sync_pending_since 起，到同步完成时清空）。定时检查
// （Monitor.CheckSync，scheduler 角色的 pursuit.check-authorization-sync）在锁定跟进行后按当前值再检查：超过 SyncDelayedWait 发布一次
// authorization_sync_delayed，超过 SyncFailedWait 发布一次 authorization_sync_failed（跟进此时按 authorization() 的口径为“授权同步失败”，
// 只允许决策者改派或关闭；channels 据此通知目标所在单元的决策者），并各写一条运营告警日志（slog，alert 键为事件类型；告警路由尚未接入，
// A02 验收缺口 C-24）。每个待处理期间各至多一次（sync_delayed_at / sync_failed_at）。同步完成由 sync 在同一把行锁下清空期间并在告警过时
// 发布 authorization_sync_recovered；因此检查与同步无论先后与并发，都不会在同步完成之后告警，也不会漏发恢复。
//
// 转回：提醒发出时负责人不满足写入准入而转给决策者（escalated）的，负责人之后恢复写入准入时（如授予待同步的新负责人在同步完成后），
// 同一提醒以 handed_back 转回给负责人一次（Monitor.HandBack，pursuit.hand-back-reminders）。在跟进行锁下（与改派、任务命令同一把锁）
// 再检查，负责人以其本人实时判断，判断不可用时留待下一轮。

// 告警事件类型（contracts/events/pursuit/events.asyncapi.yaml 0.4.0）。
const (
	AuthorizationSyncDelayed   = "pursuit.pursuit.authorization_sync_delayed.v1"
	AuthorizationSyncFailed    = "pursuit.pursuit.authorization_sync_failed.v1"
	AuthorizationSyncRecovered = "pursuit.pursuit.authorization_sync_recovered.v1"
)

// SyncDelayedWait 是授权同步待处理的告警阈值（PRD 第6段 F12 第2条：超过 5 分钟告警）。
const SyncDelayedWait = 5 * time.Minute

// CheckSync 是 Monitor.CheckSync。
func (s *Service) CheckSync(ctx context.Context, now time.Time, limit int) (int, error) {
	if _, err := serviceContext(ctx); err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	now = now.UTC()
	var pending []string
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM pur_pursuits WHERE applied_version < participation_version AND sync_pending_since IS NOT NULL
 AND ((sync_delayed_at IS NULL AND sync_pending_since < $1) OR (sync_failed_at IS NULL AND sync_pending_since < $2)) ORDER BY sync_pending_since, id LIMIT $3`,
			now.Add(-SyncDelayedWait), now.Add(-SyncFailedWait), limit)
		if err != nil {
			return err
		}
		pending, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	raised := 0
	for _, id := range pending {
		var n int
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			n, err = s.checkSync(ctx, tx, id, now)
			return err
		})
		if err != nil {
			s.logger().WarnContext(ctx, "授权同步告警检查失败，下一轮再查", "pursuit", id, "error", err.Error())
			continue
		}
		raised += n
	}
	return raised, nil
}

// syncAlert 是一次告警的级别与事件。
type syncAlert struct {
	level, event, column string
	wait                 time.Duration
}

var syncAlerts = []syncAlert{
	{"delayed", AuthorizationSyncDelayed, "sync_delayed_at", SyncDelayedWait},
	{"failed", AuthorizationSyncFailed, "sync_failed_at", SyncFailedWait},
}

// checkSync 在锁定跟进行（跳过已被并发的同步或检查锁住的）后按当前值再检查并发布到达阈值、本期间尚未发布的告警；返回发布数。
func (s *Service) checkSync(ctx context.Context, tx pgx.Tx, id string, now time.Time) (int, error) {
	var delayed, failedAt *time.Time
	p, err := scanPursuit(tx.QueryRow(ctx, `SELECT `+pursuitColumns+`, sync_delayed_at, sync_failed_at FROM pur_pursuits WHERE id = $1 FOR UPDATE SKIP LOCKED`, id),
		&delayed, &failedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // 并发的同步或检查正持有行锁：下一轮再查
	}
	if err != nil {
		return 0, err
	}
	// 同步已完成（期间已结束）或没有待处理期间：不告警
	if p.Applied >= p.Participation || p.PendingSince == nil {
		return 0, nil
	}
	raised := 0
	for _, a := range syncAlerts {
		already := delayed
		if a.level == "failed" {
			already = failedAt
		}
		if already != nil || now.Sub(*p.PendingSince) <= a.wait {
			continue
		}
		tag, err := tx.Exec(ctx, `UPDATE pur_pursuits SET `+a.column+` = $2 WHERE id = $1 AND `+a.column+` IS NULL AND sync_pending_since = $3
 AND applied_version < participation_version`, id, now, *p.PendingSince)
		if err != nil {
			return raised, err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		data := map[string]any{"pursuit_id": p.ID, "objective_id": p.ObjectiveID, "org_unit_id": p.OrgUnitID, "kind": p.Kind, "state": p.State,
			"desired_version": p.Participation, "applied_version": p.Applied, "pending_since": ts(*p.PendingSince), "detected_at": ts(now)}
		if _, err := writer.Append(ctx, tx, outbox.Event{Type: a.event, Subject: p.ID, PartitionKey: p.ID, Data: data}); err != nil {
			return raised, err
		}
		if a.level == "failed" {
			if err := timelineAt(ctx, tx, p.TenantID, p.ID, "authorization_sync_failed", "svc:"+api.Name, "授权同步超过 24 小时未成功，只允许决策者改派或关闭", now); err != nil {
				return raised, err
			}
		}
		if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.authorization.sync_" + a.level, Resource: fmt.Sprintf("%s/participation:%d", p.ID, p.Participation),
			Result: "failed", Basis: "sync_pending_since=" + ts(*p.PendingSince)}); err != nil {
			return raised, err
		}
		// 运营告警（只含标识、版本与时间，不含成员）；路由到值班尚未接入（C-24）
		log := s.logger().WarnContext
		if a.level == "failed" {
			log = s.logger().ErrorContext
		}
		log(ctx, "跟进授权同步告警", "alert", a.event, "tenant", p.TenantID, "pursuit", p.ID, "kind", p.Kind, "desired_version", p.Participation,
			"applied_version", p.Applied, "pending_since", ts(*p.PendingSince))
		raised++
	}
	return raised, nil
}

// recovered 在同步完成、待处理期间结束时（sync 已在同一把行锁下清空期间）发布 authorization_sync_recovered：只在该期间发布过告警时。
func (s *Service) recovered(ctx context.Context, tx pgx.Tx, p *Pursuit, pendingSince, delayed, failedAt *time.Time, applied int64) error {
	var alerted []string
	if delayed != nil {
		alerted = append(alerted, "delayed")
	}
	if failedAt != nil {
		alerted = append(alerted, "failed")
	}
	if len(alerted) == 0 || pendingSince == nil {
		return nil
	}
	now := s.businessNow()
	data := map[string]any{"pursuit_id": p.ID, "objective_id": p.ObjectiveID, "org_unit_id": p.OrgUnitID, "kind": p.Kind, "applied_version": applied,
		"pending_since": ts(*pendingSince), "alerted": alerted, "recovered_at": ts(now)}
	if _, err := writer.Append(ctx, tx, outbox.Event{Type: AuthorizationSyncRecovered, Subject: p.ID, PartitionKey: p.ID, Data: data}); err != nil {
		return err
	}
	if failedAt != nil {
		if err := timelineAt(ctx, tx, p.TenantID, p.ID, "authorization_sync_recovered", "svc:"+api.Name, "授权同步已恢复", now); err != nil {
			return err
		}
	}
	if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.authorization.sync_recovered", Resource: fmt.Sprintf("%s/participation:%d", p.ID, applied),
		Result: "succeeded", Basis: "sync_pending_since=" + ts(*pendingSince)}); err != nil {
		return err
	}
	s.logger().InfoContext(ctx, "跟进授权同步告警解除", "alert", AuthorizationSyncRecovered, "tenant", p.TenantID, "pursuit", p.ID, "applied_version", applied)
	return nil
}

func timelineAt(ctx context.Context, tx pgx.Tx, tenant, pursuit, kind, actor, detail string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO pur_timeline (tenant_id, pursuit_id, kind, actor, detail, recorded_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		tenant, pursuit, kind, actor, truncate(detail, 500), at)
	return err
}

// HandBack 是 Monitor.HandBack。
func (s *Service) HandBack(ctx context.Context, now time.Time, limit int) (int, error) {
	rc, err := serviceContext(ctx)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	now = now.UTC()
	var candidates []string
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM pur_reminders WHERE state = 'sent' AND escalated AND handback IS NULL
 ORDER BY handback_checked_at NULLS FIRST, id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		candidates, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, id := range candidates {
		var done bool
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			done, err = s.handBack(ctx, tx, rc.Tenant, rc.Cell, id, now)
			return err
		})
		if err != nil {
			// 判断不可用：不当作满足也不当作不满足，记下检查时间后留待下一轮（轮转，不挡住其他行）
			s.logger().WarnContext(ctx, "转交提醒暂未转回，稍后再试", "reminder", id, "error", err.Error())
			_ = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE pur_reminders SET handback_checked_at = $2 WHERE id = $1 AND handback IS NULL`, id, now)
				return err
			})
			continue
		}
		if done {
			sent++
		}
	}
	return sent, nil
}

// handBack 处理一条已发出的转交提醒；返回是否已转回。锁顺序与命令相同：跟进行 → 任务行 → 提醒行。
func (s *Service) handBack(ctx context.Context, tx pgx.Tx, tenant, cell, id string, now time.Time) (bool, error) {
	var pursuitID string
	err := tx.QueryRow(ctx, `SELECT pursuit_id FROM pur_reminders WHERE id = $1`, id).Scan(&pursuitID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p, err := loadPursuit(ctx, tx, pursuitID, true)
	if err != nil || p == nil {
		return false, err
	}
	var taskID, kind string
	var dv int64
	var fireAt time.Time
	var handback *string
	var escalated *bool
	err = tx.QueryRow(ctx, `SELECT task_id, kind, deadline_version, fire_at, handback, escalated FROM pur_reminders WHERE id = $1 AND state = 'sent' FOR UPDATE`, id).
		Scan(&taskID, &kind, &dv, &fireAt, &handback, &escalated)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if handback != nil || escalated == nil || !*escalated {
		return false, nil // 已由并发的执行者处理
	}
	t, err := loadTask(ctx, tx, taskID, true)
	if err != nil || t == nil {
		return false, err
	}
	mark := func(result string) error {
		_, err := tx.Exec(ctx, `UPDATE pur_reminders SET handback = $2, handback_at = $3, handback_checked_at = $3 WHERE id = $1 AND handback IS NULL`, id, result, now)
		return err
	}
	if p.State != "open" || t.Status != "todo" || t.DeadlineVersion != dv {
		return false, mark("skipped")
	}
	// 同一截止版本已有更晚的提醒发出：由那一条接替（它若也转交，则由它转回）
	var later int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pur_reminders WHERE task_id = $1 AND deadline_version = $2 AND state = 'sent' AND fire_at > $3`,
		taskID, dv, fireAt).Scan(&later); err != nil {
		return false, err
	}
	if later > 0 {
		return false, mark("skipped")
	}
	// 负责人以其本人实时判断（与提醒发出、getTask 的转交同一口径）；仍不满足的留待下一轮
	admitted := false
	if p.Applied >= p.Participation {
		if admitted, err = s.assigneeAdmitted(ctx, tenant, cell, p, t.Assignee); err != nil {
			return false, err
		}
	}
	if !admitted {
		_, err := tx.Exec(ctx, `UPDATE pur_reminders SET handback_checked_at = $2 WHERE id = $1`, id, now)
		return false, err
	}
	data := map[string]any{"reminder_id": id, "task_id": t.ID, "pursuit_id": p.ID, "objective_id": p.ObjectiveID, "org_unit_id": p.OrgUnitID, "kind": kind,
		"deadline_version": dv, "due": t.Due, "escalated": false, "recipient": t.Assignee, "handed_back": true, "fire_at": ts(fireAt)}
	if err := mark("sent"); err != nil {
		return false, err
	}
	if _, err := writer.Append(ctx, tx, outbox.Event{Type: TaskDue, Subject: t.ID, PartitionKey: p.ID, Data: data}); err != nil {
		return false, err
	}
	return true, recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.reminder.handed_back", Resource: fmt.Sprintf("%s/%s", t.ID, id), Result: "succeeded",
		Basis: fmt.Sprintf("deadline_version=%d", dv)})
}
