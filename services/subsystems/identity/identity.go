// Package identity 组装 identity 子系统（租户、成员、角色、授权决策、审批）。M0 提供授权决策点。
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
)

// Module 返回 identity 的模块定义。配置：MIAOQIA_IDENTITY_OPENFGA_URL。
func Module() runtime.Module {
	var decider app.Decider
	return runtime.Module{
		Name:     api.Name,
		Database: "iam",
		Start: func(ctx context.Context, env *runtime.Env) error {
			url := env.Config("OPENFGA_URL")
			if url == "" {
				return fmt.Errorf("identity：未配置 OPENFGA_URL")
			}
			d, err := infra.Connect(ctx, url)
			if err != nil {
				return err
			}
			decider = app.Decider{Store: d}
			env.Bindings.Provide(api.AuthorizerBinding, api.Local(decider))
			return nil
		},
		Routes: func(env *runtime.Env, _, internal *http.ServeMux) {
			srv, err := identityapi.NewServer(&app.Server{Decider: decider}, identityapi.WithPathPrefix("/internal"), identityapi.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			internal.Handle("/internal/v1/authz:check", binding.InternalServer(api.Name, srv))
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
