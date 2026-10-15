"""OpenTelemetry 追踪与 W3C traceparent 传递（与 services/kernel/tracing 对等）。"""
from __future__ import annotations

import os

from opentelemetry import context as otel_context
from opentelemetry import trace
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

_prop = TraceContextTextMapPropagator()
_provider: TracerProvider | None = None


def setup(service: str, role: str) -> None:
    """设置了 OTEL_EXPORTER_OTLP_ENDPOINT 时以 OTLP/gRPC 导出，否则只在进程内传递上下文。"""
    global _provider
    _provider = TracerProvider(resource=Resource.create({"service.name": service, "service.instance.id": role}))
    if os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT"):
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
        _provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(_provider)


def shutdown() -> None:
    if _provider:
        _provider.shutdown()


def tracer(name: str) -> trace.Tracer:
    return trace.get_tracer(f"miaoqia/{name}")


def traceparent() -> str:
    carrier: dict[str, str] = {}
    _prop.inject(carrier)
    return carrier.get("traceparent", "")


def context_from(traceparent_value: str) -> otel_context.Context:
    if not traceparent_value:
        return otel_context.get_current()
    return _prop.extract({"traceparent": traceparent_value})
