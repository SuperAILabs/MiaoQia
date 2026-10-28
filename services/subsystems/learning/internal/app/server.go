package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"

	lrn "github.com/superailabs/miaoqia/services/contracts/apis/learning"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api"
)

// Server 实现由 OpenAPI 生成的服务端接口（contracts/apis/learning/openapi.yaml）。
type Server struct {
	Service *Service
}

var _ lrn.Handler = (*Server)(nil)

// WriteProtocolError 使生成服务端的解码与校验错误遵循同一 Problem 契约。
func WriteProtocolError(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&lrn.Problem{Type: "https://errors.miaoqia.com/invalid_request", Title: "invalid_request", Status: status,
		Code: lrn.NewOptString("invalid_request")})
}

// NewError 把错误转换为 RFC 9457 Problem Details；未识别的错误不外泄细节。
func (s *Server) NewError(_ context.Context, err error) *lrn.ProblemStatusCode {
	status, code, detail := http.StatusInternalServerError, "internal", ""
	var safe *api.Error
	if errors.As(err, &safe) {
		status, code, detail = safe.Status, safe.Code, safe.Detail
	}
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	p := lrn.Problem{Type: "https://errors.miaoqia.com/" + code, Title: code, Status: status, Code: lrn.NewOptString(code)}
	if detail != "" {
		p.Detail = lrn.NewOptString(detail)
	}
	return &lrn.ProblemStatusCode{StatusCode: status, Response: p}
}

// RunOut 把运行转换为契约类型（字段同名，经 JSON 转换）。
func RunOut(r *Run) (*lrn.SimulationRun, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var out lrn.SimulationRun
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Server) SimulatePolicy(ctx context.Context, req *lrn.SimulatePolicyRequest) (*lrn.SimulationRun, error) {
	r, err := s.Service.SimulatePolicy(ctx, req.ObjectiveID, req.Revision)
	if err != nil {
		return nil, err
	}
	return RunOut(r)
}

func (s *Server) GetSimulation(ctx context.Context, params lrn.GetSimulationParams) (*lrn.SimulationRun, error) {
	r, err := s.Service.GetSimulation(ctx, string(params.SimulationID))
	if err != nil {
		return nil, err
	}
	return RunOut(r)
}

func (s *Server) GetFeedbackSummary(ctx context.Context, params lrn.GetFeedbackSummaryParams) (*lrn.FeedbackSummary, error) {
	sum, err := s.Service.GetFeedbackSummary(ctx, params.ObjectiveID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		return nil, err
	}
	var out lrn.FeedbackSummary
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Server) GetOutcomeSummary(ctx context.Context, params lrn.GetOutcomeSummaryParams) (*lrn.OutcomeSummary, error) {
	var from, asOf *time.Time
	if v, ok := params.From.Get(); ok {
		from = &v
	}
	if v, ok := params.AsOf.Get(); ok {
		asOf = &v
	}
	sum, err := s.Service.GetOutcomeSummary(ctx, params.ObjectiveID, from, asOf)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		return nil, err
	}
	var out lrn.OutcomeSummary
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
