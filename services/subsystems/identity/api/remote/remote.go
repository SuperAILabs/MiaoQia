// Package remote 是 identity 接口的远程绑定：由 contracts/apis/identity/openapi.yaml 生成的客户端，经调用管线。
package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// NewAuthorizer 返回远程授权决策点；baseURL 为 identity 内部接口地址，例如 http://platform-api:8091/internal。
func NewAuthorizer(baseURL string) (api.Authorizer, error) {
	cl, err := identityapi.NewClient(baseURL, identityapi.WithClient(&http.Client{Transport: binding.Transport{}}))
	if err != nil {
		return nil, err
	}
	return client{cl}, nil
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
