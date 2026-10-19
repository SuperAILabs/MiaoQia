// Package remote 是 identity 接口的远程绑定：由 contracts/apis/identity/openapi.yaml 生成的客户端，经调用管线。
package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// NewAuthorizer 返回远程授权决策点；baseURL 为 identity 内部接口地址，例如 http://platform-api:8091/internal。
func NewAuthorizer(baseURL string) (api.Authorizer, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || (!transportsecurity.Development() && u.Scheme != "https") {
		return nil, fmt.Errorf("identity remote requires authenticated HTTPS")
	}
	base, err := transportsecurity.Client()
	if err != nil {
		return nil, err
	}
	cl, err := identityapi.NewClient(baseURL, identityapi.WithClient(&http.Client{Transport: binding.Transport{Base: base}, CheckRedirect: refuseRedirect}))
	if err != nil {
		return nil, err
	}
	return client{cl}, nil
}

// refuseRedirect 拒绝内部授权接口的任何重定向：重定向后的请求会重新附加请求上下文头，且目标未经入口校验，可能降级为明文或转向其他主机。
func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("identity remote refuses redirects")
}

type client struct{ c *identityapi.Client }

func (c client) Check(ctx context.Context, req authz.Request) (bool, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "Check", Idempotent: true},
		func(ctx context.Context) (bool, error) {
			body := &identityapi.CheckRequest{Relation: req.Relation, Object: req.Object}
			if req.Subject != "" {
				body.Subject = identityapi.NewOptString(req.Subject)
			}
			res, err := c.c.CheckAuthorization(ctx, body)
			if err != nil {
				var p *identityapi.ProblemStatusCode
				if errors.As(err, &p) && p.StatusCode >= 500 {
					return false, fmt.Errorf("%w：%s", binding.ErrTransient, p.Response.Title)
				}
				return false, err
			}
			return res.Allowed, nil
		})
}

// Resolve uses the same authenticated client and retry pipeline as Check.
func (c client) Resolve(ctx context.Context) (*api.Session, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "ResolveSession", Idempotent: true}, func(ctx context.Context) (*api.Session, error) {
		value, err := c.c.ResolveSession(ctx)
		if err != nil {
			var p *identityapi.ProblemStatusCode
			if errors.As(err, &p) {
				if p.StatusCode >= 500 {
					return nil, fmt.Errorf("%w: identity unavailable", binding.ErrTransient)
				}
				return nil, &api.Error{Status: p.StatusCode, Code: p.Response.Code.Value}
			}
			return nil, err
		}
		return value, nil
	})
}

// problem 把远程错误转换为与本地绑定相同的错误：5xx 为暂时性错误（重试），其余为带稳定 code 的 api.Error。
func problem(err error) error {
	var p *identityapi.ProblemStatusCode
	if errors.As(err, &p) {
		if p.StatusCode >= 500 {
			return fmt.Errorf("%w: identity unavailable", binding.ErrTransient)
		}
		return &api.Error{Status: p.StatusCode, Code: p.Response.Code.Value}
	}
	return err
}

// OrgUnit 读取上下文租户内的组织单元。
func (c client) OrgUnit(ctx context.Context, id string) (*api.OrgUnit, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "GetOrgUnit", Idempotent: true}, func(ctx context.Context) (*api.OrgUnit, error) {
		if identityapi.OrgUnitID(id).Validate() != nil {
			return nil, &api.Error{Status: 400, Code: "invalid_request"}
		}
		value, err := c.c.GetOrgUnit(ctx, identityapi.GetOrgUnitParams{UnitID: identityapi.OrgUnitID(id)})
		if err != nil {
			return nil, problem(err)
		}
		return value, nil
	})
}

// SyncResourceParticipants 同步资源上的参与关系；按参与版本幂等，暂时性失败可重试。
func (c client) SyncResourceParticipants(ctx context.Context, req *api.SyncParticipantsRequest) (*api.SyncParticipantsResult, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "SyncResourceParticipants", Idempotent: true}, func(ctx context.Context) (*api.SyncParticipantsResult, error) {
		if req == nil || req.Validate() != nil {
			return nil, &api.Error{Status: 400, Code: "invalid_request"}
		}
		value, err := c.c.SyncResourceParticipants(ctx, req)
		if err != nil {
			return nil, problem(err)
		}
		return value, nil
	})
}

// IssueChannelSession 签发渠道免登会话；按凭据标识幂等，暂时性失败可重试（channels 只在凭据有效期内调用）。
func (c client) IssueChannelSession(ctx context.Context, req *api.IssueChannelSessionRequest) (*api.ChannelSession, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "IssueChannelSession", Idempotent: true}, func(ctx context.Context) (*api.ChannelSession, error) {
		if req == nil || req.Validate() != nil {
			return nil, &api.Error{Status: 400, Code: "invalid_request"}
		}
		value, err := c.c.IssueChannelSession(ctx, req)
		if err != nil {
			return nil, problem(err)
		}
		return value, nil
	})
}

// RevokeChannelSessions 撤销绑定版本及以下的免登会话；按版本幂等。
func (c client) RevokeChannelSessions(ctx context.Context, req *api.RevokeChannelSessionsRequest) (*api.ChannelSessionRevocation, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "RevokeChannelSessions", Idempotent: true}, func(ctx context.Context) (*api.ChannelSessionRevocation, error) {
		if req == nil || req.Validate() != nil {
			return nil, &api.Error{Status: 400, Code: "invalid_request"}
		}
		value, err := c.c.RevokeChannelSessions(ctx, req)
		if err != nil {
			return nil, problem(err)
		}
		return value, nil
	})
}

// MemberLifecycle 读取成员生命周期（状态、成员版本、最近一次退出时的成员版本）。
func (c client) MemberLifecycle(ctx context.Context, subject string) (*api.ChannelMemberLifecycle, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "MemberLifecycle", Idempotent: true}, func(ctx context.Context) (*api.ChannelMemberLifecycle, error) {
		value, err := c.c.GetChannelMemberLifecycle(ctx, identityapi.GetChannelMemberLifecycleParams{Subject: subject})
		if err != nil {
			return nil, problem(err)
		}
		return value, nil
	})
}

// ListLiveChannelSessions 分页列出仍有有效免登会话的绑定。
func (c client) ListLiveChannelSessions(ctx context.Context, cursor string, limit int) (*api.LiveChannelBindingList, error) {
	return binding.Invoke(ctx, binding.Remote, binding.Op{Subsystem: api.Name, Name: "ListLiveChannelSessions", Idempotent: true}, func(ctx context.Context) (*api.LiveChannelBindingList, error) {
		params := identityapi.ListLiveChannelSessionsParams{}
		if cursor != "" {
			params.Cursor = identityapi.NewOptChannelSessionCursor(identityapi.ChannelSessionCursor(cursor))
		}
		if limit != 0 {
			params.Limit = identityapi.NewOptInt(limit)
		}
		value, err := c.c.ListLiveChannelSessions(ctx, params)
		if err != nil {
			return nil, problem(err)
		}
		return value, nil
	})
}
