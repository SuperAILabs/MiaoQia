// Package gateway 组装接入网关（REST/OpenAPI、BFF、MCP、Webhook、限流）。M0 提供令牌校验与对外路由。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/tracing"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/internal/auth"
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

// Module 返回 gateway 的模块定义（对外路由的包装见 Public）。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "gwy"}
}
