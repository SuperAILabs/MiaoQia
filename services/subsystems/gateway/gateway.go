// Package gateway 组装接入网关（REST/OpenAPI、BFF、MCP、Webhook、限流）。M0 提供令牌校验与对外路由；M2 第4段提供对象页与
// 证据面板的 BFF（docs/01-product/prd/04-object-page-evidence.md）：数据平面读取与权益覆盖尚未接入（api.NotWired，对象页 503），
// 测试与演示使用标明为夹具的内存实现（api/publictest）。
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	gw "github.com/superailabs/miaoqia/services/contracts/apis/gateway"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/tracing"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	commerceapi "github.com/superailabs/miaoqia/services/subsystems/commerce/api"
	ccapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detectionapi "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/internal/auth"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/internal/bff"
	identityapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Public 包装对外接口：校验令牌、派生请求上下文、开启服务端追踪。issuer 为 Keycloak realm 地址。
func Public(ctx context.Context, issuer, audience string, h http.Handler, resolvers ...identityapi.SessionResolver) (http.Handler, error) {
	v, err := auth.NewVerifier(ctx, issuer, audience)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, assurance, err := v.VerifyAssurance(r)
		if err != nil {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"https://errors.miaoqia.com/unauthenticated","title":"Unauthorized","status":401,"code":"unauthenticated"}`))
			return
		}
		requestCtx := reqctx.WithAssurance(reqctx.With(r.Context(), rc), assurance)
		ctx, span := tracing.Tracer(api.Name).Start(requestCtx, r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		requestCtx = ctx
		// 渠道免登会话令牌（PRD 第1段 F6、F7）：每次请求都经 identity 核验会话未作废、未过期、绑定版本高于撤销水位且成员有效。
		// 只有本地会话核验（identity.ChannelSessionResolver）能做这项核验；取不到时失败关闭，不退回普通会话核验。
		channel := assurance.Source == reqctx.SourceChannel
		if channel && (rc.Tenant == "" || assurance.ChannelSession == "") {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"https://errors.miaoqia.com/channel_session_invalid","title":"channel_session_invalid","status":401,"code":"channel_session_invalid"}`))
			return
		}
		if rc.Tenant != "" {
			var checkErr error
			if len(resolvers) == 0 || resolvers[0] == nil {
				checkErr = &identityapi.Error{Status: 503, Code: "identity_unavailable"}
			} else {
				var session *identityapi.Session
				var err error
				if channel {
					if cs, ok := resolvers[0].(identityapi.ChannelSessionResolver); ok {
						session, err = cs.ResolveChannelSession(requestCtx, assurance.ChannelSession)
					} else {
						err = &identityapi.Error{Status: 503, Code: "identity_unavailable"}
					}
				} else {
					session, err = resolvers[0].Resolve(requestCtx)
				}
				checkErr = err
				if err == nil && (session == nil || session.Validate() != nil || session.Tenant != rc.Tenant || string(session.Cell) != rc.Cell || session.Subject != rc.Principal.ID || session.State != "active") {
					checkErr = &identityapi.Error{Status: 403, Code: "invalid_tenant_context"}
				}
			}
			if checkErr != nil {
				status, code := 503, "identity_unavailable"
				var safe *identityapi.Error
				if errors.As(checkErr, &safe) {
					status, code = safe.Status, safe.Code
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://errors.miaoqia.com/" + code, "title": code, "status": status, "code": code})
				return
			}
		}
		r = r.Clone(ctx)
		r.Header.Del(reqctx.Header)
		h.ServeHTTP(w, r)
	}), nil
}

// 对外接口（经 Public 包装）的路由。
var publicRoutes = []string{
	"GET /v1/objects/{objectID}",
	"GET /v1/objects/{objectID}/sections/{section}",
	"GET /v1/evidence/{evidenceID}",
}

// Adapters 是对象页的出站端口与业务时钟。零值字段使用缺省：数据平面与证据为未接入（api.NotWired），覆盖判断为 commerce 的
// 覆盖判断绑定（本地或远程，启动时解析，缺失不启动），Now 为系统时钟。
type Adapters struct {
	Public       api.PublicData
	Evidence     api.EvidenceResolver
	Entitlements api.Entitlements
	Now          func() time.Time
}

// Module 返回 gateway 的模块定义（对外路由的包装见 Public；对象页适配器未接入）。
func Module() runtime.Module { return ModuleWith(Adapters{}) }

// ModuleWith 返回使用给定适配器的 gateway 模块定义。对象页依赖的跨子系统绑定在启动时全部解析：identity 的授权决策点、commerce 的
// 查看计量与覆盖判断（未显式给出 Entitlements 时）、detection 的信号固定求值输入与 customer-context 的上下文读取（本地或远程绑定），
// 缺一不启动。
func ModuleWith(adapters Adapters) runtime.Module {
	svc := &bff.Service{Public: adapters.Public, Evidence: adapters.Evidence, Entitlements: adapters.Entitlements, Now: adapters.Now}
	if svc.Public == nil {
		svc.Public = api.NotWired{}
	}
	if svc.Evidence == nil {
		svc.Evidence = api.NotWiredEvidence{}
	}
	coverageFromCommerce := svc.Entitlements == nil
	return runtime.Module{
		Name:     api.Name,
		Database: "gwy",
		Start: func(ctx context.Context, env *runtime.Env) error {
			a, ok := runtime.Lookup[identityapi.Authorizer](env.Bindings, identityapi.AuthorizerBinding)
			if !ok {
				return fmt.Errorf("gateway：缺少 %s 绑定", identityapi.AuthorizerBinding)
			}
			meter, ok := runtime.Lookup[commerceapi.ViewMetering](env.Bindings, commerceapi.MeteringBinding)
			if !ok {
				return fmt.Errorf("gateway：缺少 %s 绑定", commerceapi.MeteringBinding)
			}
			signals, ok := runtime.Lookup[detectionapi.SignalExplainer](env.Bindings, detectionapi.ExplainerBinding)
			if !ok {
				return fmt.Errorf("gateway：缺少 %s 绑定", detectionapi.ExplainerBinding)
			}
			reader, ok := runtime.Lookup[ccapi.ContextReader](env.Bindings, ccapi.ContextBinding)
			if !ok {
				return fmt.Errorf("gateway：缺少 %s 绑定", ccapi.ContextBinding)
			}
			if coverageFromCommerce {
				coverage, ok := runtime.Lookup[commerceapi.Coverage](env.Bindings, commerceapi.CoverageBinding)
				if !ok {
					return fmt.Errorf("gateway：缺少 %s 绑定", commerceapi.CoverageBinding)
				}
				svc.Entitlements = bff.CommerceCoverage{Coverage: coverage}
			}
			key, err := cursorKey(env)
			if err != nil {
				return err
			}
			svc.DB, svc.Authz, svc.Meter, svc.Signals, svc.Context, svc.Log, svc.CursorKey = env.DB, a, meter, signals, reader, env.Log, key
			return nil
		},
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			pub, err := gw.NewServer(&bff.Server{Service: svc}, gw.WithErrorHandler(bff.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range publicRoutes {
				public.Handle(pattern, pub)
			}
		},
	}
}

// cursorKey 读取分区游标的签名密钥 MIAOQIA_GATEWAY_CURSOR_KEY（base64，解码后至少 32 字节；同一部署的全部 api 实例必须相同，
// 否则跨实例翻页为 invalid_cursor）。缺省只在 MIAOQIA_ENV=development 时允许：每个进程随机生成，重启后已签发的游标失效；
// 其他环境缺少或无效时不启动。
func cursorKey(env *runtime.Env) ([]byte, error) {
	v := strings.TrimSpace(env.Config("CURSOR_KEY"))
	if v == "" {
		if !transportsecurity.Development() {
			return nil, fmt.Errorf("gateway：缺少 MIAOQIA_GATEWAY_CURSOR_KEY（分区游标签名密钥）")
		}
		key := make([]byte, bff.MinCursorKey)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if env.Log != nil {
			env.Log.Warn("gateway：未配置 MIAOQIA_GATEWAY_CURSOR_KEY，开发环境使用进程内随机密钥")
		}
		return key, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(v); err == nil {
			if len(key) < bff.MinCursorKey {
				break
			}
			return key, nil
		}
	}
	return nil, fmt.Errorf("gateway：MIAOQIA_GATEWAY_CURSOR_KEY 须为 base64 编码、至少 %d 字节", bff.MinCursorKey)
}
