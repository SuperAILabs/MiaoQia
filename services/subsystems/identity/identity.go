// Package identity 组装 identity 子系统（租户、成员、角色、授权决策、审批）。M0 提供授权决策、最小目录生命周期及cell核验。
package identity

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"

	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/identity/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/identity/internal/infra"
	"github.com/superailabs/miaoqia/services/subsystems/identity/migrations"
)

// Adapters 是 identity 出站端口的适配器与业务时钟。零值字段使用缺省：IdP 与 Notifier 为未接入（api.NotWired，
// 依赖它们的命令失败关闭），Now 为系统时钟。生产 Keycloak 管理接口与邮件渠道的适配器尚未实现。
type Adapters struct {
	IdP      api.IdentityProvider
	Notifier api.Notifier
	Now      func() time.Time
}

// Module 返回 identity 的模块定义（出站端口未接入）。配置：MIAOQIA_IDENTITY_OPENFGA_URL。
func Module() runtime.Module { return ModuleWith(Adapters{}) }

// ModuleWith 返回使用给定适配器的 identity 模块定义（测试使用内存实现，见 api/idptest）。
func ModuleWith(adapters Adapters) runtime.Module {
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
			directory = &app.Directory{DB: env.DB, Authorizer: d, FGA: d, IdP: adapters.IdP, Notifier: adapters.Notifier, Now: adapters.Now}
			decider = app.Decider{Store: d, Directory: directory}
			env.Bindings.Provide(api.AuthorizerBinding, api.Local(decider))
			env.Bindings.Provide(api.SessionBinding, api.LocalSession(directory))
			env.Bindings.Provide(api.DirectoryBinding, api.LocalDirectory(directory))
			env.Bindings.Provide(api.GuardBinding, api.LocalGuard(directory))
			env.Bindings.Provide(api.MaintenanceBinding, api.LocalMaintenance(directory))
			env.Bindings.Provide(api.ChannelSessionsBinding, api.LocalChannelSessions(directory))
			return nil
		},
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			handler := &app.Server{Decider: decider, Directory: directory}
			srv, err := identityapi.NewServer(handler, identityapi.WithPathPrefix("/internal"), identityapi.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range []string{"POST /internal/v1/authz:check", "GET /internal/v1/identity/session",
				"GET /internal/v1/identity/org-units/{unitID}", "POST /internal/v1/identity/participants:sync",
				"POST /internal/v1/identity/channel-sessions", "GET /internal/v1/identity/channel-sessions", "POST /internal/v1/identity/channel-sessions:revoke",
				"GET /internal/v1/identity/channel-members/{subject}"} {
				internal.Handle(pattern, binding.InternalServer(api.Name, srv))
			}
			pub, err := identityapi.NewServer(handler, identityapi.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range []string{"POST /v1/tenants", "GET /v1/tenants/{tenantID}", "GET /v1/identity/members/{subject}", "PUT /v1/tenants/{tenantID}/state", "PUT /v1/identity/members/{subject}", "GET /v1/identity/session",
				"GET /v1/identity/org-units", "POST /v1/identity/org-units", "GET /v1/identity/org-units/{unitID}", "PUT /v1/identity/org-units/{unitID}",
				"GET /v1/identity/invitations", "POST /v1/identity/invitations", "POST /v1/identity/invitations/{invitationID}/resend",
				"POST /v1/identity/invitations/{invitationID}/revoke", "POST /v1/identity/invitations:accept"} {
				public.Handle(pattern, pub)
			}
		},
		// 渠道绑定失效的撤销重试：消费 channels.binding.changed.v1（经 channels 的发件箱），按事件版本抬高撤销水位并作废免登会话。
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{{
				Name: "identity.channel-revocations", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
				Topics: []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "channels")},
				Handlers: map[string]consumer.Handler{"channels.binding.changed.v1": func(ctx context.Context, tx pgx.Tx, e *outbox.Envelope) error {
					return directory.ChannelBindingChanged(ctx, tx, e)
				}},
			}}
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
