// Package remote 是 commerce 查看计量与覆盖判断接口的远程绑定：由 contracts/apis/commerce/openapi.yaml 生成的客户端，经调用管线。
// 调用以请求上下文中的用户主体转交（binding.Transport）。
package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	com "github.com/superailabs/miaoqia/services/contracts/apis/commerce"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/api"
)

// NewMetering 返回远程查看计量接口；baseURL 为 commerce 内部接口地址，例如 https://platform-api:8091/internal。
func NewMetering(baseURL string) (api.ViewMetering, error) { return newClient(baseURL) }

// NewCoverage 返回远程覆盖判断接口（地址同 NewMetering）。
func NewCoverage(baseURL string) (api.Coverage, error) { return newClient(baseURL) }

func newClient(baseURL string) (client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || (!transportsecurity.Development() && u.Scheme != "https") {
		return client{}, fmt.Errorf("commerce remote requires authenticated HTTPS")
	}
	base, err := transportsecurity.Client()
	if err != nil {
		return client{}, err
	}
	cl, err := com.NewClient(baseURL, com.WithClient(&http.Client{Transport: binding.Transport{Base: base}, CheckRedirect: refuseRedirect}))
	if err != nil {
		return client{}, err
	}
	return client{cl}, nil
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("commerce remote refuses redirects")
}

type client struct{ c *com.Client }

// translate 把远程 Problem 还原为与本地绑定相同的稳定错误（含 quota.exhausted 的用量）；5xx 为暂时性错误，可重试。
func translate(err error) error {
	var p *com.ProblemStatusCode
	if errors.As(err, &p) {
		if p.StatusCode >= 500 {
			return fmt.Errorf("%w：%s", binding.ErrTransient, p.Response.Code.Or("unavailable"))
		}
		e := &api.Error{Status: p.StatusCode, Code: p.Response.Code.Or("invalid_request"), Detail: p.Response.Detail.Or("")}
		e.Used, e.Limit = p.Response.Used.Or(0), p.Response.Limit.Or(0)
		if v, ok := p.Response.ResetsAt.Get(); ok {
			e.ResetsAt = v
		}
		return e
	}
	return err
}

func (c client) RecordView(ctx context.Context, kind, target, requestKey string) (*api.Receipt, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "RecordView", Idempotent: true},
		func(ctx context.Context) (*api.Receipt, error) {
			r, err := c.c.RecordView(ctx, &com.ViewMeterRequest{Kind: com.ViewKind(kind), Target: target}, com.RecordViewParams{IdempotencyKey: requestKey})
			if err != nil {
				return nil, translate(err)
			}
			return r, nil
		})
}

func (c client) CheckCoverage(ctx context.Context, infoDomain, region string) (*api.CoverageDecision, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "CheckCoverage", Idempotent: true},
		func(ctx context.Context) (*api.CoverageDecision, error) {
			params := com.CheckCoverageParams{InfoDomain: infoDomain}
			if region != "" {
				params.Region = com.NewOptString(region)
			}
			r, err := c.c.CheckCoverage(ctx, params)
			if err != nil {
				return nil, translate(err)
			}
			// 响应必须回答所问的对象：信息领域或区划不符时不采信（失败关闭）。
			if r.InfoDomain != infoDomain || r.Region.Or("") != region || r.Covered != (len(r.Missing) == 0) {
				return nil, fmt.Errorf("%w：覆盖判断响应与请求不符", binding.ErrTransient)
			}
			out := &api.CoverageDecision{Covered: r.Covered, Missing: []string{}}
			for _, m := range r.Missing {
				out.Missing = append(out.Missing, string(m))
			}
			return out, nil
		})
}
