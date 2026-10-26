// Package customercontext 组装 customer-context 客户上下文与策略 子系统。M2 第2段提供客户档案、上下文版本与快照、目标配置
// （docs/01-product/prd/02-profile-and-objectives.md）；第7段提供策略草案、版本历史与差异、激活与回滚申请及其执行
// （docs/01-product/prd/07-policy-simulation-approval.md）；清单与知识资产随所属段落实现。
package customercontext

import (
	"context"
	"fmt"
	"net/http"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
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
		"GET /v1/customer-context/objectives/{objectiveID}/policy",
		"PUT /v1/customer-context/objectives/{objectiveID}/policy/draft",
		"POST /v1/customer-context/objectives/{objectiveID}/policy/draft:discard",
		"GET /v1/customer-context/objectives/{objectiveID}/policy/versions",
		"GET /v1/customer-context/objectives/{objectiveID}/policy/versions/{version}",
		"GET /v1/customer-context/objectives/{objectiveID}/policy/diff",
		"POST /v1/customer-context/objectives/{objectiveID}/policy/activation-requests",
		"POST /v1/customer-context/objectives/{objectiveID}/policy/rollback-requests",
	}
	internalRoutes = []string{"GET /internal/v1/customer-context/context-version", "GET /internal/v1/customer-context/context-snapshot",
		"GET /internal/v1/customer-context/objectives/{objectiveID}/policy/simulation-input",
		"GET /internal/v1/customer-context/objectives/{objectiveID}/access", "GET /internal/v1/customer-context/active-policies"}
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
			policies, err := infra.LoadPolicies(dir)
			if err != nil {
				return err
			}
			a, ok := runtime.Lookup[identity.Authorizer](env.Bindings, identity.AuthorizerBinding)
			if !ok {
				return fmt.Errorf("customer-context：缺少 %s 绑定", identity.AuthorizerBinding)
			}
			directory, ok := runtime.Lookup[identity.Directory](env.Bindings, identity.DirectoryBinding)
			if !ok {
				return fmt.Errorf("customer-context：缺少 %s 绑定", identity.DirectoryBinding)
			}
			approvals, ok := runtime.Lookup[identity.Approvals](env.Bindings, identity.ApprovalsBinding)
			if !ok {
				return fmt.Errorf("customer-context：缺少 %s 绑定", identity.ApprovalsBinding)
			}
			svc.DB, svc.Authz, svc.Directory, svc.Catalog, svc.Log = env.DB, a, directory, catalog, env.Log
			svc.Policies, svc.Features, svc.Approvals = policies.Templates, policies.Features, approvals
			env.Bindings.Provide(api.ContextBinding, api.Local(app.Reader{Service: svc}))
			env.Bindings.Provide(api.PolicyBinding, api.LocalPolicy(app.PolicyReader{Service: svc}))
			env.Bindings.Provide(api.AccessBinding, api.LocalAccess(app.ObjectiveAccessor{Service: svc}))
			env.Bindings.Provide(api.ScopeBinding, api.LocalScope(app.ObjectiveAccessor{Service: svc}))
			env.Bindings.Provide(api.ActivePoliciesBinding, api.LocalActivePolicies(app.ActivePolicyReader{Service: svc}))
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
		// 目标参与关系同步的重试：消费本子系统的 objective.changed（经发件箱），按目标当前版本调用 identity。
		// 策略申请的登记对账、执行与关闭：消费 identity 的审批事件（平台域主题）；仿真附着：消费 learning 的 simulation.completed。
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{
				{
					Name: "customer-context.objective-participants", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
					Topics:   []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "customer_context")},
					Handlers: map[string]consumer.Handler{app.ObjectiveChanged: svc.SyncObjectiveParticipants},
				},
				{
					Name: "customer-context.policy-approvals", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
					Topics: []string{outbox.Topic(reqctx.ZonePlatform, env.Cell, "identity")},
					Handlers: map[string]consumer.Handler{
						"identity.approval.requested.v1": svc.ApprovalRequested,
						"identity.approval.decided.v1":   svc.ApprovalDecided,
						"identity.approval.closed.v1":    svc.ApprovalClosed,
					},
				},
				{
					Name: "customer-context.policy-simulations", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
					Topics:   []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "learning")},
					Handlers: map[string]consumer.Handler{"learning.simulation.completed.v1": svc.AttachSimulation},
				},
			}
		},
	}
}
