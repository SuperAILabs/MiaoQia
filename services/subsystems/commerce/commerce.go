// Package commerce 组装 commerce 订阅计量计费 子系统。M2 第4段提供对象查看与原文查看的计量与额度（docs/01-product/prd/04-object-page-evidence.md
// F9）：内部接口 recordView（gateway 以用户主体调用）与补记消费（gateway.view_meter.deferred.v1）；以及数据覆盖判断 checkCoverage
// （gateway 的对象页与 learning 的代表样本以用户主体调用）。套餐、权益与账单随所属里程碑实现（docs/05-delivery/01-roadmap.md）；
// 权益接入前额度与覆盖包来自部署配置。
package commerce

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
	_ "time/tzdata" // 计量日时区：容器内可能没有系统时区数据

	com "github.com/superailabs/miaoqia/services/contracts/apis/commerce"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/api"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/migrations"
)

// 内部接口（mTLS 内部端口）的路由。
var internalRoutes = []string{"POST /internal/v1/commerce/view-meters", "GET /internal/v1/commerce/coverage"}

// Adapters 是业务时钟（测试用）。零值为系统时钟。
type Adapters struct {
	Now func() time.Time
}

// Module 返回 commerce 的模块定义。
func Module() runtime.Module { return ModuleWith(Adapters{}) }

// ModuleWith 返回使用给定时钟的 commerce 模块定义。配置：MIAOQIA_COMMERCE_OBJECT_VIEW_DAILY_QUOTA 与
// MIAOQIA_COMMERCE_EVIDENCE_VIEW_DAILY_QUOTA（每租户每计量日，缺省 2000）、MIAOQIA_COMMERCE_METERING_TZ（缺省 Asia/Shanghai）、
// MIAOQIA_COMMERCE_COVERAGE（数据覆盖包，格式见 app.ParseCoverage；未配置时覆盖判断为 503 coverage_unavailable）。
func ModuleWith(adapters Adapters) runtime.Module {
	svc := &app.Service{Now: adapters.Now}
	return runtime.Module{
		Name:       api.Name,
		Database:   "com",
		Migrations: migrations.FS,
		Start: func(ctx context.Context, env *runtime.Env) error {
			quotas := map[string]int64{}
			for kind, key := range map[string]string{api.ObjectView: "OBJECT_VIEW_DAILY_QUOTA", api.EvidenceView: "EVIDENCE_VIEW_DAILY_QUOTA"} {
				quotas[kind] = app.DefaultDailyQuota
				if v := env.Config(key); v != "" {
					n, err := strconv.ParseInt(v, 10, 64)
					if err != nil || n <= 0 {
						return fmt.Errorf("commerce：%s 无效", key)
					}
					quotas[kind] = n
				}
			}
			tz := env.Config("METERING_TZ")
			if tz == "" {
				tz = "Asia/Shanghai"
			}
			zone, err := time.LoadLocation(tz)
			if err != nil {
				return fmt.Errorf("commerce：计量时区 %s 无效：%w", tz, err)
			}
			plan, err := app.ParseCoverage(env.Config("COVERAGE"))
			if err != nil {
				return err
			}
			if plan == nil && env.Log != nil {
				env.Log.Warn("commerce：未配置 MIAOQIA_COMMERCE_COVERAGE，覆盖判断不可用（对象页与证据面板 503）")
			}
			svc.DB, svc.Log, svc.Quotas, svc.Zone, svc.Coverage = env.DB, env.Log, quotas, zone, plan
			env.Bindings.Provide(api.MeteringBinding, api.LocalMetering(app.Metering{Service: svc}))
			env.Bindings.Provide(api.CoverageBinding, api.LocalCoverage(app.Coverage{Service: svc}))
			return nil
		},
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			in, err := com.NewServer(&app.Server{Service: svc}, com.WithPathPrefix("/internal"), com.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range internalRoutes {
				internal.Handle(pattern, binding.InternalServer(api.Name, in))
			}
		},
		// 补记：消费 gateway 在计量服务不可用时放行查看后发布的补记事件（租户域主题）。
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{{
				Name: "commerce.view-backfill", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
				Topics:   []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "gateway")},
				Handlers: map[string]consumer.Handler{app.ViewDeferred: svc.ViewDeferred},
			}}
		},
	}
}
