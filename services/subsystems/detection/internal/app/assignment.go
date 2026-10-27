package app

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 候选负责人（PRD 第6段 F1 第2条；customer-context.md 第11节；C-14）：生成信号时按目标的分派规则算出，与所用的分派配置版本一同写入
// 信号，只是建议，不授予访问。
//
//   - 规则来自批次开始时以服务主体读取的生效策略（customer-context 0.7.0 的 ActivePolicy.assignment），与负责人、参与人是同一次读取；
//   - 按顺序取第一条命中的规则：regions 按 region.within、categories 按 category.any_match（kernel/policy 的同一口径）匹配信号的
//     地点与品类，两者都给出时须都满足，都不给出时匹配全部信号；被分派人已不是该读取中的负责人或参与人的规则跳过；
//   - 提供方没有给出分派配置（旧版本）时不计算，信号记为“分派规则不可用”（assignment_version 为空），不当作“没有候选”；
//   - 读取（getSignalCandidateOwner）只返回生成时记录的值，不重新求值规则；使用方（pursuit 的创建面板）以候选本人再检查。

const relAssign = "can_assign"

// candidateOwner 返回信号的候选负责人与所用的分派配置版本；ok 为假表示分派规则不可用。
func candidateOwner(p cc.ActivePolicy, location string, categories []string) (owner string, version int64, ok bool) {
	a, set := p.Assignment.Get()
	if !set {
		return "", 0, false
	}
	involved := func(subject string) bool {
		if string(p.Owner.Or("")) == subject {
			return true
		}
		for _, x := range p.Participants {
			if string(x) == subject {
				return true
			}
		}
		return false
	}
	for _, r := range a.Rules {
		if !involved(string(r.Assignee)) {
			continue
		}
		if len(r.Regions) > 0 && (location == "" || !policy.RegionWithin(location, r.Regions)) {
			continue
		}
		if len(r.Categories) > 0 && !slices.ContainsFunc(categories, func(c string) bool { return policy.CategoryCovered(c, r.Categories) }) {
			continue
		}
		return string(r.Assignee), a.Version, true
	}
	return "", a.Version, true
}

// SignalCandidate 是契约 SignalCandidate。
type SignalCandidate struct {
	SignalID          string `json:"signal_id"`
	ObjectiveID       string `json:"objective_id"`
	State             string `json:"state"`
	CandidateOwner    string `json:"candidate_owner,omitempty"`
	AssignmentVersion *int64 `json:"assignment_version,omitempty"`
}

// 候选状态。
const (
	CandidateAssigned    = "assigned"
	CandidateNone        = "none"
	CandidateUnavailable = "rules_unavailable"
)

// candidateFields 是候选读取的字段级白名单（按调用者在目标上的级别）：只有能分派跟进（can_assign）的成员看得到候选人标识；
// 只能查看信号的成员不在名单中（403），不能查看的为 404。
var candidateFields = map[string][]string{
	relAssign: {"signal_id", "objective_id", "state", "candidate_owner", "assignment_version"},
}

// GetSignalCandidate 读取信号的候选负责人（契约 getSignalCandidateOwner，只经内部端口）。
func (s *Service) GetSignalCandidate(ctx context.Context, id string) (*SignalCandidate, error) {
	if _, err := userContext(ctx); err != nil {
		return nil, err
	}
	var objective string
	var owner *string
	var version *int64
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT objective_id, candidate_owner, assignment_version FROM det_signals WHERE id = $1`, id).Scan(&objective, &owner, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "signal_not_found")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	visible, granted, err := s.access(ctx, objective, relViewSignals, relAssign)
	if err != nil {
		return nil, err
	}
	if !visible || !has(granted, relViewSignals) {
		return nil, failure(404, "signal_not_found")
	}
	level := ""
	if has(granted, relAssign) {
		level = relAssign
	}
	fields, ok := candidateFields[level]
	if !ok {
		return nil, failure(403, "forbidden")
	}
	full := SignalCandidate{SignalID: id, ObjectiveID: objective, State: CandidateUnavailable, AssignmentVersion: version}
	switch {
	case version != nil && owner != nil:
		full.State, full.CandidateOwner = CandidateAssigned, *owner
	case version != nil:
		full.State = CandidateNone
	}
	out := &SignalCandidate{}
	for _, f := range fields {
		switch f {
		case "signal_id":
			out.SignalID = full.SignalID
		case "objective_id":
			out.ObjectiveID = full.ObjectiveID
		case "state":
			out.State = full.State
		case "candidate_owner":
			out.CandidateOwner = full.CandidateOwner
		case "assignment_version":
			out.AssignmentVersion = full.AssignmentVersion
		}
	}
	return out, nil
}

// Candidates 是 api.SignalCandidates 的本地实现（pursuit 的创建面板读取候选负责人；授权与 getSignalCandidateOwner 相同）。
type Candidates struct{ Service *Service }

func (c Candidates) SignalCandidate(ctx context.Context, signalID string) (*api.SignalCandidate, error) {
	out, err := c.Service.GetSignalCandidate(ctx, signalID)
	if err != nil {
		return nil, err
	}
	return convert[api.SignalCandidate](out)
}
