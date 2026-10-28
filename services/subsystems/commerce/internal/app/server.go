package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ogen-go/ogen/ogenerrors"

	com "github.com/superailabs/miaoqia/services/contracts/apis/commerce"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/api"
)

func commerceKind(kind string) com.ViewKind { return com.ViewKind(kind) }

// Server 实现由 OpenAPI 生成的服务端接口（contracts/apis/commerce/openapi.yaml）。
type Server struct{ Service *Service }

var _ com.Handler = (*Server)(nil)

// Metering 是 api.ViewMetering 的本地实现。
type Metering struct{ Service *Service }

func (m Metering) RecordView(ctx context.Context, kind, target, requestKey string) (*api.Receipt, error) {
	return m.Service.RecordView(ctx, kind, target, requestKey)
}

// WriteProtocolError 使生成服务端的解码与校验错误遵循同一 Problem 契约。
func WriteProtocolError(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&com.Problem{Type: "https://errors.miaoqia.com/invalid_request", Title: "invalid_request", Status: status,
		Code: com.NewOptString("invalid_request")})
}

// NewError 把错误转换为 RFC 9457 Problem Details；未识别的错误不外泄细节。
func (s *Server) NewError(_ context.Context, err error) *com.ProblemStatusCode {
	status, code := http.StatusInternalServerError, "internal"
	p := com.Problem{}
	var safe *api.Error
	if errors.As(err, &safe) {
		status, code = safe.Status, safe.Code
		if safe.Detail != "" {
			p.Detail = com.NewOptString(safe.Detail)
		}
		if safe.Code == "quota.exhausted" {
			p.Used, p.Limit, p.ResetsAt = com.NewOptInt64(safe.Used), com.NewOptInt64(safe.Limit), com.NewOptDateTime(safe.ResetsAt)
		}
	}
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	p.Type, p.Title, p.Status, p.Code = "https://errors.miaoqia.com/"+code, code, status, com.NewOptString(code)
	return &com.ProblemStatusCode{StatusCode: status, Response: p}
}

func (s *Server) RecordView(ctx context.Context, req *com.ViewMeterRequest, p com.RecordViewParams) (*com.ViewMeterReceipt, error) {
	return s.Service.RecordView(ctx, string(req.Kind), req.Target, p.IdempotencyKey)
}

func (s *Server) CheckCoverage(ctx context.Context, p com.CheckCoverageParams) (*com.CoverageDecision, error) {
	d, err := s.Service.CheckCoverage(ctx, p.InfoDomain, p.Region.Or(""))
	if err != nil {
		return nil, err
	}
	out := &com.CoverageDecision{Covered: d.Covered, InfoDomain: p.InfoDomain, Region: p.Region, Missing: []com.CoverageDecisionMissingItem{},
		Source: com.CoverageDecisionSourceDeploymentConfig}
	for _, m := range d.Missing {
		out.Missing = append(out.Missing, com.CoverageDecisionMissingItem(m))
	}
	return out, nil
}
