package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// 授权同步（PRD 第6段 F12）：pursuit 本地事务写入期望参与关系与参与版本，并在同一事务中发布 pursuit.team.changed.v1；命令提交后立即
// 同步一次，失败由 team.changed 的消费者按退避重试（Reminders.RetrySync 另可补做）。同步以本子系统的服务主体调用 identity
// SyncResourceParticipants，提交跟进当前的完整期望关系与当前参与版本（不是事件中的版本）：identity 只接受更高的版本，同一版本重放返回
// 同一结果。identity 拒绝的成员（同步前已退出或失去目标访问）不获得关系：被拒的负责人标为无有效负责人，被拒的协作者移出，参与版本加一
// 再同步。分派通知（pursuit.pursuit.assigned.v1）只在该参与版本同步完成后发布一次。

// syncAfterCommit 在命令提交后同步一次；失败只告警，由事件消费者重试。
func (s *Service) syncAfterCommit(ctx context.Context, tenant, cell, id string) {
	sctx := asService(ctx, tenant, cell)
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error { return s.sync(ctx, tx, id) })
	if err != nil {
		s.logger().WarnContext(ctx, "跟进授权同步失败，等待事件重试", "pursuit", id, "error", err.Error())
	}
}

// sync 按跟进当前的期望关系与参与版本同步（tx 为本子系统库中、以服务主体打开的事务）。
func (s *Service) sync(ctx context.Context, tx pgx.Tx, id string) error {
	p, err := loadPursuit(ctx, tx, id, false)
	if err != nil || p == nil {
		return err
	}
	if p.Applied >= p.Participation {
		return s.notify(ctx, tx, p)
	}
	if s.Directory == nil {
		return failure(503, "identity_unavailable")
	}
	owner, collaborators := p.desired()
	req := &identityapi.SyncParticipantsRequest{Resource: "pursuit:" + p.ID, OrgUnitID: identityapi.OrgUnitID(p.OrgUnitID), Version: p.Participation,
		Participants: []identityapi.ParticipantRelation{}}
	if owner != "" {
		req.Participants = append(req.Participants, identityapi.ParticipantRelation{Subject: owner, Relation: identityapi.ParticipantRelationRelationOwner})
	}
	for _, c := range collaborators {
		if c != owner {
			req.Participants = append(req.Participants, identityapi.ParticipantRelation{Subject: c, Relation: identityapi.ParticipantRelationRelationCollaborator})
		}
	}
	res, err := s.Directory.SyncResourceParticipants(ctx, req)
	if err != nil {
		return err
	}
	p, err = loadPursuit(ctx, tx, id, true)
	if err != nil {
		return err
	}
	applied := min(res.AppliedVersion, p.Participation)
	if applied <= p.Applied {
		return s.notify(ctx, tx, p)
	}
	if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET applied_version = $2, sync_pending_since = CASE WHEN $2 >= participation_version THEN NULL ELSE sync_pending_since END
 WHERE id = $1`, id, applied); err != nil {
		return err
	}
	p.Applied = applied
	if applied >= p.Participation {
		p.PendingSince = nil
	}
	if res.AppliedVersion == req.Version && p.Participation == req.Version && len(res.Rejected) > 0 && p.State != "voided" {
		if err := s.rejected(ctx, tx, p, res.Rejected); err != nil {
			return err
		}
		return nil
	}
	return s.notify(ctx, tx, p)
}

// rejected 处理 identity 拒绝的成员（F12 第6条）：负责人标为无有效负责人，协作者移出；参与版本加一并重新排队同步，通知操作者
// （时间线与审计；渠道通知随第5段的通知能力）。
func (s *Service) rejected(ctx context.Context, tx pgx.Tx, p *Pursuit, rejected []identityapi.SyncParticipantsResultRejectedItem) error {
	ownerInvalid := false
	collaborators := slices.Clone(p.Collaborators)
	var names []string
	for _, r := range rejected {
		names = append(names, r.Subject+":"+string(r.Reason))
		if r.Subject == p.Owner && r.Relation == identityapi.SyncParticipantsResultRejectedItemRelationOwner {
			ownerInvalid = true
		}
		collaborators = slices.DeleteFunc(collaborators, func(c string) bool { return c == r.Subject })
	}
	pv := p.Participation + 1
	if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET owner_valid = owner_valid AND NOT $2, collaborators = $3, participation_version = $4, version = version + 1,
 sync_pending_since = COALESCE(sync_pending_since, now()), notify_version = CASE WHEN $2 THEN NULL ELSE notify_version END,
 notify_assignee = CASE WHEN $2 THEN NULL ELSE notify_assignee END, notify_by = CASE WHEN $2 THEN NULL ELSE notify_by END,
 notify_kind = CASE WHEN $2 THEN NULL ELSE notify_kind END, updated_at = now() WHERE id = $1`, p.ID, ownerInvalid, collaborators, pv); err != nil {
		return err
	}
	reason := "team_changed"
	if ownerInvalid {
		reason = "owner_invalid"
	}
	if err := teamChanged(ctx, tx, p.ID, pv, reason); err != nil {
		return err
	}
	rc, _ := reqctx.From(ctx)
	detail := fmt.Sprintf("identity 拒绝：%v", names)
	if err := timelineNow(ctx, tx, rc.Tenant, p.ID, "authorization_rejected", rc.Principal.ID, detail); err != nil {
		return err
	}
	return recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.authorization.rejected", Resource: fmt.Sprintf("%s/participation:%d", p.ID, p.Participation),
		Result: "failed", Basis: "sync_resource_participants"})
}

func timelineNow(ctx context.Context, tx pgx.Tx, tenant, pursuit, kind, actor, detail string) error {
	_, err := tx.Exec(ctx, `INSERT INTO pur_timeline (tenant_id, pursuit_id, kind, actor, detail, recorded_at) VALUES ($1,$2,$3,$4,$5,now())`,
		tenant, pursuit, kind, actor, truncate(detail, 500))
	return err
}

// notify 在分派对应的参与版本同步完成后发布一次分派通知；被分派人已不是当前有效负责人（之后又改派或失效）时不再通知。
func (s *Service) notify(ctx context.Context, tx pgx.Tx, p *Pursuit) error {
	if p.NotifyVersion == nil || p.Applied < *p.NotifyVersion {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE pur_pursuits SET notify_version = NULL, notify_assignee = NULL, notify_by = NULL, notify_kind = NULL
 WHERE id = $1 AND notify_version = $2`, p.ID, *p.NotifyVersion)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if p.State != "open" || !p.OwnerValid || p.Owner != p.NotifyAssignee {
		return nil
	}
	_, err = writer.Append(ctx, tx, outbox.Event{Type: PursuitAssigned, Subject: p.ID, Data: map[string]any{"pursuit_id": p.ID, "objective_id": p.ObjectiveID,
		"assignee": p.NotifyAssignee, "assigned_by": p.NotifyBy, "participation_version": *p.NotifyVersion, "kind": p.NotifyKind, "applied_at": ts(s.businessNow())}})
	return err
}

// TeamChangedEvent 是 pursuit.team.changed.v1 的消费处理：按跟进当前的期望关系与版本同步。返回错误时事件按退避重试。
func (s *Service) TeamChangedEvent(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data struct {
		PursuitID string `json:"pursuit_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	return s.sync(ctx, tx, data.PursuitID)
}

// RetrySync 是 Reminders.RetrySync。
func (s *Service) RetrySync(ctx context.Context, limit int) (int, error) {
	if _, err := serviceContext(ctx); err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var pending []string
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM pur_pursuits WHERE applied_version < participation_version ORDER BY sync_pending_since NULLS LAST, id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		pending, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	done := 0
	for _, id := range pending {
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error { return s.sync(ctx, tx, id) })
		if err != nil {
			s.logger().WarnContext(ctx, "跟进授权同步重试失败", "pursuit", id, "error", err.Error())
			continue
		}
		done++
	}
	return done, nil
}
