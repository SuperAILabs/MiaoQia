// Package remote 是 customer-context 上下文读取接口的远程绑定：由 contracts/apis/customer-context/openapi.yaml 生成的客户端，经调用管线。
package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
)

// NewContextReader 返回远程上下文读取接口；baseURL 为 customer-context 内部接口地址，例如 https://platform-api:8091/internal。
func NewContextReader(baseURL string) (api.ContextReader, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || (!transportsecurity.Development() && u.Scheme != "https") {
		return nil, fmt.Errorf("customer-context remote requires authenticated HTTPS")
	}
	base, err := transportsecurity.Client()
	if err != nil {
		return nil, err
	}
	cl, err := cc.NewClient(baseURL, cc.WithClient(&http.Client{Transport: binding.Transport{Base: base}, CheckRedirect: refuseRedirect}))
	if err != nil {
		return nil, err
	}
	return client{cl}, nil
}

// NewPolicyReader 返回远程策略仿真输入读取接口；baseURL 同 NewContextReader。
func NewPolicyReader(baseURL string) (api.PolicyReader, error) {
	r, err := NewContextReader(baseURL)
	if err != nil {
		return nil, err
	}
	return r.(client), nil
}

// NewObjectiveAccess 返回远程目标访问判断接口；baseURL 同 NewContextReader。调用以请求上下文中的用户主体转交。
func NewObjectiveAccess(baseURL string) (api.ObjectiveAccess, error) {
	r, err := NewContextReader(baseURL)
	if err != nil {
		return nil, err
	}
	return r.(client), nil
}

// refuseRedirect 拒绝重定向：重定向后的请求会重新附加请求上下文头，且目标未经入口校验。
func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("customer-context remote refuses redirects")
}

type client struct{ c *cc.Client }

// translate 把远程 Problem 还原为与本地绑定相同的稳定错误；5xx 为暂时性错误，可重试。
func translate(err error) error {
	var p *cc.ProblemStatusCode
	if errors.As(err, &p) {
		if p.StatusCode >= 500 {
			return fmt.Errorf("%w：%s", binding.ErrTransient, p.Response.Code.Or("unavailable"))
		}
		return &api.Error{Status: p.StatusCode, Code: p.Response.Code.Or("invalid_request"), Detail: p.Response.Detail.Or("")}
	}
	return err
}

func (c client) ContextVersion(ctx context.Context) (*api.Version, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetContextVersion", Idempotent: true},
		func(ctx context.Context) (*api.Version, error) {
			v, err := c.c.GetContextVersion(ctx)
			if err != nil {
				return nil, translate(err)
			}
			return v, nil
		})
}

func (c client) ContextSnapshot(ctx context.Context, orgUnit string, version int64) (*api.Snapshot, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetContextSnapshot", Idempotent: true},
		func(ctx context.Context) (*api.Snapshot, error) {
			// 0 为当前版本（不带参数），正数为指定版本；负数与本地绑定一样是无效请求，不能退化为当前版本。
			if version < 0 {
				return nil, &api.Error{Status: http.StatusBadRequest, Code: "invalid_request", Detail: "context_version 不能为负数"}
			}
			params := cc.GetContextSnapshotParams{OrgUnitID: orgUnit}
			if version > 0 {
				params.ContextVersion = cc.NewOptInt64(version)
			}
			s, err := c.c.GetContextSnapshot(ctx, params)
			if err != nil {
				return nil, translate(err)
			}
			return s, nil
		})
}

func (c client) SimulationInput(ctx context.Context, objective string, revision int64) (*api.SimulationInput, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetPolicySimulationInput", Idempotent: true},
		func(ctx context.Context) (*api.SimulationInput, error) {
			in, err := c.c.GetPolicySimulationInput(ctx, cc.GetPolicySimulationInputParams{ObjectiveID: objective, Revision: revision})
			if err != nil {
				return nil, translate(err)
			}
			return in, nil
		})
}

func (c client) Access(ctx context.Context, objective string, relations []string) ([]string, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetObjectiveAccess", Idempotent: true},
		func(ctx context.Context) ([]string, error) {
			params := cc.GetObjectiveAccessParams{ObjectiveID: objective}
			for _, r := range relations {
				params.Relation = append(params.Relation, cc.ObjectiveRelation(r))
			}
			res, err := c.c.GetObjectiveAccess(ctx, params)
			if err != nil {
				return nil, translate(err)
			}
			out := make([]string, 0, len(res.Allowed))
			for _, r := range res.Allowed {
				out = append(out, string(r))
			}
			return out, nil
		})
}
