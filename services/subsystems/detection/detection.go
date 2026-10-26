// Package detection 组装 detection 监测与匹配 子系统。M2 第3段提供生效边界上的匹配（消费 domain.fact.upserted.v1，每批读取
// 生效策略与策略代次）、信号收件箱、处理与反馈、批量与撤销（docs/01-product/prd/03-signal-inbox-feedback.md）：视图组装与订阅反查
// 尚未接入（api.NotWired，事件处理失败并重试、转入死信），测试与演示使用标明为夹具的内存实现（api/facttest）。
// 第4段提供对象页的本租户信号分区（listObjectSignals；项目经归属边展开，归属边读取 api.ProjectLinks 尚未接入）与供 gateway
// 读取信号固定求值输入的绑定（api.ExplainerBinding，内部端口为 explainSignal）。第5段提供简报取数、信号卡片与渠道内处理的绑定
// （api.BriefingBinding，内部端口为 listBriefingSignals、listSignalCards、triageSignal、undoSignalTriage）。关注、回填、重新求值与漏报诊断随所属段落实现。
package detection

import (
	"context"
	"fmt"
	"net/http"
	"time"

	det "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/detection/migrations"
)

// 对外接口（经 gateway）的路由。
var publicRoutes = []string{
	"GET /v1/detection/objectives/{objectiveID}/signals",
	"GET /v1/detection/objectives/{objectiveID}/signal-selection",
	"POST /v1/detection/objectives/{objectiveID}/signal-batches",
	"GET /v1/detection/signals/{signalID}",
	"GET /v1/detection/signals/{signalID}/explanation",
	"GET /v1/detection/signals/{signalID}/triage-records",
	"POST /v1/detection/signals/{signalID}/triage",
	"GET /v1/detection/signal-batches/{batchID}",
	"POST /v1/detection/signal-batches/{batchID}/cancel",
	"POST /v1/detection/signal-batches/{batchID}/retry",
	"POST /v1/detection/triage-undos",
	"GET /v1/detection/object-signals",
}

// 内部接口（mTLS 内部端口）的路由：gateway 以用户主体读取信号固定的求值输入（远程绑定）。
// 第5段起 channels 以收件人或点击者的用户主体读取简报条目与信号卡片，并在渠道内处理与撤销（api.BriefingBinding）。简报条目与卡片
// 只经内部端口提供（不对外）。
var internalRoutes = []string{
	"GET /internal/v1/detection/signals/{signalID}/explanation",
	"GET /internal/v1/detection/objectives/{objectiveID}/briefing-signals",
	"GET /internal/v1/detection/signal-cards",
	"POST /internal/v1/detection/signals/{signalID}/triage",
	"POST /internal/v1/detection/triage-undos",
}

// Adapters 是匹配端口的适配器、业务时钟与批量作业的测试钩子。零值字段使用缺省：Facts 与 Candidates 为未接入（api.NotWired），
// Now 为系统时钟，LeaseTTL 为 2 分钟，BatchHook 为空。
type Adapters struct {
	Facts        api.Facts
	Candidates   api.Candidates
	ProjectLinks api.ProjectLinks
	Now          func() time.Time
	LeaseTTL     time.Duration
	BatchHook    func(ctx context.Context, batchID string, position int) error
}

// Module 返回 detection 的模块定义（匹配适配器未接入）。
func Module() runtime.Module { return ModuleWith(Adapters{}) }

// ModuleWith 返回使用给定适配器的 detection 模块定义。配置：MIAOQIA_DETECTION_PACKS_DIR（扩展包目录，默认 packs）。
func ModuleWith(adapters Adapters) runtime.Module {
	svc := &app.Service{Facts: adapters.Facts, Candidates: adapters.Candidates, Links: adapters.ProjectLinks, Now: adapters.Now, LeaseTTL: adapters.LeaseTTL}
	if adapters.BatchHook != nil {
		svc.Hook = adapters.BatchHook
	}
	if svc.Facts == nil {
		svc.Facts = api.NotWired{}
	}
	if svc.Candidates == nil {
		svc.Candidates = api.NotWired{}
	}
	if svc.Links == nil {
		svc.Links = api.NotWired{}
	}
	return runtime.Module{
		Name:       api.Name,
		Database:   "det",
		Migrations: migrations.FS,
		Start: func(ctx context.Context, env *runtime.Env) error {
			dir := env.Config("PACKS_DIR")
			if dir == "" {
				dir = "packs"
			}
			packs, err := app.LoadPacks(dir)
			if err != nil {
				return err
			}
			access, ok := runtime.Lookup[customercontext.ObjectiveAccess](env.Bindings, customercontext.AccessBinding)
			if !ok {
				return fmt.Errorf("detection：缺少 %s 绑定", customercontext.AccessBinding)
			}
			reader, ok := runtime.Lookup[customercontext.ContextReader](env.Bindings, customercontext.ContextBinding)
			if !ok {
				return fmt.Errorf("detection：缺少 %s 绑定", customercontext.ContextBinding)
			}
			policies, ok := runtime.Lookup[customercontext.ActivePolicies](env.Bindings, customercontext.ActivePoliciesBinding)
			if !ok {
				return fmt.Errorf("detection：缺少 %s 绑定", customercontext.ActivePoliciesBinding)
			}
			svc.DB, svc.Access, svc.Context, svc.Policies, svc.Packs, svc.Cell, svc.Log = env.DB, access, reader, policies, packs, env.Cell, env.Log
			env.Bindings.Provide(api.ExplainerBinding, api.LocalExplainer(app.Explainer{Service: svc}))
			env.Bindings.Provide(api.BriefingBinding, api.LocalBriefing(app.Briefing{Service: svc}))
			return nil
		},
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			handler := &app.Server{Service: svc}
			in, err := det.NewServer(handler, det.WithPathPrefix("/internal"), det.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range internalRoutes {
				internal.Handle(pattern, binding.InternalServer(api.Name, in))
			}
			pub, err := det.NewServer(handler, det.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range publicRoutes {
				public.Handle(pattern, pub)
			}
		},
		// 匹配：消费公共域的 domain.fact.upserted.v1（每个候选租户在各自的租户事务中处理）。
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{{
				Name: "detection.fact-matching", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
				Topics:   []string{outbox.Topic(reqctx.ZonePublic, "", "domain")},
				Handlers: map[string]consumer.Handler{"domain.fact.upserted.v1": svc.FactUpserted},
			}}
		},
	}
}
