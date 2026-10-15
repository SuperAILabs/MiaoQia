// Package gateway 组装接入网关（REST/OpenAPI、BFF、MCP、Webhook、限流）。M0 提供令牌校验与对外路由。
package gateway

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/tracing"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/internal/auth"
)

// Public 包装对外接口：校验令牌、派生请求上下文、开启服务端追踪。issuer 为 Keycloak realm 地址。
func Public(ctx context.Context, issuer, audience string, h http.Handler) (http.Handler, error) {
	v, err := auth.NewVerifier(ctx, issuer, audience)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, err := v.Context(r)
		if err != nil {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"https://errors.miaoqia.com/unauthenticated","title":"Unauthorized","status":401,"code":"unauthenticated"}`))
			return
		}
		ctx, span := tracing.Tracer(api.Name).Start(reqctx.With(r.Context(), rc), r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		r = r.Clone(ctx)
		r.Header.Del(reqctx.Header)
		h.ServeHTTP(w, r)
	}), nil
}

// Module 返回 gateway 的模块定义（对外路由的包装见 Public）。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "gwy"}
}
