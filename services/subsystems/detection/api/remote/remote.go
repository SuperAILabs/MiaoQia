// Package remote 是 detection 信号固定求值输入读取接口的远程绑定：由 contracts/apis/detection/openapi.yaml 生成的客户端，经调用管线。
// 调用以请求上下文中的用户主体转交（binding.Transport），授权在 detection 内按该用户判断。
package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

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

// NewBriefing 返回远程简报取数与渠道内处理接口；baseURL 同 NewExplainer。
func NewBriefing(baseURL string) (api.Briefing, error) {
	e, err := NewExplainer(baseURL)
	if err != nil {
		return nil, err
	}
	return e.(client), nil
}

// translateCurrent 与 translate 相同，另保留冲突时信号的当前值。
func translateCurrent(err error) error {
	var p *det.ProblemStatusCode
	if errors.As(err, &p) && p.StatusCode < 500 {
		out := &api.Error{Status: p.StatusCode, Code: p.Response.Code.Or("invalid_request"), Detail: p.Response.Detail.Or("")}
		if cur, ok := p.Response.Current.Get(); ok {
			if raw, err := cur.MarshalJSON(); err == nil {
				out.Current = raw
			}
		}
		return out
	}
	return translate(err)
}

func (c client) BriefingSignals(ctx context.Context, objective string, from, to time.Time) (*api.BriefingSignalPage, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "ListBriefingSignals", Idempotent: true},
		func(ctx context.Context) (*api.BriefingSignalPage, error) {
			out, err := c.c.ListBriefingSignals(ctx, det.ListBriefingSignalsParams{ObjectiveID: objective, From: from, To: to})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}

func (c client) SignalCards(ctx context.Context, ids []string) (*api.SignalCardList, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "ListSignalCards", Idempotent: true},
		func(ctx context.Context) (*api.SignalCardList, error) {
			out, err := c.c.ListSignalCards(ctx, det.ListSignalCardsParams{Ids: ids})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}

func (c client) Triage(ctx context.Context, signal, key string, req *api.TriageRequest) (*api.TriageResult, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "TriageSignal"},
		func(ctx context.Context) (*api.TriageResult, error) {
			out, err := c.c.TriageSignal(ctx, req, det.TriageSignalParams{SignalID: signal, IdempotencyKey: key})
			if err != nil {
				return nil, translateCurrent(err)
			}
			return out, nil
		})
}

func (c client) Undo(ctx context.Context, key string, req *api.UndoRequest) (*api.UndoResult, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "UndoSignalTriage"},
		func(ctx context.Context) (*api.UndoResult, error) {
			out, err := c.c.UndoSignalTriage(ctx, req, det.UndoSignalTriageParams{IdempotencyKey: key})
			if err != nil {
				return nil, translateCurrent(err)
			}
			return out, nil
		})
}

// NewCandidates 返回远程信号候选负责人读取接口（C-14）；baseURL 同 NewExplainer。
func NewCandidates(baseURL string) (api.SignalCandidates, error) {
	e, err := NewExplainer(baseURL)
	if err != nil {
		return nil, err
	}
	return e.(client), nil
}

func (c client) SignalCandidate(ctx context.Context, signalID string) (*api.SignalCandidate, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetSignalCandidateOwner", Idempotent: true},
		func(ctx context.Context) (*api.SignalCandidate, error) {
			out, err := c.c.GetSignalCandidateOwner(ctx, det.GetSignalCandidateOwnerParams{SignalID: signalID})
			if err != nil {
				return nil, translate(err)
			}
			return out, nil
		})
}
