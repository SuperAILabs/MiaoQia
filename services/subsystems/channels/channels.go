// Package channels 组装 channels 渠道与会话 子系统。M1 第1段提供企业微信渠道的安装与停用、账号绑定与解绑、免登与卡片跳转
// （docs/01-product/prd/01-tenant-onboarding-and-sso.md F4–F7）；企业微信适配器尚未接入（api.NotWired），渠道内对话、
// 简报与通知随所属段落实现。
package channels

import (
	"context"
	"fmt"
	"net/http"
	"time"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/channels/migrations"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 对外接口（经 gateway）的路由。渠道侧入口（安装回调、消息回调、免登）不带妙洽令牌，只以本地绑定 api.Callbacks 提供。
var publicRoutes = []string{
	"GET /v1/channels/installations", "POST /v1/channels/installations",
	"POST /v1/channels/installations/{installationID}/verify", "POST /v1/channels/installations/{installationID}/disable",
	"GET /v1/channels/bindings", "POST /v1/channels/bindings:prepare", "POST /v1/channels/bindings:confirm",
	"POST /v1/channels/bindings/{bindingID}/unbind", "POST /v1/channels/card-links:open",
}

// Adapters 是 channels 出站端口的适配器与业务时钟。零值字段使用缺省：WeCom 为未接入（api.NotWired，依赖它的操作失败关闭），
// Now 为系统时钟。生产企业微信适配器未获授权接入（C10 只批准了设计）。
type Adapters struct {
	WeCom api.Provider
	Now   func() time.Time
}

// Module 返回 channels 的模块定义（企业微信未接入）。
func Module() runtime.Module { return ModuleWith(Adapters{}) }

// ModuleWith 返回使用给定适配器的 channels 模块定义（测试使用内存实现，见 api/wecomtest）。
func ModuleWith(adapters Adapters) runtime.Module {
	svc := &app.Service{WeCom: adapters.WeCom, Now: adapters.Now}
	return runtime.Module{
		Name:       api.Name,
		Database:   "chn",
		Migrations: migrations.FS,
		Start: func(ctx context.Context, env *runtime.Env) error {
			a, ok := runtime.Lookup[identity.Authorizer](env.Bindings, identity.AuthorizerBinding)
			if !ok {
				return fmt.Errorf("channels：缺少 %s 绑定", identity.AuthorizerBinding)
			}
			sessions, ok := runtime.Lookup[identity.ChannelSessions](env.Bindings, identity.ChannelSessionsBinding)
			if !ok {
				return fmt.Errorf("channels：缺少 %s 绑定", identity.ChannelSessionsBinding)
			}
			members, ok := runtime.Lookup[identity.SessionResolver](env.Bindings, identity.SessionBinding)
			if !ok {
				return fmt.Errorf("channels：缺少 %s 绑定", identity.SessionBinding)
			}
			// 强认证门禁只有本地绑定；取不到时管理动作失败关闭（503 identity_unavailable）
			guard, _ := runtime.Lookup[identity.Guard](env.Bindings, identity.GuardBinding)
			svc.DB, svc.Authz, svc.Guard, svc.Sessions, svc.Members, svc.Log = env.DB, a, guard, sessions, members, env.Log
			env.Bindings.Provide(api.CallbacksBinding, api.LocalCallbacks(svc))
			env.Bindings.Provide(api.MessagingBinding, api.LocalMessaging(svc))
			env.Bindings.Provide(api.MaintenanceBinding, api.LocalMaintenance(svc))
			return nil
		},
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			pub, err := chn.NewServer(&app.Server{Service: svc}, chn.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range publicRoutes {
				public.Handle(pattern, pub)
			}
		},
		// 绑定失效的撤销重试（本子系统的 channels.binding.changed.v1）与成员停用或退出（identity.member.changed.v1）。
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{
				{
					Name: "channels.binding-revocations", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
					Topics:   []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "channels")},
					Handlers: map[string]consumer.Handler{app.BindingChanged: svc.BindingChangedEvent},
				},
				{
					Name: "channels.member-changes", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
					Topics:   []string{outbox.Topic(reqctx.ZonePlatform, env.Cell, "identity")},
					Handlers: map[string]consumer.Handler{"identity.member.changed.v1": svc.MemberChanged},
				},
			}
		},
	}
}
