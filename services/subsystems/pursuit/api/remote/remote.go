// Package remote 是 pursuit 渠道内动作接口的远程绑定：由 contracts/apis/pursuit/openapi.yaml 生成的客户端，经调用管线。
// 调用以请求上下文中的用户主体转交（binding.Transport），授权在 pursuit 内按该用户判断。
package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	pur "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// NewActions 返回远程渠道内动作接口；baseURL 为 pursuit 内部接口地址，例如 https://platform-api:8091/internal。
func NewActions(baseURL string) (api.Actions, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || (!transportsecurity.Development() && u.Scheme != "https") {
		return nil, fmt.Errorf("pursuit remote requires authenticated HTTPS")
	}
	base, err := transportsecurity.Client()
	if err != nil {
		return nil, err
	}
	cl, err := pur.NewClient(baseURL, pur.WithClient(&http.Client{Transport: binding.Transport{Base: base}, CheckRedirect: refuseRedirect}))
	if err != nil {
		return nil, err
	}
	return client{cl}, nil
}

// refuseRedirect 拒绝重定向：重定向后的请求会重新附加请求上下文头，且目标未经入口校验。
func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("pursuit remote refuses redirects")
}

// NewTrackings 返回远程持续跟踪读取接口（C-11/C-12）；baseURL 同 NewActions。
func NewTrackings(baseURL string) (api.Trackings, error) {
	a, err := NewActions(baseURL)
	if err != nil {
		return nil, err
	}
	return a.(client), nil
}

type client struct{ c *pur.Client }

// translate 把远程 Problem 还原为与本地绑定相同的稳定错误（保留冲突时的当前值）；5xx 为暂时性错误。
func translate(err error) error {
	var p *pur.ProblemStatusCode
	if errors.As(err, &p) {
		if p.StatusCode >= 500 {
			return fmt.Errorf("%w：%s", binding.ErrTransient, p.Response.Code.Or("unavailable"))
		}
		out := &api.Error{Status: p.StatusCode, Code: p.Response.Code.Or("invalid_request"), Detail: p.Response.Detail.Or("")}
		if cur, ok := p.Response.Current.Get(); ok {
			out.Current, _ = cur.MarshalJSON()
		}
		if cur, ok := p.Response.SignalCurrent.Get(); ok {
			out.SignalCurrent, _ = cur.MarshalJSON()
		}
		return out
	}
	return err
}

func (c client) CreatePursuit(ctx context.Context, key string, req *api.CreatePursuitRequest) (*api.CommandResult, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "CreatePursuit", Timeout: 10 * time.Second},
		func(ctx context.Context) (*api.CommandResult, error) {
			out, err := c.c.CreatePursuit(ctx, req, pur.CreatePursuitParams{IdempotencyKey: key})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}

func (c client) GetTask(ctx context.Context, task string) (*api.TaskView, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetTask", Idempotent: true},
		func(ctx context.Context) (*api.TaskView, error) {
			out, err := c.c.GetTask(ctx, pur.GetTaskParams{TaskID: pur.TaskID(task)})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}

func (c client) CompleteTask(ctx context.Context, task, key string, req *api.CompleteTaskRequest) (*api.CommandResult, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "CompleteTask"},
		func(ctx context.Context) (*api.CommandResult, error) {
			out, err := c.c.CompleteTask(ctx, req, pur.CompleteTaskParams{TaskID: pur.TaskID(task), IdempotencyKey: key})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}

func (c client) ReopenTask(ctx context.Context, task, key string, req *api.ReopenTaskRequest) (*api.CommandResult, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "ReopenTask"},
		func(ctx context.Context) (*api.CommandResult, error) {
			out, err := c.c.ReopenTask(ctx, req, pur.ReopenTaskParams{TaskID: pur.TaskID(task), IdempotencyKey: key})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}

func (c client) ListMyTasks(ctx context.Context, before *time.Time, limit int, cursor string) (*api.MyTaskList, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "ListMyTasks", Idempotent: true},
		func(ctx context.Context) (*api.MyTaskList, error) {
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
			out, err := c.c.ListMyTasks(ctx, p)
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}

func (c client) GetPursuit(ctx context.Context, pursuit string) (*api.Pursuit, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetPursuit", Idempotent: true},
		func(ctx context.Context) (*api.Pursuit, error) {
			out, err := c.c.GetPursuit(ctx, pur.GetPursuitParams{PursuitID: pur.PursuitID(pursuit)})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}

func (c client) GetTracking(ctx context.Context, tracking string) (*api.Tracking, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetTracking", Idempotent: true},
		func(ctx context.Context) (*api.Tracking, error) {
			out, err := c.c.GetTracking(ctx, pur.GetTrackingParams{PursuitID: pur.PursuitID(tracking)})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}
