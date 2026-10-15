// Package identity 组装 identity 子系统（租户、成员、角色、授权决策、审批）。M0 提供授权决策、最小目录生命周期及cell核验。
package identity

import (
	"context"
	"fmt"
	"net/http"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/identity/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/identity/internal/infra"
	"github.com/superailabs/miaoqia/services/subsystems/identity/migrations"
)

// Module 返回 identity 的模块定义。配置：MIAOQIA_IDENTITY_OPENFGA_URL。
func Module() runtime.Module {
	var decider app.Decider
	var directory *app.Directory
	return runtime.Module{
		Name:       api.Name,
		Database:   "iam",
		Migrations: migrations.FS,
		Start: func(ctx context.Context, env *runtime.Env) error {
			url := env.Config("OPENFGA_URL")
			if url == "" {
				return fmt.Errorf("identity：未配置 OPENFGA_URL")
			}
			d, err := infra.Connect(ctx, url)
			if err != nil {
				return err
			}
			directory = &app.Directory{DB: env.DB, Authorizer: d, Roles: d}
			decider = app.Decider{Store: d, Directory: directory}
			env.Bindings.Provide(api.AuthorizerBinding, api.Local(decider))
			env.Bindings.Provide(api.SessionBinding, api.LocalSession(directory))
			return nil
		},
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			handler := &app.Server{Decider: decider, Directory: directory}
			srv, err := identityapi.NewServer(handler, identityapi.WithPathPrefix("/internal"), identityapi.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range []string{"POST /internal/v1/authz:check", "GET /internal/v1/identity/session"} {
				internal.Handle(pattern, binding.InternalServer(api.Name, srv))
			}
			pub, err := identityapi.NewServer(handler, identityapi.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range []string{"POST /v1/tenants", "GET /v1/tenants/{tenantID}", "GET /v1/identity/members/{subject}", "PUT /v1/tenants/{tenantID}/state", "PUT /v1/identity/members/{subject}", "GET /v1/identity/session"} {
				public.Handle(pattern, pub)
			}
		},
	}
}

// Grant 写入一条授权关系（初始化与运维工具使用）。
func Grant(ctx context.Context, openfgaURL, subject, relation, object string) error {
	d, err := infra.Connect(ctx, openfgaURL)
	if err != nil {
		return err
	}
	return d.Grant(ctx, subject, relation, object)
}
