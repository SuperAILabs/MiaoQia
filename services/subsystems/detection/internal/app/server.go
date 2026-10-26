package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"

	det "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// Server 实现由 OpenAPI 生成的服务端接口（contracts/apis/detection/openapi.yaml）。
type Server struct {
	Service *Service
}

var _ det.Handler = (*Server)(nil)

// WriteProtocolError 使生成服务端的解码与校验错误遵循同一 Problem 契约。
func WriteProtocolError(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&det.Problem{Type: "https://errors.miaoqia.com/invalid_request", Title: "invalid_request", Status: status,
		Code: det.NewOptString("invalid_request")})
}

// NewError 把错误转换为 RFC 9457 Problem Details；未识别的错误不外泄细节。
func (s *Server) NewError(_ context.Context, err error) *det.ProblemStatusCode {
	status, code, detail := http.StatusInternalServerError, "internal", ""
	var current []byte
	var safe *api.Error
	if errors.As(err, &safe) {
		status, code, detail, current = safe.Status, safe.Code, safe.Detail, safe.Current
	}
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	p := det.Problem{Type: "https://errors.miaoqia.com/" + code, Title: code, Status: status, Code: det.NewOptString(code)}
	if detail != "" {
		p.Detail = det.NewOptString(detail)
	}
	if current != nil {
		var st det.SignalState
		if json.Unmarshal(current, &st) == nil {
			p.Current = det.NewOptSignalState(st)
		}
	}
	return &det.ProblemStatusCode{StatusCode: status, Response: p}
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

func optTime(o det.OptDateTime) *time.Time {
	if v, ok := o.Get(); ok {
		return &v
	}
	return nil
}

func optInt64(o det.OptInt64) *int64 {
	if v, ok := o.Get(); ok {
		return &v
	}
	return nil
}

func (s *Server) ListSignals(ctx context.Context, p det.ListSignalsParams) (*det.SignalPage, error) {
	f := Filter{Tab: string(p.Tab.Or(det.ListSignalsTabOpen)), Disposition: string(p.Disposition.Or("")), FactType: p.FactType.Or(""),
		Region: p.Region.Or(""), Category: p.Category.Or(""), AmountMin: optInt64(p.AmountMin), AmountMax: optInt64(p.AmountMax),
		From: optTime(p.DiscoveredFrom), To: optTime(p.DiscoveredTo), MyFeedback: string(p.MyFeedback.Or("")), Marker: string(p.Marker.Or(""))}
	page, err := s.Service.ListSignals(ctx, p.ObjectiveID, f, string(p.Sort.Or(det.ListSignalsSortScore)), p.Limit.Or(pageSize), p.Cursor.Or(""))
	if err != nil {
		return nil, err
	}
	return convert[det.SignalPage](page)
}

func (s *Server) SelectSignals(ctx context.Context, p det.SelectSignalsParams) (*det.SignalSelection, error) {
	f := Filter{Tab: string(p.Tab.Or(det.SelectSignalsTabOpen)), Disposition: string(p.Disposition.Or("")), FactType: p.FactType.Or(""),
		Region: p.Region.Or(""), Category: p.Category.Or(""), AmountMin: optInt64(p.AmountMin), AmountMax: optInt64(p.AmountMax),
		From: optTime(p.DiscoveredFrom), To: optTime(p.DiscoveredTo), MyFeedback: string(p.MyFeedback.Or("")), Marker: string(p.Marker.Or(""))}
	sel, err := s.Service.SelectSignals(ctx, p.ObjectiveID, f)
	if err != nil {
		return nil, err
	}
	return convert[det.SignalSelection](sel)
}

func (s *Server) ListObjectSignals(ctx context.Context, p det.ListObjectSignalsParams) (*det.ObjectSignalPage, error) {
	page, err := s.Service.ListObjectSignals(ctx, p.ObjectID, string(p.Role.Or(det.ListObjectSignalsRoleAny)), p.Limit.Or(objectPageSize), p.Cursor.Or(""))
	if err != nil {
		return nil, err
	}
	return convert[det.ObjectSignalPage](page)
}

// Explainer 是 api.SignalExplainer 的本地实现（gateway 对象页读取信号固定的求值输入；授权与 explainSignal 相同）。
type Explainer struct{ Service *Service }

func (e Explainer) Explain(ctx context.Context, signalID string) (*api.Explanation, error) {
	out, err := e.Service.ExplainSignal(ctx, signalID)
	if err != nil {
		return nil, err
	}
	return convert[api.Explanation](out)
}

func (s *Server) GetSignal(ctx context.Context, p det.GetSignalParams) (*det.SignalDetail, error) {
	d, err := s.Service.GetSignal(ctx, p.SignalID)
	if err != nil {
		return nil, err
	}
	return convert[det.SignalDetail](d)
}

func (s *Server) ExplainSignal(ctx context.Context, p det.ExplainSignalParams) (*det.Explanation, error) {
	e, err := s.Service.ExplainSignal(ctx, p.SignalID)
	if err != nil {
		return nil, err
	}
	return convert[det.Explanation](e)
}

func (s *Server) ListSignalTriage(ctx context.Context, p det.ListSignalTriageParams) (*det.TriageRecordPage, error) {
	page, err := s.Service.ListTriage(ctx, p.SignalID, p.Cursor.Or(""))
	if err != nil {
		return nil, err
	}
	return convert[det.TriageRecordPage](page)
}

func (s *Server) TriageSignal(ctx context.Context, req *det.TriageRequest, p det.TriageSignalParams) (*det.TriageResult, error) {
	in, err := convert[Request](req)
	if err != nil {
		return nil, err
	}
	res, err := s.Service.TriageSignal(ctx, p.SignalID, p.IdempotencyKey, *in)
	if err != nil {
		return nil, err
	}
	return convert[det.TriageResult](res)
}

func (s *Server) TriageSignals(ctx context.Context, req *det.TriageSignalsRequest, p det.TriageSignalsParams) (*det.SignalBatch, error) {
	in, err := convert[BatchRequest](req)
	if err != nil {
		return nil, err
	}
	b, err := s.Service.TriageSignals(ctx, p.ObjectiveID, p.IdempotencyKey, *in)
	if err != nil {
		return nil, err
	}
	return convert[det.SignalBatch](b)
}

func (s *Server) GetSignalBatch(ctx context.Context, p det.GetSignalBatchParams) (*det.SignalBatch, error) {
	b, err := s.Service.GetSignalBatch(ctx, p.BatchID)
	if err != nil {
		return nil, err
	}
	return convert[det.SignalBatch](b)
}

func (s *Server) CancelSignalBatch(ctx context.Context, p det.CancelSignalBatchParams) (*det.SignalBatch, error) {
	b, err := s.Service.CancelSignalBatch(ctx, p.BatchID, p.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return convert[det.SignalBatch](b)
}

func (s *Server) RetrySignalBatchItems(ctx context.Context, p det.RetrySignalBatchItemsParams) (*det.SignalBatch, error) {
	b, err := s.Service.RetrySignalBatchItems(ctx, p.BatchID, p.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return convert[det.SignalBatch](b)
}

func (s *Server) UndoSignalTriage(ctx context.Context, req *det.UndoRequest, p det.UndoSignalTriageParams) (*det.UndoResult, error) {
	in, err := convert[UndoRequest](req)
	if err != nil {
		return nil, err
	}
	res, err := s.Service.UndoSignalTriage(ctx, p.IdempotencyKey, *in)
	if err != nil {
		return nil, err
	}
	return convert[det.UndoResult](res)
}

func (s *Server) ListBriefingSignals(ctx context.Context, p det.ListBriefingSignalsParams) (*det.BriefingSignalPage, error) {
	page, err := s.Service.BriefingSignals(ctx, p.ObjectiveID, p.From, p.To)
	if err != nil {
		return nil, err
	}
	return convert[det.BriefingSignalPage](page)
}

func (s *Server) ListSignalCards(ctx context.Context, p det.ListSignalCardsParams) (*det.SignalCardList, error) {
	list, err := s.Service.SignalCards(ctx, p.Ids)
	if err != nil {
		return nil, err
	}
	return convert[det.SignalCardList](list)
}

// Briefing 是 api.Briefing 的本地实现（channels 以收件人或点击者的用户主体调用；授权与对应的公开接口相同）。
type Briefing struct{ Service *Service }

func (b Briefing) BriefingSignals(ctx context.Context, objective string, from, to time.Time) (*api.BriefingSignalPage, error) {
	page, err := b.Service.BriefingSignals(ctx, objective, from, to)
	if err != nil {
		return nil, err
	}
	return convert[api.BriefingSignalPage](page)
}

func (b Briefing) SignalCards(ctx context.Context, ids []string) (*api.SignalCardList, error) {
	list, err := b.Service.SignalCards(ctx, ids)
	if err != nil {
		return nil, err
	}
	return convert[api.SignalCardList](list)
}

func (b Briefing) Triage(ctx context.Context, signal, key string, req *api.TriageRequest) (*api.TriageResult, error) {
	return (&Server{Service: b.Service}).TriageSignal(ctx, req, det.TriageSignalParams{SignalID: signal, IdempotencyKey: key})
}

func (b Briefing) Undo(ctx context.Context, key string, req *api.UndoRequest) (*api.UndoResult, error) {
	return (&Server{Service: b.Service}).UndoSignalTriage(ctx, req, det.UndoSignalTriageParams{IdempotencyKey: key})
}
