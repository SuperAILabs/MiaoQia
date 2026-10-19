package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ogen-go/ogen/ogenerrors"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
)

// Server 实现由 OpenAPI 生成的服务端接口（contracts/apis/channels/openapi.yaml）。
type Server struct {
	Service *Service
}

var _ chn.Handler = (*Server)(nil)

// WriteProtocolError 使生成服务端的解码与校验错误遵循同一 Problem 契约。
func WriteProtocolError(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&chn.Problem{Type: "https://errors.miaoqia.com/invalid_request", Title: "invalid_request", Status: status,
		Code: chn.NewOptString("invalid_request")})
}

// NewError 把错误转换为 RFC 9457 Problem Details；未识别的错误不外泄细节。account_mismatch 带出链接收件人。
func (s *Server) NewError(_ context.Context, err error) *chn.ProblemStatusCode {
	status, code, recipient := http.StatusInternalServerError, "internal", ""
	var safe *api.Error
	if errors.As(err, &safe) {
		status, code, recipient = safe.Status, safe.Code, safe.Recipient
	}
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	p := chn.Problem{Type: "https://errors.miaoqia.com/" + code, Title: code, Status: status, Code: chn.NewOptString(code)}
	if recipient != "" {
		p.Recipient = chn.NewOptString(recipient)
	}
	return &chn.ProblemStatusCode{StatusCode: status, Response: p}
}

func (s *Server) ListInstallations(ctx context.Context) (*chn.InstallationList, error) {
	return s.Service.ListInstallations(ctx)
}

func (s *Server) InstallChannel(ctx context.Context, req *chn.InstallChannelRequest, params chn.InstallChannelParams) (*chn.InstallationStarted, error) {
	return s.Service.InstallChannel(ctx, req, params.IdempotencyKey)
}

func (s *Server) VerifyInstallation(ctx context.Context, req *chn.VersionRequest, params chn.VerifyInstallationParams) (*chn.Installation, error) {
	return s.Service.VerifyInstallation(ctx, string(params.InstallationID), req, params.IdempotencyKey)
}

func (s *Server) DisableInstallation(ctx context.Context, req *chn.VersionRequest, params chn.DisableInstallationParams) (*chn.Installation, error) {
	return s.Service.DisableInstallation(ctx, string(params.InstallationID), req, params.IdempotencyKey)
}

func (s *Server) ListBindings(ctx context.Context, params chn.ListBindingsParams) (*chn.BindingList, error) {
	return s.Service.ListBindings(ctx, params)
}

func (s *Server) PrepareBinding(ctx context.Context, req *chn.BindingLinkRequest) (*chn.BindingPreview, error) {
	return s.Service.PrepareBinding(ctx, req)
}

func (s *Server) ConfirmBinding(ctx context.Context, req *chn.ConfirmBindingRequest) (*chn.BindingConfirmation, error) {
	return s.Service.ConfirmBinding(ctx, req)
}

func (s *Server) UnbindAccount(ctx context.Context, req *chn.UnbindRequest, params chn.UnbindAccountParams) (*chn.Binding, error) {
	return s.Service.UnbindAccount(ctx, string(params.BindingID), req, params.IdempotencyKey)
}

func (s *Server) OpenCardLink(ctx context.Context, req *chn.OpenCardLinkRequest) (*chn.CardTarget, error) {
	return s.Service.OpenCardLink(ctx, req)
}
