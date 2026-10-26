// Package channels 组装 channels 渠道与会话 子系统。M1 第1段提供企业微信渠道的安装与停用、账号绑定与解绑、免登与卡片跳转
// （docs/01-product/prd/01-tenant-onboarding-and-sso.md F4–F7）；企业微信适配器尚未接入（api.NotWired），渠道内对话与通知随所属
// 段落实现。M2 第5段提供简报（偏好、组装、投递、历史与完整简报、重新推送、链接）与卡片快捷动作（docs/01-product/prd/05-briefing-quick-actions.md）：
// 组装、投递与动作执行以本地绑定 api.Briefings 提供，定时任务与工作进程尚未接入；覆盖与来源权利端口未接入（api.NotWired）。
package channels

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/channels/migrations"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 对外接口（经 gateway）的路由。渠道侧入口（安装回调、消息回调、免登）不带妙洽令牌，只以本地绑定 api.Callbacks 提供。
var publicRoutes = []string{
	"GET /v1/channels/installations", "POST /v1/channels/installations",
	"POST /v1/channels/installations/{installationID}/verify", "POST /v1/channels/installations/{installationID}/disable",
	"GET /v1/channels/bindings", "POST /v1/channels/bindings:prepare", "POST /v1/channels/bindings:confirm",
	"POST /v1/channels/bindings/{bindingID}/unbind", "POST /v1/channels/card-links:open",
	// PRD 第5段：简报偏好、历史、完整简报、重新推送、简报链接与投递统计
	"GET /v1/channels/briefing-preferences", "PUT /v1/channels/briefing-preferences", "GET /v1/channels/briefings",
	"GET /v1/channels/briefings/{briefingID}", "POST /v1/channels/briefings/{briefingID}/resend", "POST /v1/channels/briefing-links:open",
	"GET /v1/channels/delivery-stats",
}

// Adapters 是 channels 出站端口的适配器与业务时钟。零值字段使用缺省：WeCom 为未接入（api.NotWired，依赖它的操作失败关闭），
// Now 为系统时钟。生产企业微信适配器未获授权接入（C10 只批准了设计）。
//
// Coverage 与 Rights 是简报组装的覆盖与来源权利端口（缺省未接入：覆盖为“未知”，来源字段失败关闭为 none）。
type Adapters struct {
	WeCom    api.Provider
	Coverage api.Coverage
	Rights   api.Rights
	Now      func() time.Time
}

// Module 返回 channels 的模块定义（企业微信未接入）。
func Module() runtime.Module { return ModuleWith(Adapters{}) }

// ModuleWith 返回使用给定适配器的 channels 模块定义（测试使用内存实现，见 api/wecomtest）。配置：MIAOQIA_CHANNELS_SIGNING_KEY
// （动作令牌与翻页游标的签名密钥，base64，至少 32 字节，同一部署的全部实例相同；只有 development 可缺省）、MIAOQIA_CHANNELS_PACKS_DIR
// （应用领域包目录，默认 packs）。
func ModuleWith(adapters Adapters) runtime.Module {
	svc := &app.Service{WeCom: adapters.WeCom, Coverage: adapters.Coverage, Rights: adapters.Rights, Now: adapters.Now}
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
			// 第5段：简报取数与渠道内处理（detection）、目标访问与生效策略（customer-context）、审批读取（identity）
			briefing, ok := runtime.Lookup[detection.Briefing](env.Bindings, detection.BriefingBinding)
			if !ok {
				return fmt.Errorf("channels：缺少 %s 绑定", detection.BriefingBinding)
			}
			access, ok := runtime.Lookup[customercontext.ObjectiveAccess](env.Bindings, customercontext.AccessBinding)
			if !ok {
				return fmt.Errorf("channels：缺少 %s 绑定", customercontext.AccessBinding)
			}
			policies, ok := runtime.Lookup[customercontext.ActivePolicies](env.Bindings, customercontext.ActivePoliciesBinding)
			if !ok {
				return fmt.Errorf("channels：缺少 %s 绑定", customercontext.ActivePoliciesBinding)
			}
			approvals, ok := runtime.Lookup[identity.ApprovalReader](env.Bindings, identity.ApprovalReaderBinding)
			if !ok {
				return fmt.Errorf("channels：缺少 %s 绑定", identity.ApprovalReaderBinding)
			}
			key, err := signingKey(env)
			if err != nil {
				return err
			}
			dir := env.Config("PACKS_DIR")
			if dir == "" {
				dir = "packs"
			}
			packs, err := app.LoadPacks(dir)
			if err != nil {
				return err
			}
			// 强认证门禁只有本地绑定；取不到时管理动作失败关闭（503 identity_unavailable）
			guard, _ := runtime.Lookup[identity.Guard](env.Bindings, identity.GuardBinding)
			svc.DB, svc.Authz, svc.Guard, svc.Sessions, svc.Members, svc.Log = env.DB, a, guard, sessions, members, env.Log
			svc.Detection, svc.Access, svc.Policies, svc.Approvals, svc.Key, svc.Packs = briefing, access, policies, approvals, key, packs
			env.Bindings.Provide(api.CallbacksBinding, api.LocalCallbacks(svc))
			env.Bindings.Provide(api.MessagingBinding, api.LocalMessaging(svc))
			env.Bindings.Provide(api.MaintenanceBinding, api.LocalMaintenance(svc))
			env.Bindings.Provide(api.BriefingsBinding, api.LocalBriefings(svc))
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

// signingKey 读取 MIAOQIA_CHANNELS_SIGNING_KEY（base64，解码后至少 32 字节）；只有 development 可以缺省，此时每个进程随机生成
// （多进程开发环境中，一个进程签发的令牌在另一个进程不被接受，失败关闭）。
func signingKey(env *runtime.Env) ([]byte, error) {
	v := strings.TrimSpace(env.Config("SIGNING_KEY"))
	if v == "" {
		if !transportsecurity.Development() {
			return nil, fmt.Errorf("channels：缺少 MIAOQIA_CHANNELS_SIGNING_KEY（动作令牌与游标签名密钥）")
		}
		key := make([]byte, app.MinSigningKey)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if env.Log != nil {
			env.Log.Warn("channels：未配置 MIAOQIA_CHANNELS_SIGNING_KEY，开发环境使用进程内随机密钥")
		}
		return key, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(v); err == nil {
			if len(key) < app.MinSigningKey {
				break
			}
			return key, nil
		}
	}
	return nil, fmt.Errorf("channels：MIAOQIA_CHANNELS_SIGNING_KEY 须为 base64 编码、至少 %d 字节", app.MinSigningKey)
}
