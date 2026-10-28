package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"

	pur "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// Server 实现由 OpenAPI 生成的服务端接口（contracts/apis/pursuit/openapi.yaml）。
type Server struct {
	Service *Service
}

var _ pur.Handler = (*Server)(nil)

// WriteProtocolError 使生成服务端的解码与校验错误遵循同一 Problem 契约。
func WriteProtocolError(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := ogenerrors.ErrorCode(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&pur.Problem{Type: "https://errors.miaoqia.com/invalid_request", Title: "invalid_request", Status: status,
		Code: pur.NewOptString("invalid_request")})
}

// InternalOnly 标记内部端口（mTLS）上的请求：channel 为 wecom 的命令只在这里接受。
func InternalOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r.WithContext(Internal(r.Context()))) })
}

// NewError 把错误转换为 RFC 9457 Problem Details；未识别的错误不外泄细节。
func (s *Server) NewError(_ context.Context, err error) *pur.ProblemStatusCode {
	status, code, detail := http.StatusInternalServerError, "internal", ""
	var current, signal []byte
	var safe *api.Error
	if errors.As(err, &safe) {
		status, code, detail, current, signal = safe.Status, safe.Code, safe.Detail, safe.Current, safe.SignalCurrent
	}
	if errors.Is(err, reqctx.ErrMissing) {
		status, code = http.StatusUnauthorized, "missing_context"
	}
	p := pur.Problem{Type: "https://errors.miaoqia.com/" + code, Title: code, Status: status, Code: pur.NewOptString(code)}
	if detail != "" {
		p.Detail = pur.NewOptString(detail)
	}
	if current != nil {
		var st pur.PursuitState
		if json.Unmarshal(current, &st) == nil {
			p.Current = pur.NewOptPursuitState(st)
		}
	}
	if signal != nil {
		var sv pur.SignalVersion
		if json.Unmarshal(signal, &sv) == nil {
			p.SignalCurrent = pur.NewOptSignalVersion(sv)
		}
	}
	return &pur.ProblemStatusCode{StatusCode: status, Response: p}
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

func result(res *CommandResult, err error) (*pur.CommandResult, error) {
	if err != nil {
		return nil, err
	}
	return convert[pur.CommandResult](res)
}

func (s *Server) CreatePursuit(ctx context.Context, req *pur.CreatePursuitRequest, p pur.CreatePursuitParams) (*pur.CommandResult, error) {
	in, err := convert[CreateRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.CreatePursuit(ctx, p.IdempotencyKey, *in))
}

func (s *Server) GetPursuit(ctx context.Context, p pur.GetPursuitParams) (*pur.Pursuit, error) {
	d, err := s.Service.GetPursuit(ctx, string(p.PursuitID))
	if err != nil {
		return nil, err
	}
	return convert[pur.Pursuit](d)
}

func (s *Server) ListPursuits(ctx context.Context, p pur.ListPursuitsParams) (*pur.PursuitList, error) {
	list, err := s.Service.ListPursuits(ctx, string(p.View), string(p.ObjectiveID.Or("")), p.Limit.Or(50), string(p.Cursor.Or("")))
	if err != nil {
		return nil, err
	}
	return convert[pur.PursuitList](list)
}

func (s *Server) VoidPursuit(ctx context.Context, req *pur.VersionRequest, p pur.VoidPursuitParams) (*pur.CommandResult, error) {
	in, err := convert[VersionRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.VoidPursuit(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}

func (s *Server) AssignPursuit(ctx context.Context, req *pur.AssignRequest, p pur.AssignPursuitParams) (*pur.CommandResult, error) {
	in, err := convert[AssignRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.AssignPursuit(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}

func (s *Server) CreateTask(ctx context.Context, req *pur.CreateTaskRequest, p pur.CreateTaskParams) (*pur.CommandResult, error) {
	in, err := convert[CreateTaskRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.CreateTask(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}

func (s *Server) ClosePursuit(ctx context.Context, req *pur.CloseRequest, p pur.ClosePursuitParams) (*pur.CommandResult, error) {
	in, err := convert[CloseRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.ClosePursuit(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}

func (s *Server) ReviseOutcome(ctx context.Context, req *pur.ReviseOutcomeRequest, p pur.ReviseOutcomeParams) (*pur.CommandResult, error) {
	in, err := convert[ReviseRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.ReviseOutcome(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}

func (s *Server) GetTask(ctx context.Context, p pur.GetTaskParams) (*pur.TaskView, error) {
	v, err := s.Service.GetTask(ctx, string(p.TaskID))
	if err != nil {
		return nil, err
	}
	return convert[pur.TaskView](v)
}

func (s *Server) UpdateTask(ctx context.Context, req *pur.UpdateTaskRequest, p pur.UpdateTaskParams) (*pur.CommandResult, error) {
	in, err := convert[UpdateTaskRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.UpdateTask(ctx, string(p.TaskID), p.IdempotencyKey, *in))
}

func (s *Server) CompleteTask(ctx context.Context, req *pur.CompleteTaskRequest, p pur.CompleteTaskParams) (*pur.CommandResult, error) {
	in, err := convert[CompleteRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.CompleteTask(ctx, string(p.TaskID), p.IdempotencyKey, *in))
}

func (s *Server) ReopenTask(ctx context.Context, req *pur.ReopenTaskRequest, p pur.ReopenTaskParams) (*pur.CommandResult, error) {
	in, err := convert[ReopenRequest](req)
	if err != nil {
		return nil, err
	}
	return result(s.Service.ReopenTask(ctx, string(p.TaskID), p.IdempotencyKey, *in))
}

func (s *Server) ListMyTasks(ctx context.Context, p pur.ListMyTasksParams) (*pur.MyTaskList, error) {
	var before *time.Time
	if v, ok := p.Before.Get(); ok {
		before = &v
	}
	list, err := s.Service.ListMyTasks(ctx, before, p.Limit.Or(100), string(p.Cursor.Or("")))
	if err != nil {
		return nil, err
	}
	return convert[pur.MyTaskList](list)
}

// Actions 是 api.Actions 的本地实现（channels 以点击者的用户主体调用；授权与对应的公开接口相同）。本地绑定视同内部端口。
type Actions struct{ Service *Service }

func (a Actions) CreatePursuit(ctx context.Context, key string, req *api.CreatePursuitRequest) (*api.CommandResult, error) {
	return (&Server{Service: a.Service}).CreatePursuit(Internal(ctx), req, pur.CreatePursuitParams{IdempotencyKey: key})
}

func (a Actions) GetTask(ctx context.Context, task string) (*api.TaskView, error) {
	return (&Server{Service: a.Service}).GetTask(Internal(ctx), pur.GetTaskParams{TaskID: pur.TaskID(task)})
}

func (a Actions) CompleteTask(ctx context.Context, task, key string, req *api.CompleteTaskRequest) (*api.CommandResult, error) {
	return (&Server{Service: a.Service}).CompleteTask(Internal(ctx), req, pur.CompleteTaskParams{TaskID: pur.TaskID(task), IdempotencyKey: key})
}

func (a Actions) ReopenTask(ctx context.Context, task, key string, req *api.ReopenTaskRequest) (*api.CommandResult, error) {
	return (&Server{Service: a.Service}).ReopenTask(Internal(ctx), req, pur.ReopenTaskParams{TaskID: pur.TaskID(task), IdempotencyKey: key})
}

func (a Actions) ListMyTasks(ctx context.Context, before *time.Time, limit int, cursor string) (*api.MyTaskList, error) {
	p := pur.ListMyTasksParams{}
	if before != nil {
		p.Before = pur.NewOptDateTime(*before)
	}
	if limit > 0 {
		p.Limit = pur.NewOptInt(limit)
	}
	if cursor != "" {
		p.Cursor = pur.NewOptCursor(pur.Cursor(cursor))
	}
	return (&Server{Service: a.Service}).ListMyTasks(Internal(ctx), p)
}

func (a Actions) GetPursuit(ctx context.Context, pursuit string) (*api.Pursuit, error) {
	return (&Server{Service: a.Service}).GetPursuit(Internal(ctx), pur.GetPursuitParams{PursuitID: pur.PursuitID(pursuit)})
}

// GetTracking 是 api.Trackings 的本地实现（C-11/C-12：授权被拒通知的操作者为持续跟踪时以本人再检查）。本地绑定视同内部端口。
func (a Actions) GetTracking(ctx context.Context, tracking string) (*api.Tracking, error) {
	return (&Server{Service: a.Service}).GetTracking(Internal(ctx), pur.GetTrackingParams{PursuitID: pur.PursuitID(tracking)})
}
