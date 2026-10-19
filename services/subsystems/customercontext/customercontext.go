// Package customercontext 组装 customer-context 客户上下文与策略 子系统。M2 第2段提供客户档案、上下文版本与快照、目标配置
// （docs/01-product/prd/02-profile-and-objectives.md）；策略、清单与知识资产随所属段落实现。
package customercontext

import (
	"context"
	"fmt"
	"net/http"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/infra"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/migrations"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 对外接口（经 gateway）与内部接口（mTLS 内部端口）的路由。
var (
	publicRoutes = []string{
		"GET /v1/customer-context/profile", "PUT /v1/customer-context/profile",
		"GET /v1/customer-context/objective-templates",
		"GET /v1/customer-context/objectives", "POST /v1/customer-context/objectives",
		"GET /v1/customer-context/objectives/{objectiveID}", "PUT /v1/customer-context/objectives/{objectiveID}",
		"PUT /v1/customer-context/objectives/{objectiveID}/state",
	}
	internalRoutes = []string{"GET /internal/v1/customer-context/context-version", "GET /internal/v1/customer-context/context-snapshot"}
)

// Module 返回 customer-context 的模块定义。配置：MIAOQIA_CUSTOMER_CONTEXT_PACKS_DIR（扩展包目录，默认 packs）。
func Module() runtime.Module {
	svc := &app.Service{}
	return runtime.Module{
		Name:       api.Name,
		Database:   "ctx",
		Migrations: migrations.FS,
		Start: func(ctx context.Context, env *runtime.Env) error {
			dir := env.Config("PACKS_DIR")
			if dir == "" {
				dir = "packs"
			}
			catalog, err := infra.LoadCatalog(dir)
			if err != nil {
				return err
			}
			a, ok := runtime.Lookup[identity.Authorizer](env.Bindings, identity.AuthorizerBinding)
			if !ok {
				return fmt.Errorf("customer-context：缺少 %s 绑定", identity.AuthorizerBinding)
			}
			svc.DB, svc.Authz, svc.Catalog = env.DB, a, catalog
			env.Bindings.Provide(api.ContextBinding, api.Local(app.Reader{Service: svc}))
			return nil
		},
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			handler := &app.Server{Service: svc}
			in, err := cc.NewServer(handler, cc.WithPathPrefix("/internal"), cc.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range internalRoutes {
				internal.Handle(pattern, binding.InternalServer(api.Name, in))
			}
			pub, err := cc.NewServer(handler, cc.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range publicRoutes {
				public.Handle(pattern, pub)
			}
		},
	}
}
