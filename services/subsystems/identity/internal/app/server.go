// Package app 是 identity 的用例编排。
package app

import (
	"context"
	"errors"
	"net/http"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// Server 实现内部接口（由 OpenAPI 生成的服务端调用）。
type Server struct {
	Decider authz.Decider
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
	return &identityapi.ProblemStatusCode{StatusCode: status, Response: identityapi.Problem{
		Type: "https://errors.miaoqia.com/" + code, Title: err.Error(), Status: status, Code: identityapi.NewOptString(code)}}
}
