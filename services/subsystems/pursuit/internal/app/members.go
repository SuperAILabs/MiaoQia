package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
)

// 成员生命周期（PRD 第6段 F4 第5条）：负责人停用、退出租户或失去目标访问后，跟进标“无有效负责人”，提醒改发给目标所在单元的决策者；
// 协作者同样情况下移出团队。事件只是触发：每个受影响成员以其本人此刻的成员状态与目标访问重新判断（identity 会话核验与 customer-context
// 的 can_view_signals），判断失败时返回错误、事件按退避重试（不当作无权限也不当作有权限）。重新加入租户或恢复访问不自动恢复负责人
// 或协作者身份（须由有 can_manage_team 的成员改派）。参与版本加一，授权同步撤回 identity 中的关系。
//
// 成员事件可能迟到或重放：退出（state 不是 active）事件的成员版本不高于授予记录中该成员当前角色授予时的成员版本时，是授予之前
// 的旧事件，不据此判无效，而是以其本人此刻的成员状态与目标访问重新判断；高于时（授予之后退出过）判无效，此后重新加入也不恢复。

// MemberChanged 是 identity.member.changed.v1 的消费处理（平台域主题，信封带租户）。
func (s *Service) MemberChanged(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data struct {
		TenantID string `json:"tenant_id"`
		Subject  string `json:"subject"`
		State    string `json:"state"`
		Version  int64  `json:"version"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	if env.Tenant == "" || env.Cell == "" || env.Tenant != data.TenantID {
		return fmt.Errorf("pursuit：成员事件 %s 的租户上下文缺失或不一致", env.ID)
	}
	var left func(*Pursuit, string) bool
	if data.State != "active" {
		left = func(p *Pursuit, subject string) bool {
			g, ok := p.Grants[subject]
			return !ok || data.Version > g.Member
		}
	}
	return s.revalidate(ctx, tx, env.Tenant, env.Cell, `state = 'open' AND ((owner = $1 AND owner_valid) OR $1 = ANY(collaborators))`, data.Subject,
		func(subject string) bool { return subject == data.Subject }, left)
}

// ObjectiveChanged 是 customer_context.objective.changed.v1 的消费处理：目标的负责人、参与人或单元变化后，重新判断该目标上进行中的
// 跟进的负责人与协作者是否仍能查看目标的信号。
func (s *Service) ObjectiveChanged(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data struct {
		ObjectiveID string `json:"objective_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	if env.Tenant == "" || env.Cell == "" {
		return fmt.Errorf("pursuit：目标事件 %s 缺少租户上下文", env.ID)
	}
	return s.revalidate(ctx, tx, env.Tenant, env.Cell, `state = 'open' AND objective_id = $1`, data.ObjectiveID, nil, nil)
}

// revalidate 对匹配 where 的进行中跟进，重新判断其有效负责人与协作者（only 为空时判断全部成员；left 对某跟进上 only 选中的成员
// 为真时——授予之后退出过——直接视为无效，否则以其本人此刻的成员状态与目标访问判断）。
func (s *Service) revalidate(ctx context.Context, tx pgx.Tx, tenant, cell, where string, arg any, only func(string) bool, left func(*Pursuit, string) bool) error {
	rows, err := tx.Query(ctx, `SELECT `+pursuitColumns+` FROM pur_pursuits WHERE `+where+` ORDER BY id FOR UPDATE`, arg)
	if err != nil {
		return err
	}
	var list []*Pursuit
	for rows.Next() {
		p, err := scanPursuit(rows)
		if err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	verdicts := map[string]bool{} // 成员 → 仍有效（同一事件内每个成员在每个目标上只判断一次）
	valid := func(p *Pursuit, subject, objective string) (bool, error) {
		if only != nil && !only(subject) {
			return true, nil
		}
		if left != nil && left(p, subject) {
			return false, nil
		}
		k := subject + "/" + objective
		if v, ok := verdicts[k]; ok {
			return v, nil
		}
		v, _, err := s.memberAccess(ctx, tenant, cell, subject, objective)
		if err != nil {
			return false, err
		}
		verdicts[k] = v
		return v, nil
	}
	for _, p := range list {
		ownerInvalid := false
		if p.OwnerValid {
			ok, err := valid(p, p.Owner, p.ObjectiveID)
			if err != nil {
				return err
			}
			ownerInvalid = !ok
		}
		var removed []string
		for _, c := range p.Collaborators {
			ok, err := valid(p, c, p.ObjectiveID)
			if err != nil {
				return err
			}
			if !ok {
				removed = append(removed, c)
			}
		}
		if !ownerInvalid && len(removed) == 0 {
			continue
		}
		collaborators := slices.DeleteFunc(slices.Clone(p.Collaborators), func(c string) bool { return slices.Contains(removed, c) })
		pv := p.Participation + 1
		if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET owner_valid = owner_valid AND NOT $2, collaborators = $3, participation_version = $4, version = version + 1,
 sync_pending_since = COALESCE(sync_pending_since, now()), changed_by = $5, team_changed_by = $5, updated_at = now() WHERE id = $1`, p.ID, ownerInvalid, collaborators, pv, "svc:pursuit"); err != nil {
			return err
		}
		reason := "team_changed"
		detail := fmt.Sprintf("协作者移出：%v", removed)
		if ownerInvalid {
			reason, detail = "owner_invalid", fmt.Sprintf("负责人 %s 已无效；%s", p.Owner, detail)
		}
		if err := teamChanged(ctx, tx, p.ID, pv, reason); err != nil {
			return err
		}
		kind := "team_changed"
		if ownerInvalid {
			kind = "owner_invalid"
		}
		if err := timelineNow(ctx, tx, tenant, p.ID, kind, "svc:pursuit", detail); err != nil {
			return err
		}
		if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.team." + kind, Resource: fmt.Sprintf("%s/participation:%d", p.ID, pv),
			Result: "succeeded", Basis: "member_lifecycle"}); err != nil {
			return err
		}
	}
	return nil
}
