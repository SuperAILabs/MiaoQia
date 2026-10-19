// Package app 是 identity 的用例编排。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ogen-go/ogen/ogenerrors"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Server 实现内部接口（由 OpenAPI 生成的服务端调用）。
type Server struct {
	Decider   authz.Decider
	Directory *Directory
}

// WriteProtocolError 使生成服务端的解码/校验错误遵循同一Problem契约。
func WriteProtocolError(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&identityapi.Problem{Type: "https://errors.miaoqia.com/invalid_request", Title: "请求无效", Status: status, Code: identityapi.NewOptString("invalid_request")})
}

var _ identityapi.Handler = (*Server)(nil)

// CheckAuthorization 是授权决策（语义见 Decider）。
func (s *Server) CheckAuthorization(ctx context.Context, req *identityapi.CheckRequest) (*identityapi.CheckResult, error) {
	subject, _ := req.Subject.Get()
	allowed, err := s.Decider.Check(ctx, authz.Request{Subject: subject, Relation: req.Relation, Object: req.Object})
	if err != nil {
		return nil, err
	}
	return &identityapi.CheckResult{Allowed: allowed}, nil
}

// NewError 把错误转换为 RFC 9457 Problem Details。
func (s *Server) NewError(_ context.Context, err error) *identityapi.ProblemStatusCode {
	status, code := http.StatusInternalServerError, "internal"
	var safe *api.Error
	if errors.As(err, &safe) {
		status, code = safe.Status, safe.Code
	}
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	if errors.Is(err, authz.ErrInvalid) {
		status, code = http.StatusBadRequest, "invalid_request"
	}
	return &identityapi.ProblemStatusCode{StatusCode: status, Response: identityapi.Problem{
		Type: "https://errors.miaoqia.com/" + code, Title: code, Status: status, Code: identityapi.NewOptString(code)}}
}

func (s *Server) CreateTenant(ctx context.Context, req *identityapi.CreateTenantRequest, params identityapi.CreateTenantParams) (*identityapi.Tenant, error) {
	return s.Directory.Create(ctx, req, params.IdempotencyKey)
}
func (s *Server) SetTenantState(ctx context.Context, req *identityapi.SetTenantStateRequest, params identityapi.SetTenantStateParams) (*identityapi.Tenant, error) {
	return s.Directory.SetState(ctx, params.TenantID, req, params.IdempotencyKey)
}
func (s *Server) PutMember(ctx context.Context, req *identityapi.PutMemberRequest, params identityapi.PutMemberParams) (*identityapi.Session, error) {
	return s.Directory.Put(ctx, params.Subject, req, params.IdempotencyKey)
}
func (s *Server) ResolveSession(ctx context.Context) (*identityapi.Session, error) {
	return s.Directory.Resolve(ctx)
}

func (s *Server) GetTenant(ctx context.Context, params identityapi.GetTenantParams) (*identityapi.Tenant, error) {
	return s.Directory.GetTenant(ctx, params.TenantID)
}
func (s *Server) GetMember(ctx context.Context, params identityapi.GetMemberParams) (*identityapi.Session, error) {
	return s.Directory.GetMember(ctx, params.Subject)
}

func (s *Server) ListOrgUnits(ctx context.Context) (*identityapi.OrgUnitList, error) {
	return s.Directory.ListOrgUnits(ctx)
}
func (s *Server) GetOrgUnit(ctx context.Context, params identityapi.GetOrgUnitParams) (*identityapi.OrgUnit, error) {
	return s.Directory.OrgUnit(ctx, string(params.UnitID))
}
func (s *Server) CreateOrgUnit(ctx context.Context, req *identityapi.CreateOrgUnitRequest, params identityapi.CreateOrgUnitParams) (*identityapi.OrgUnit, error) {
	return s.Directory.CreateOrgUnit(ctx, req, params.IdempotencyKey)
}
func (s *Server) UpdateOrgUnit(ctx context.Context, req *identityapi.UpdateOrgUnitRequest, params identityapi.UpdateOrgUnitParams) (*identityapi.OrgUnit, error) {
	return s.Directory.UpdateOrgUnit(ctx, string(params.UnitID), req, params.IdempotencyKey)
}
func (s *Server) SyncResourceParticipants(ctx context.Context, req *identityapi.SyncParticipantsRequest) (*identityapi.SyncParticipantsResult, error) {
	return s.Directory.SyncResourceParticipants(ctx, req)
}

func (s *Server) CreateInvitation(ctx context.Context, req *identityapi.CreateInvitationRequest, params identityapi.CreateInvitationParams) (*identityapi.InvitationIssued, error) {
	return s.Directory.CreateInvitation(ctx, req, params.IdempotencyKey)
}
func (s *Server) ListInvitations(ctx context.Context, params identityapi.ListInvitationsParams) (*identityapi.InvitationList, error) {
	return s.Directory.ListInvitations(ctx, params)
}
func (s *Server) ResendInvitation(ctx context.Context, req *identityapi.InvitationVersionRequest, params identityapi.ResendInvitationParams) (*identityapi.InvitationIssued, error) {
	return s.Directory.ResendInvitation(ctx, string(params.InvitationID), req, params.IdempotencyKey)
}
func (s *Server) RevokeInvitation(ctx context.Context, req *identityapi.InvitationVersionRequest, params identityapi.RevokeInvitationParams) (*identityapi.Invitation, error) {
	return s.Directory.RevokeInvitation(ctx, string(params.InvitationID), req, params.IdempotencyKey)
}
func (s *Server) AcceptInvitation(ctx context.Context, req *identityapi.AcceptInvitationRequest) (*identityapi.InvitationAcceptance, error) {
	return s.Directory.AcceptInvitation(ctx, req)
}

func (s *Server) IssueChannelSession(ctx context.Context, req *identityapi.IssueChannelSessionRequest) (*identityapi.ChannelSession, error) {
	return s.Directory.IssueChannelSession(ctx, req)
}
func (s *Server) GetChannelMemberLifecycle(ctx context.Context, params identityapi.GetChannelMemberLifecycleParams) (*identityapi.ChannelMemberLifecycle, error) {
	return s.Directory.MemberLifecycle(ctx, params.Subject)
}
func (s *Server) RevokeChannelSessions(ctx context.Context, req *identityapi.RevokeChannelSessionsRequest) (*identityapi.ChannelSessionRevocation, error) {
	return s.Directory.RevokeChannelSessions(ctx, req)
}
func (s *Server) ListLiveChannelSessions(ctx context.Context, params identityapi.ListLiveChannelSessionsParams) (*identityapi.LiveChannelBindingList, error) {
	return s.Directory.ListLiveChannelSessions(ctx, string(params.Cursor.Or("")), params.Limit.Or(maxLiveBindings))
}
