// Package pursuit 组装 pursuit 推进与结果 子系统。M2 第6段提供交易型跟进（docs/01-product/prd/06-pursuit-follow-up.md）：
// 从信号、卡片或手动创建与关联、撤销创建、改派与协作者、任务与截止、提醒、关闭与结果修订、列表与详情，以及经 identity 的授权同步。
// 提醒的定时执行以本地绑定 api.Reminders 提供，定时任务角色尚未接入；持续跟踪、公开截止时间绑定随后续切片实现。
package pursuit

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	pur "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/migrations"
)

// 对外接口（经 gateway）与内部接口（mTLS 内部端口，channels 以点击者的用户主体调用）的路由。
var (
	publicRoutes = []string{
		"GET /v1/pursuit/pursuits", "POST /v1/pursuit/pursuits", "GET /v1/pursuit/pursuits/{pursuitID}",
		"POST /v1/pursuit/pursuits/{pursuitID}/void", "POST /v1/pursuit/pursuits/{pursuitID}/assign", "POST /v1/pursuit/pursuits/{pursuitID}/tasks",
		"POST /v1/pursuit/pursuits/{pursuitID}/close", "POST /v1/pursuit/pursuits/{pursuitID}/outcomes",
		"GET /v1/pursuit/tasks/{taskID}", "POST /v1/pursuit/tasks/{taskID}/update", "POST /v1/pursuit/tasks/{taskID}/complete",
		"POST /v1/pursuit/tasks/{taskID}/reopen", "GET /v1/pursuit/my-tasks",
	}
	internalRoutes = []string{"POST /internal/v1/pursuit/pursuits", "GET /internal/v1/pursuit/tasks/{taskID}",
		"POST /internal/v1/pursuit/tasks/{taskID}/complete", "POST /internal/v1/pursuit/tasks/{taskID}/reopen",
		// C-09：简报“待我处理”中的任务与授权被拒通知的再检查（以收件人本人）
		"GET /internal/v1/pursuit/my-tasks", "GET /internal/v1/pursuit/pursuits/{pursuitID}"}
)

// Module 返回 pursuit 的模块定义。配置：MIAOQIA_PURSUIT_SIGNING_KEY（翻页游标签名密钥，base64，至少 32 字节，同一部署的全部实例相同；
// 只有 development 可缺省）、MIAOQIA_PURSUIT_PACKS_DIR（应用领域包目录，默认 packs）。
//
// 依赖的绑定：identity 的授权决策点、目录（参与关系同步）与会话核验，customer-context 的 ObjectiveScope；detection 的信号卡片
// （从信号创建时核对）只在服务对外接口的 api 角色上必需，其他角色（consumer、workflow-worker）不服务创建，缺少时从信号创建失败关闭为 503。
func Module() runtime.Module { return ModuleWith(Adapters{}) }

// Adapters 是 pursuit 的业务时钟（零值为系统时钟；测试使用可控时钟）。
type Adapters struct {
	Now func() time.Time
}

// ModuleWith 返回使用给定适配器的 pursuit 模块定义。
func ModuleWith(adapters Adapters) runtime.Module {
	svc := &app.Service{Now: adapters.Now}
	return runtime.Module{
		Name:       api.Name,
		Database:   "pur",
		Migrations: migrations.FS,
		Start: func(ctx context.Context, env *runtime.Env) error {
			a, ok := runtime.Lookup[identity.Authorizer](env.Bindings, identity.AuthorizerBinding)
			if !ok {
				return fmt.Errorf("pursuit：缺少 %s 绑定", identity.AuthorizerBinding)
			}
			directory, ok := runtime.Lookup[identity.Directory](env.Bindings, identity.DirectoryBinding)
			if !ok {
				return fmt.Errorf("pursuit：缺少 %s 绑定", identity.DirectoryBinding)
			}
			members, ok := runtime.Lookup[identity.SessionResolver](env.Bindings, identity.SessionBinding)
			if !ok {
				return fmt.Errorf("pursuit：缺少 %s 绑定", identity.SessionBinding)
			}
			scope, ok := runtime.Lookup[customercontext.ObjectiveScope](env.Bindings, customercontext.ScopeBinding)
			if !ok {
				return fmt.Errorf("pursuit：缺少 %s 绑定", customercontext.ScopeBinding)
			}
			signals, ok := runtime.Lookup[detection.Briefing](env.Bindings, detection.BriefingBinding)
			if !ok && env.Role == "api" {
				return fmt.Errorf("pursuit：缺少 %s 绑定", detection.BriefingBinding)
			}
			key, err := signingKey(env)
			if err != nil {
				return err
			}
			dir := env.Config("PACKS_DIR")
			if dir == "" {
				dir = "packs"
			}
			outcomes, err := app.LoadOutcomes(dir)
			if err != nil {
				return err
			}
			svc.DB, svc.Authz, svc.Directory, svc.Members, svc.Scope, svc.Signals = env.DB, a, directory, members, scope, signals
			svc.Key, svc.Outcomes, svc.Log = key, outcomes, env.Log
			env.Bindings.Provide(api.ActionsBinding, api.LocalActions(app.Actions{Service: svc}))
			env.Bindings.Provide(api.RemindersBinding, api.LocalReminders(svc))
			return nil
		},
		Jobs: jobs(clock(adapters.Now)),
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			handler := &app.Server{Service: svc}
			in, err := pur.NewServer(handler, pur.WithPathPrefix("/internal"), pur.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range internalRoutes {
				internal.Handle(pattern, binding.InternalServer(api.Name, app.InternalOnly(in)))
			}
			pub, err := pur.NewServer(handler, pur.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range publicRoutes {
				public.Handle(pattern, pub)
			}
		},
		// 授权同步的重试（本子系统的 team.changed，按跟进当前的期望关系与版本）；成员停用或退出（identity 的平台域主题）与目标参与人变化
		// （customer-context）后重新判断负责人与协作者。
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{
				{
					Name: "pursuit.authorization-sync", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
					Topics:   []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "pursuit")},
					Handlers: map[string]consumer.Handler{app.TeamChanged: svc.TeamChangedEvent},
				},
				{
					Name: "pursuit.member-changes", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
					Topics:   []string{outbox.Topic(reqctx.ZonePlatform, env.Cell, "identity")},
					Handlers: map[string]consumer.Handler{"identity.member.changed.v1": svc.MemberChanged},
				},
				{
					Name: "pursuit.objective-changes", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
					Topics:   []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "customer_context")},
					Handlers: map[string]consumer.Handler{"customer_context.objective.changed.v1": svc.ObjectiveChanged},
				},
			}
		},
	}
}

// signingKey 读取 MIAOQIA_PURSUIT_SIGNING_KEY（base64，解码后至少 32 字节）；只有 development 可以缺省，此时每个进程随机生成
// （多进程开发环境中，一个进程签发的游标在另一个进程不被接受，失败关闭为 400 invalid_cursor）。
func signingKey(env *runtime.Env) ([]byte, error) {
	v := strings.TrimSpace(env.Config("SIGNING_KEY"))
	if v == "" {
		if !transportsecurity.Development() {
			return nil, fmt.Errorf("pursuit：缺少 MIAOQIA_PURSUIT_SIGNING_KEY（翻页游标签名密钥）")
		}
		key := make([]byte, app.MinSigningKey)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if env.Log != nil {
			env.Log.Warn("pursuit：未配置 MIAOQIA_PURSUIT_SIGNING_KEY，开发环境使用进程内随机密钥")
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
	return nil, fmt.Errorf("pursuit：MIAOQIA_PURSUIT_SIGNING_KEY 须为 base64 编码、至少 %d 字节", app.MinSigningKey)
}
