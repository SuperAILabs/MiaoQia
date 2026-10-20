package app

import (
	"context"
	"encoding/json"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
)

// 策略接口的契约转换：本子系统的记录与契约字段同名（JSON），经 JSON 转换为生成类型；转换失败（记录不符合契约）不外泄细节。
func convert[T any](in any) (*T, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func formIn(in cc.PolicyForm) (PolicyForm, error) {
	f, err := convert[PolicyForm](&in)
	if err != nil {
		return PolicyForm{}, failure(400, "invalid_request")
	}
	return *f, nil
}

func policyOut(p *PolicyState, err error) (*cc.Policy, error) {
	if err != nil {
		return nil, err
	}
	return convert[cc.Policy](p)
}

func requestOut(r *RequestRec, err error) (*cc.PolicyRequest, error) {
	if err != nil {
		return nil, err
	}
	return convert[cc.PolicyRequest](r)
}

func (s *Server) GetPolicy(ctx context.Context, params cc.GetPolicyParams) (*cc.Policy, error) {
	return policyOut(s.Service.GetPolicy(ctx, params.ObjectiveID))
}

func (s *Server) DraftPolicyChange(ctx context.Context, req *cc.DraftPolicyRequest, params cc.DraftPolicyChangeParams) (*cc.Policy, error) {
	form, err := formIn(req.Form)
	if err != nil {
		return nil, err
	}
	return policyOut(s.Service.DraftPolicyChange(ctx, params.ObjectiveID, req.ExpectedVersion, form, params.IdempotencyKey))
}

func (s *Server) DiscardPolicyDraft(ctx context.Context, req *cc.DiscardPolicyDraftRequest, params cc.DiscardPolicyDraftParams) (*cc.Policy, error) {
	return policyOut(s.Service.DiscardPolicyDraft(ctx, params.ObjectiveID, req.ExpectedVersion, params.IdempotencyKey))
}

func (s *Server) ListPolicyVersions(ctx context.Context, params cc.ListPolicyVersionsParams) (*cc.PolicyVersionList, error) {
	page, err := s.Service.ListPolicyVersions(ctx, params.ObjectiveID, params.Limit.Or(0), string(params.Cursor.Or("")))
	if err != nil {
		return nil, err
	}
	out := struct {
		Versions   []VersionRec `json:"versions"`
		Truncated  bool         `json:"truncated"`
		NextCursor string       `json:"next_cursor,omitempty"`
	}{page.Versions, page.Truncated, page.NextCursor}
	return convert[cc.PolicyVersionList](out)
}

func (s *Server) GetPolicyVersion(ctx context.Context, params cc.GetPolicyVersionParams) (*cc.PolicyVersion, error) {
	v, err := s.Service.GetPolicyVersion(ctx, params.ObjectiveID, params.Version)
	if err != nil {
		return nil, err
	}
	return convert[cc.PolicyVersion](v)
}

func (s *Server) DiffPolicyVersions(ctx context.Context, params cc.DiffPolicyVersionsParams) (*cc.PolicyDiff, error) {
	d, err := s.Service.DiffPolicyVersions(ctx, params.ObjectiveID, params.From, params.To)
	if err != nil {
		return nil, err
	}
	return convert[cc.PolicyDiff](d)
}

func (s *Server) RequestPolicyActivation(ctx context.Context, req *cc.RequestPolicyActivationRequest, params cc.RequestPolicyActivationParams) (*cc.PolicyRequest, error) {
	in := ActivationInput{Expected: req.ExpectedVersion, Revision: req.Revision, SimulationID: req.SimulationID, Note: req.Note}
	if d, ok := req.BackfillDays.Get(); ok {
		in.BackfillDays = &d
	}
	return requestOut(s.Service.RequestPolicyActivation(ctx, params.ObjectiveID, in, params.IdempotencyKey))
}

func (s *Server) RequestPolicyRollback(ctx context.Context, req *cc.RequestPolicyRollbackRequest, params cc.RequestPolicyRollbackParams) (*cc.PolicyRequest, error) {
	return requestOut(s.Service.RequestPolicyRollback(ctx, params.ObjectiveID, req.TargetVersion, req.Note, params.IdempotencyKey))
}

func (s *Server) GetPolicySimulationInput(ctx context.Context, params cc.GetPolicySimulationInputParams) (*cc.PolicySimulationInput, error) {
	return s.Service.PolicySimulationInput(ctx, params.ObjectiveID, params.Revision)
}

// PolicySimulationInput 把仿真输入转换为契约类型（本地绑定与内部接口共用）。
func (s *Service) PolicySimulationInput(ctx context.Context, id string, revision int64) (*cc.PolicySimulationInput, error) {
	in, err := s.SimulationInputFor(ctx, id, revision)
	if err != nil {
		return nil, err
	}
	return convert[cc.PolicySimulationInput](in)
}

// PolicyReader 实现 api.PolicyReader（本地绑定）。
type PolicyReader struct{ Service *Service }

func (r PolicyReader) SimulationInput(ctx context.Context, objective string, revision int64) (*cc.PolicySimulationInput, error) {
	return r.Service.PolicySimulationInput(ctx, objective, revision)
}

func (s *Server) GetObjectiveAccess(ctx context.Context, params cc.GetObjectiveAccessParams) (*cc.ObjectiveAccess, error) {
	relations := make([]string, 0, len(params.Relation))
	for _, r := range params.Relation {
		relations = append(relations, string(r))
	}
	allowed, err := s.Service.ObjectiveAccess(ctx, params.ObjectiveID, relations)
	if err != nil {
		return nil, err
	}
	out := &cc.ObjectiveAccess{ObjectiveID: params.ObjectiveID, Allowed: []cc.ObjectiveRelation{}}
	for _, r := range allowed {
		out.Allowed = append(out.Allowed, cc.ObjectiveRelation(r))
	}
	return out, nil
}

// ObjectiveAccessor 实现 api.ObjectiveAccess（本地绑定）。
type ObjectiveAccessor struct{ Service *Service }

func (a ObjectiveAccessor) Access(ctx context.Context, objective string, relations []string) ([]string, error) {
	return a.Service.ObjectiveAccess(ctx, objective, relations)
}
