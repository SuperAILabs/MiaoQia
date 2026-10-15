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
)

// Server 实现内部接口（由 OpenAPI 生成的服务端调用）。
type Server struct {
	Decider authz.Decider
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
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	if errors.Is(err, authz.ErrInvalid) {
		status, code = http.StatusBadRequest, "invalid_request"
	}
	return &identityapi.ProblemStatusCode{StatusCode: status, Response: identityapi.Problem{
		Type: "https://errors.miaoqia.com/" + code, Title: err.Error(), Status: status, Code: identityapi.NewOptString(code)}}
}
