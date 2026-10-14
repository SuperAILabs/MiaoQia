// Package tracing 配置 OpenTelemetry 追踪，并在事件信封与内部调用之间传递 W3C 追踪上下文。
package tracing

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

var propagator = propagation.TraceContext{}

// Setup 安装全局追踪器。设置了 OTEL_EXPORTER_OTLP_ENDPOINT 时以 OTLP/gRPC 导出，否则只在进程内传递上下文。
// 返回的函数在进程退出前调用，刷新未导出的数据。
func Setup(ctx context.Context, service, role string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagator)
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	res := resource.NewSchemaless(semconv.ServiceName(service), semconv.ServiceInstanceID(role))
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Tracer 返回命名追踪器。
func Tracer(name string) trace.Tracer { return otel.Tracer("miaoqia/" + name) }

// Traceparent 返回 ctx 中当前 span 的 traceparent（无 span 时为空）。
func Traceparent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	propagator.Inject(ctx, c)
	return c["traceparent"]
}

// WithTraceparent 返回以 traceparent 为远端父 span 的 context。
func WithTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}
