// Package binding 是接口绑定的统一调用管线。同语言内子系统接口可绑定到本地实现或远程客户端，
// 两者都经过同一管线：上下文传递、超时、重试（仅幂等操作）、追踪（docs/03-architecture/09-codebase-structure.md 第 4 节）。
package binding

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/tracing"
)

// Mode 是绑定方式。
type Mode string

const (
	Local  Mode = "local"
	Remote Mode = "remote"
)

// Op 描述一个接口操作。
type Op struct {
	Subsystem  string
	Name       string
	Timeout    time.Duration // 默认 5s
	Idempotent bool          // 幂等操作才自动重试
}

// ErrTransient 标记可重试的错误（网络错误、5xx）。
var ErrTransient = errors.New("binding: 暂时性错误")

// Invoke 经调用管线执行一次接口调用。
func Invoke[T any](ctx context.Context, mode Mode, op Op, call func(context.Context) (T, error)) (T, error) {
	var zero T
	if _, ok := reqctx.From(ctx); !ok {
		return zero, reqctx.ErrMissing
	}
	if op.Timeout == 0 {
		op.Timeout = 5 * time.Second
	}
	ctx, span := tracing.Tracer(op.Subsystem).Start(ctx, op.Subsystem+"."+op.Name, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("miaoqia.binding", string(mode))))
	defer span.End()
	attempts := 1
	if op.Idempotent {
		attempts = 3
	}
	var err error
	for i := range attempts {
		cctx, cancel := context.WithTimeout(ctx, op.Timeout)
		var out T
		out, err = call(cctx)
		cancel()
		if err == nil {
			return out, nil
		}
		if !isTransient(err) || i == attempts-1 {
			break
		}
		time.Sleep(time.Duration(50*(1<<i)) * time.Millisecond)
	}
	span.SetStatus(codes.Error, err.Error())
	return zero, err
}

func isTransient(err error) bool {
	var ne net.Error
	return errors.Is(err, ErrTransient) || errors.As(err, &ne)
}

// Transport 是远程客户端的 HTTP 传输：附加请求上下文头与 traceparent。
type Transport struct {
	Base http.RoundTripper
}

func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	r = r.Clone(r.Context())
	if rc, ok := reqctx.From(r.Context()); ok {
		r.Header.Set(reqctx.Header, reqctx.Encode(rc))
	}
	if tp := tracing.Traceparent(r.Context()); tp != "" {
		r.Header.Set("traceparent", tp)
	}
	return base.RoundTrip(r)
}

// InternalServer 包装内部接口的服务端处理器：恢复请求上下文与追踪上下文。只用于内部监听端口。
func InternalServer(subsystem string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := tracing.WithTraceparent(r.Context(), r.Header.Get("traceparent"))
		raw := r.Header.Get(reqctx.Header)
		if raw == "" {
			http.Error(w, "缺少请求上下文", http.StatusUnauthorized)
			return
		}
		rc, err := reqctx.Decode(raw)
		if err != nil {
			http.Error(w, "请求上下文无效", http.StatusBadRequest)
			return
		}
		ctx, span := tracing.Tracer(subsystem).Start(reqctx.With(ctx, rc), subsystem+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}
