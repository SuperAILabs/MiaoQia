package bff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ogen-go/ogen/ogenerrors"

	gw "github.com/superailabs/miaoqia/services/contracts/apis/gateway"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// Server 实现由 OpenAPI 生成的服务端接口（contracts/apis/gateway/openapi.yaml）。
type Server struct{ Service *Service }

var _ gw.Handler = (*Server)(nil)

// WriteProtocolError 使生成服务端的解码与校验错误遵循同一 Problem 契约。
func WriteProtocolError(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&gw.Problem{Type: "https://errors.miaoqia.com/invalid_request", Title: "invalid_request", Status: status,
		Code: gw.NewOptString("invalid_request")})
}

// NewError 把错误转换为 RFC 9457 Problem Details；未识别的错误不外泄细节。
func (s *Server) NewError(_ context.Context, err error) *gw.ProblemStatusCode {
	status, code := http.StatusInternalServerError, "internal"
	p := gw.Problem{}
	var safe *Error
	if errors.As(err, &safe) {
		status, code = safe.Status, safe.Code
		if safe.Detail != "" {
			p.Detail = gw.NewOptString(safe.Detail)
		}
		if safe.Object != nil {
			p.Object = gw.NewOptObjectName(gw.ObjectName{ID: safe.Object.ID, Type: gw.ObjectType(safe.Object.Type), Name: safe.Object.Name})
		}
		if safe.Code == "quota.exhausted" {
			p.Used, p.Limit, p.ResetsAt = gw.NewOptInt64(safe.Used), gw.NewOptInt64(safe.Limit), gw.NewOptDateTime(safe.ResetsAt)
		}
	}
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	p.Type, p.Title, p.Status, p.Code = "https://errors.miaoqia.com/"+code, code, status, gw.NewOptString(code)
	return &gw.ProblemStatusCode{StatusCode: status, Response: p}
}

// convert 把用例类型转换为契约类型（字段同名，经 JSON 转换）。
func convert[T any](v any) (*T, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Server) GetObjectPage(ctx context.Context, p gw.GetObjectPageParams) (*gw.ObjectPage, error) {
	page, err := s.Service.Page(ctx, p.ObjectID, string(p.View.Or(gw.ViewCurrent)), p.SignalID.Or(""), p.IdempotencyKey.Or(""))
	if err != nil {
		return nil, err
	}
	return convert[gw.ObjectPage](page)
}

func (s *Server) GetObjectSection(ctx context.Context, p gw.GetObjectSectionParams) (*gw.ObjectSection, error) {
	sec, err := s.Service.GetSection(ctx, p.ObjectID, string(p.Section), string(p.View.Or(gw.ViewCurrent)), p.SignalID.Or(""), p.IdempotencyKey.Or(""),
		p.Mine.Or(false), p.Limit.Or(pageLimit), p.Cursor.Or(""))
	if err != nil {
		return nil, err
	}
	return convert[gw.ObjectSection](sec)
}

func (s *Server) ResolveEvidence(ctx context.Context, p gw.ResolveEvidenceParams) (*gw.EvidenceView, error) {
	e, err := s.Service.GetEvidence(ctx, p.EvidenceID, p.IdempotencyKey.Or(""))
	if err != nil {
		return nil, err
	}
	return convert[gw.EvidenceView](e)
}
