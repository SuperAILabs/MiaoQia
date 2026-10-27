package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// Server 实现由 OpenAPI 生成的服务端接口（contracts/apis/customer-context/openapi.yaml）。
type Server struct {
	Service *Service
}

var _ cc.Handler = (*Server)(nil)

// WriteProtocolError 使生成服务端的解码与校验错误遵循同一 Problem 契约。
func WriteProtocolError(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&cc.Problem{Type: "https://errors.miaoqia.com/invalid_request", Title: "invalid_request", Status: status,
		Code: cc.NewOptString("invalid_request")})
}

// NewError 把错误转换为 RFC 9457 Problem Details；未识别的错误不外泄细节。
func (s *Server) NewError(_ context.Context, err error) *cc.ProblemStatusCode {
	status, code, detail := http.StatusInternalServerError, "internal", ""
	var safe *api.Error
	if errors.As(err, &safe) {
		status, code, detail = safe.Status, safe.Code, safe.Detail
	}
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	p := cc.Problem{Type: "https://errors.miaoqia.com/" + code, Title: code, Status: status, Code: cc.NewOptString(code)}
	if detail != "" {
		p.Detail = cc.NewOptString(detail)
	}
	return &cc.ProblemStatusCode{StatusCode: status, Response: p}
}

func (s *Server) GetProfile(ctx context.Context) (*cc.Profile, error) {
	p, err := s.Service.GetProfile(ctx)
	if err != nil {
		return nil, err
	}
	return profileOut(p), nil
}

func (s *Server) UpdateProfile(ctx context.Context, req *cc.UpdateProfileRequest, params cc.UpdateProfileParams) (*cc.Profile, error) {
	p, err := s.Service.UpdateProfile(ctx, req.ExpectedVersion, profileIn(req.Content), params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return profileOut(p), nil
}

func (s *Server) GetContextVersion(ctx context.Context) (*cc.ContextVersion, error) {
	return s.Service.ContextVersion(ctx)
}

func (s *Server) GetContextSnapshot(ctx context.Context, params cc.GetContextSnapshotParams) (*cc.ContextSnapshot, error) {
	return s.Service.Snapshot(ctx, params.OrgUnitID, params.ContextVersion.Or(0))
}

func (s *Server) ListObjectiveTemplates(ctx context.Context) (*cc.ObjectiveTemplateList, error) {
	roles, views, err := s.Service.Templates(ctx)
	if err != nil {
		return nil, err
	}
	out := &cc.ObjectiveTemplateList{ValueChainRoles: append([]string{}, roles...), Templates: []cc.ObjectiveTemplate{}}
	for _, v := range views {
		t := cc.ObjectiveTemplate{TemplateID: v.Objective.ID, Version: v.Objective.Version, Name: v.Objective.Name, Say: v.Objective.Say,
			ReleaseStage: v.Objective.Stage, Enableable: v.Enableable, DefaultForRoles: v.DefaultForRoles,
			RequiresDomains: append([]string{}, v.Objective.RequiresDomains...), Params: []cc.ObjectiveTemplateParam{}}
		if v.UnavailableReason != "" {
			t.UnavailableReason = cc.NewOptObjectiveTemplateUnavailableReason(cc.ObjectiveTemplateUnavailableReason(v.UnavailableReason))
		}
		if v.Policy != nil {
			t.PolicyTemplateID, t.PolicyTemplateVersion = cc.NewOptString(v.Policy.ID), cc.NewOptString(v.Policy.Version)
			for _, d := range v.Policy.Params {
				p := cc.ObjectiveTemplateParam{Name: d.Name, Type: d.Type, Configurable: d.Configurable(), DefaultValues: d.DefaultValues}
				if d.DefaultFrom != "" {
					p.DefaultFrom = cc.NewOptString(d.DefaultFrom)
				}
				if d.DefaultNumber != nil {
					p.DefaultNumber = cc.NewOptInt64(*d.DefaultNumber)
				}
				t.Params = append(t.Params, p)
			}
		}
		out.Templates = append(out.Templates, t)
	}
	return out, nil
}

func (s *Server) ListObjectives(ctx context.Context) (*cc.ObjectiveList, error) {
	list, err := s.Service.ListObjectives(ctx)
	if err != nil {
		return nil, err
	}
	out := &cc.ObjectiveList{Objectives: []cc.Objective{}}
	for i := range list {
		out.Objectives = append(out.Objectives, *objectiveOut(&list[i]))
	}
	return out, nil
}

func (s *Server) EnableObjective(ctx context.Context, req *cc.EnableObjectiveRequest, params cc.EnableObjectiveParams) (*cc.Objective, error) {
	in := ObjectiveInput{TemplateID: req.TemplateID, OrgUnitID: req.OrgUnitID, Owner: string(req.Owner.Or("")), Participants: subjects(req.Participants)}
	if p, ok := req.Params.Get(); ok {
		in.Params = paramsIn(p)
	}
	o, err := s.Service.EnableObjective(ctx, in, params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return objectiveOut(o), nil
}

func (s *Server) GetObjective(ctx context.Context, params cc.GetObjectiveParams) (*cc.Objective, error) {
	o, err := s.Service.GetObjective(ctx, params.ObjectiveID)
	if err != nil {
		return nil, err
	}
	return objectiveOut(o), nil
}

func (s *Server) ConfigureObjective(ctx context.Context, req *cc.ConfigureObjectiveRequest, params cc.ConfigureObjectiveParams) (*cc.Objective, error) {
	in := ObjectiveInput{Owner: string(req.Owner), Participants: subjects(req.Participants), Params: paramsIn(req.Params)}
	o, err := s.Service.ConfigureObjective(ctx, params.ObjectiveID, req.ExpectedVersion, in, params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return objectiveOut(o), nil
}

func (s *Server) SetObjectiveState(ctx context.Context, req *cc.SetObjectiveStateRequest, params cc.SetObjectiveStateParams) (*cc.Objective, error) {
	o, err := s.Service.SetObjectiveState(ctx, params.ObjectiveID, req.ExpectedVersion, string(req.State), params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return objectiveOut(o), nil
}

// Snapshot 把快照转换为契约类型（本地绑定与内部接口共用）。
func (s *Service) Snapshot(ctx context.Context, unit string, version int64) (*api.Snapshot, error) {
	r, err := s.ContextSnapshot(ctx, unit, version)
	if err != nil {
		return nil, err
	}
	return SnapshotOut(r), nil
}

// SnapshotOut 把快照转换为契约类型：未知的 org_ids 与 entities 不出现（不用空列表）。
func SnapshotOut(r *SnapshotResult) *api.Snapshot {
	v := cc.ContextPolicyView{OrgIds: r.View.OrgIDs, OfferingCategories: r.View.Categories, ServiceRegions: r.View.Regions, Brands: r.View.Brands}
	if r.View.Entities != nil {
		entities := cc.ContextPolicyViewEntities{}
		for org, e := range r.View.Entities {
			q := cc.EntityQualificationsQualifications{}
			for code, status := range e.Qualifications {
				q[code] = cc.EntityQualificationsQualificationsItem(status)
			}
			entities[org] = cc.EntityQualifications{Qualifications: q, QualificationsPending: e.Pending, QualificationsComplete: e.Complete}
		}
		v.Entities = cc.NewOptContextPolicyViewEntities(entities)
	}
	return &api.Snapshot{TenantID: r.Tenant, OrgUnitID: r.Unit, ContextVersion: r.Version, UnitChain: r.Chain, PolicyView: v}
}

// Reader 实现 api.ContextReader（本地绑定）。
type Reader struct{ Service *Service }

func (r Reader) ContextVersion(ctx context.Context) (*api.Version, error) {
	return r.Service.ContextVersion(ctx)
}
func (r Reader) ContextSnapshot(ctx context.Context, unit string, version int64) (*api.Snapshot, error) {
	return r.Service.Snapshot(ctx, unit, version)
}

func profileIn(c cc.ProfileContent) domain.Profile {
	p := domain.Profile{SubjectType: string(c.SubjectType), Entities: []domain.Entity{}, Roles: nonNil(c.ValueChainRoles), Industries: nonNil(c.IndustryPacks),
		Categories: nonNil(c.OfferingCategories), Brands: nonNil(c.Brands), Regions: nonNil(c.ServiceRegions), Qualifications: []domain.Qualification{}}
	for _, e := range c.Entities {
		p.Entities = append(p.Entities, domain.Entity{OrgID: e.OrgID, OrgUnitID: e.OrgUnitID.Or(""), QualificationsComplete: e.QualificationsComplete})
	}
	for _, q := range c.Qualifications {
		p.Qualifications = append(p.Qualifications, domain.Qualification{OrgID: q.OrgID, Code: q.Code, Grade: q.Grade.Or(""), CertificateNo: q.CertificateNo,
			ValidFrom: date(q.ValidFrom), ValidTo: date(q.ValidTo)})
	}
	return p
}

func date(d cc.OptDate) domain.Date {
	if v, ok := d.Get(); ok {
		return domain.Date(v.Format("2006-01-02"))
	}
	return ""
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func profileOut(r *ProfileResult) *cc.Profile {
	c := r.Content
	out := cc.Profile{Version: r.Version, ContextVersion: r.ContextVersion, Content: cc.ProfileContent{SubjectType: cc.ProfileContentSubjectType(c.SubjectType),
		Entities: []cc.ProfileEntity{}, ValueChainRoles: nonNil(c.Roles), IndustryPacks: nonNil(c.Industries), OfferingCategories: nonNil(c.Categories),
		Brands: nonNil(c.Brands), ServiceRegions: nonNil(c.Regions), Qualifications: []cc.Qualification{}}}
	for _, e := range c.Entities {
		pe := cc.ProfileEntity{OrgID: e.OrgID, QualificationsComplete: e.QualificationsComplete}
		if e.OrgUnitID != "" {
			pe.OrgUnitID = cc.NewOptString(e.OrgUnitID)
		}
		out.Content.Entities = append(out.Content.Entities, pe)
	}
	for _, q := range c.Qualifications {
		pq := cc.Qualification{OrgID: q.OrgID, Code: q.Code, CertificateNo: q.CertificateNo}
		if q.Grade != "" {
			pq.Grade = cc.NewOptString(q.Grade)
		}
		pq.ValidFrom, pq.ValidTo = optDate(q.ValidFrom), optDate(q.ValidTo)
		out.Content.Qualifications = append(out.Content.Qualifications, pq)
	}
	return &out
}

func optDate(d domain.Date) cc.OptDate {
	if d == "" {
		return cc.OptDate{}
	}
	t, _ := time.Parse("2006-01-02", string(d))
	return cc.NewOptDate(t)
}

func subjects(in []cc.Subject) []string {
	out := []string{}
	for _, s := range in {
		out = append(out, string(s))
	}
	return out
}

func paramsIn(in cc.ObjectiveParams) map[string]domain.Param {
	out := map[string]domain.Param{}
	for name, p := range in {
		dp := domain.Param{Source: string(p.Source), Values: p.Values}
		if n, ok := p.Number.Get(); ok {
			dp.Number = &n
		}
		out[name] = dp
	}
	return out
}

func objectiveOut(o *Objective) *cc.Objective {
	out := &cc.Objective{ID: o.ID, TemplateID: o.TemplateID, TemplateVersion: o.TemplateVersion, PolicyTemplateID: o.PolicyTemplateID,
		PolicyTemplateVersion: o.PolicyTemplateVersion, OrgUnitID: o.OrgUnitID, State: cc.ObjectiveState(o.State), Owner: cc.Subject(o.Owner),
		Participants: []cc.Subject{}, Params: cc.ObjectiveParams{}, Version: o.Version}
	for _, p := range o.Participants {
		out.Participants = append(out.Participants, cc.Subject(p))
	}
	for name, p := range o.Params {
		op := cc.ObjectiveParam{Source: cc.ObjectiveParamSource(p.Source), Values: p.Values}
		if p.Number != nil {
			op.Number = cc.NewOptInt64(*p.Number)
		}
		out.Params[name] = op
	}
	return out
}

// assignmentFields 是分派规则读取的字段级白名单（C-14）：每个读取级别只输出列出的字段，其余一律不出现。full 为能管理参与人或能分派
// 跟进的成员（看得到候选人标识），summary 为其他能查看目标的成员（只见版本与规则条数，不出现规则与候选人）。
var assignmentFields = map[string][]string{
	AssignmentFull:    {"objective_id", "version", "rule_count", "view", "rules", "updated_at"},
	AssignmentSummary: {"objective_id", "version", "rule_count", "view"},
}

// assignmentOut 按读取级别的白名单输出分派规则；未登记的级别失败关闭为 summary。
func assignmentOut(a *Assignment, level string) *cc.AssignmentRules {
	fields, ok := assignmentFields[level]
	if !ok {
		level, fields = AssignmentSummary, assignmentFields[AssignmentSummary]
	}
	out := &cc.AssignmentRules{}
	for _, f := range fields {
		switch f {
		case "objective_id":
			out.ObjectiveID = a.ObjectiveID
		case "version":
			out.Version = a.Version
		case "rule_count":
			out.RuleCount = len(a.Rules)
		case "view":
			out.View = cc.AssignmentRulesView(level)
		case "rules":
			out.Rules = rulesOut(a.Rules)
		case "updated_at":
			if a.UpdatedAt != nil {
				out.UpdatedAt = cc.NewOptDateTime(*a.UpdatedAt)
			}
		}
	}
	return out
}

func rulesOut(in []domain.AssignmentRule) []cc.AssignmentRule {
	out := []cc.AssignmentRule{}
	for _, r := range in {
		out = append(out, cc.AssignmentRule{Assignee: cc.Subject(r.Assignee), Regions: r.Regions, Categories: r.Categories})
	}
	return out
}

func (s *Server) GetAssignmentRules(ctx context.Context, params cc.GetAssignmentRulesParams) (*cc.AssignmentRules, error) {
	a, level, err := s.Service.GetAssignment(ctx, params.ObjectiveID)
	if err != nil {
		return nil, err
	}
	return assignmentOut(a, level), nil
}

func (s *Server) UpdateAssignmentRules(ctx context.Context, req *cc.UpdateAssignmentRulesRequest, params cc.UpdateAssignmentRulesParams) (*cc.AssignmentRules, error) {
	rules := []domain.AssignmentRule{}
	for _, r := range req.Rules {
		rules = append(rules, domain.AssignmentRule{Assignee: string(r.Assignee), Regions: r.Regions, Categories: r.Categories})
	}
	a, err := s.Service.UpdateAssignment(ctx, params.ObjectiveID, req.ExpectedVersion, rules, params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return assignmentOut(a, AssignmentFull), nil
}
