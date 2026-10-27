package app

import (
	"context"
	"errors"

	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 创建面板的缺省值（PRD 第6段 F1 第2条、用例4；C-10）：默认负责人优先取目标分派规则给出的候选负责人，候选须此刻以其本人仍是租户
// 有效成员且能查看该目标的信号；没有候选、候选无效或分派规则未接入时默认负责人为创建者本人，并说明原因。候选只是建议：
// 不因被建议而获得访问，创建时仍按 createPursuit 的分派检查判断；候选无效时不返回其标识（不泄露规则配置给无关成员以外的信息）。

// CreateDefaults 是契约 CreateDefaults。
type CreateDefaults struct {
	ObjectiveID    string `json:"objective_id"`
	SignalID       string `json:"signal_id,omitempty"`
	Owner          string `json:"owner"`
	OwnerSource    string `json:"owner_source"`
	CandidateState string `json:"candidate_state"`
	Candidate      string `json:"candidate,omitempty"`
}

// 候选状态。
const (
	CandidateValid       = "valid"
	CandidateNone        = "none"
	CandidateNoAccess    = "no_access"
	CandidateUnavailable = "rules_unavailable"
)

// GetCreateDefaults 返回创建面板的默认负责人（契约 getCreateDefaults）。调用者须在目标上有 can_assign（不可见 404，无权 403）；
// 给出信号时信号须对调用者可见且属于该目标（否则 404 signal_not_found）。分派规则未接入为 rules_unavailable（不是“没有候选”）；
// 读取候选或判断候选失败为 503（失败关闭，不退回本人以免掩盖故障）。
func (s *Service) GetCreateDefaults(ctx context.Context, objective, signal string) (*CreateDefaults, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	scope, err := s.scopeOf(ctx, objective, relAssign)
	if err != nil {
		return nil, err
	}
	if !scope.Has(relAssign) {
		return nil, failure(403, "forbidden")
	}
	if signal != "" {
		if err := s.visibleSignal(ctx, objective, signal); err != nil {
			return nil, err
		}
	}
	out := &CreateDefaults{ObjectiveID: objective, SignalID: signal, Owner: rc.Principal.ID, OwnerSource: "self"}
	if signal == "" {
		// 候选负责人随信号给出：不带信号时没有候选
		out.CandidateState = CandidateNone
		return out, nil
	}
	if s.Candidates == nil {
		out.CandidateState = CandidateUnavailable
		return out, nil
	}
	candidate, err := s.Candidates.CandidateOwner(ctx, objective, signal)
	switch {
	case errors.Is(err, api.ErrNotWired):
		out.CandidateState = CandidateUnavailable
		return out, nil
	case err != nil:
		s.logger().WarnContext(ctx, "pursuit: 读取候选负责人失败", "objective", objective, "err", err)
		return nil, failure(503, "assignment_rules_unavailable")
	case candidate == "":
		out.CandidateState = CandidateNone
		return out, nil
	}
	ok, _, err := s.memberAccess(ctx, rc.Tenant, rc.Cell, candidate, objective)
	if err != nil {
		return nil, err
	}
	if !ok {
		out.CandidateState = CandidateNoAccess
		return out, nil
	}
	out.Owner, out.OwnerSource, out.CandidateState, out.Candidate = candidate, "candidate", CandidateValid, candidate
	return out, nil
}

// visibleSignal 以调用者的权限确认信号可见且属于该目标（不检查内容版本与状态：缺省值只用于展示）。
func (s *Service) visibleSignal(ctx context.Context, objective, id string) error {
	if s.Signals == nil {
		return failure(503, "detection_unavailable")
	}
	list, err := s.Signals.SignalCards(ctx, []string{id})
	if err != nil {
		var safe *detection.Error
		if errors.As(err, &safe) && safe.Status == 404 {
			return failure(404, "signal_not_found")
		}
		if errors.As(err, &safe) && safe.Status < 500 {
			return failure(safe.Status, safe.Code)
		}
		return failure(503, "detection_unavailable")
	}
	if len(list.Cards) == 0 || list.Cards[0].Signal.ID != id || list.Cards[0].Signal.ObjectiveID != objective {
		return failure(404, "signal_not_found")
	}
	return nil
}
