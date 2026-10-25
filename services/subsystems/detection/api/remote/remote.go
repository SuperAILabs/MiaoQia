// Package remote 是 detection 信号固定求值输入读取接口的远程绑定：由 contracts/apis/detection/openapi.yaml 生成的客户端，经调用管线。
// 调用以请求上下文中的用户主体转交（binding.Transport），授权在 detection 内按该用户判断。
package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	det "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// NewExplainer 返回远程信号固定求值输入读取接口；baseURL 为 detection 内部接口地址，例如 https://platform-api:8091/internal。
func NewExplainer(baseURL string) (api.SignalExplainer, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || (!transportsecurity.Development() && u.Scheme != "https") {
		return nil, fmt.Errorf("detection remote requires authenticated HTTPS")
	}
	base, err := transportsecurity.Client()
	if err != nil {
		return nil, err
	}
	cl, err := det.NewClient(baseURL, det.WithClient(&http.Client{Transport: binding.Transport{Base: base}, CheckRedirect: refuseRedirect}))
	if err != nil {
		return nil, err
	}
	return client{cl}, nil
}

// refuseRedirect 拒绝重定向：重定向后的请求会重新附加请求上下文头，且目标未经入口校验。
func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("detection remote refuses redirects")
}

type client struct{ c *det.Client }

// translate 把远程 Problem 还原为与本地绑定相同的稳定错误；5xx 为暂时性错误，可重试。
func translate(err error) error {
	var p *det.ProblemStatusCode
	if errors.As(err, &p) {
		if p.StatusCode >= 500 {
			return fmt.Errorf("%w：%s", binding.ErrTransient, p.Response.Code.Or("unavailable"))
		}
		return &api.Error{Status: p.StatusCode, Code: p.Response.Code.Or("invalid_request"), Detail: p.Response.Detail.Or("")}
	}
	return err
}

func (c client) Explain(ctx context.Context, signalID string) (*api.Explanation, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "ExplainSignal", Idempotent: true},
		func(ctx context.Context) (*api.Explanation, error) {
			e, err := c.c.ExplainSignal(ctx, det.ExplainSignalParams{SignalID: signalID})
			if err != nil {
				return nil, translate(err)
			}
			return e, nil
		})
}
