package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
)

// 目标的参与关系由 identity 保存在授权模型中（identity.md 第5节 SyncResourceParticipants）。本子系统在目标的每个版本上
// 以服务主体同步“所在单元 + 负责人 + 参与人”的完整期望集合，参与版本即目标版本：identity 只接受更高的版本，
// 同一版本重放返回同一结果，迟到的旧版本不会覆盖新版本。
//
// 写入路径：命令提交后立即同步一次（通常即时生效）；失败时由 objective.changed 事件的消费者重试（经发件箱，
// 按目标当前版本同步）。同步完成前 identity 尚不知道目标所在单元，目标的查看与操作一律拒绝（失败关闭）。

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) syncObjective(ctx context.Context, o *Objective) error {
	if s.Directory == nil {
		return failure(503, "identity_unavailable")
	}
	req := &identityapi.SyncParticipantsRequest{Resource: "objective:" + o.ID, OrgUnitID: identityapi.OrgUnitID(o.OrgUnitID), Version: o.Version,
		Participants: []identityapi.ParticipantRelation{{Subject: o.Owner, Relation: identityapi.ParticipantRelationRelationOwner}}}
	for _, p := range o.Participants {
		req.Participants = append(req.Participants, identityapi.ParticipantRelation{Subject: p, Relation: identityapi.ParticipantRelationRelationParticipant})
	}
	res, err := s.Directory.SyncResourceParticipants(reqctx.Service(ctx, api.Name), req)
	if err != nil {
		return err
	}
	for _, r := range res.Rejected {
		// 被拒成员（已退出、无范围访问）不获得参与关系，其余照常生效；界面提示随第2段界面实现。
		s.logger().InfoContext(ctx, "目标参与关系被 identity 拒绝", "subsystem", api.Name, "objective", o.ID, "version", o.Version,
			"subject", r.Subject, "relation", string(r.Relation), "reason", string(r.Reason))
	}
	return nil
}

// syncAfterCommit 在命令提交后同步一次；失败只告警，由事件消费者重试。
func (s *Service) syncAfterCommit(ctx context.Context, o *Objective) {
	if err := s.syncObjective(ctx, o); err != nil {
		s.logger().WarnContext(ctx, "目标参与关系同步失败，等待事件重试", "subsystem", api.Name, "objective", o.ID, "version", o.Version, "error", err.Error())
	}
}

// SyncObjectiveParticipants 是 customer_context.objective.changed.v1 的消费处理：按目标当前版本同步参与关系。
// 返回错误时事件按退避重试；目标不存在（不应发生）时确认事件。
func (s *Service) SyncObjectiveParticipants(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data struct {
		ObjectiveID string `json:"objective_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	o, err := scanObjective(tx.QueryRow(ctx, `SELECT `+objectiveColumns+` FROM ctx_objectives WHERE id = $1`, data.ObjectiveID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.syncObjective(ctx, o)
}
