"""事件消费框架（与 Go 内核语义一致）：校验信封与主题，在子系统数据库事务中以事件 id 去重并处理，
失败按退避重试，超过上限转入死信主题 dlq.<子系统>。"""
from __future__ import annotations

import json
import logging
import time
from dataclasses import dataclass, field
from typing import Callable

from confluent_kafka import Consumer as KafkaConsumer
from confluent_kafka import KafkaError, Message, Producer
from opentelemetry import trace
from sqlalchemy import Connection, text

from . import outbox, reqctx, tracing
from .db import Database

log = logging.getLogger("miaoqia.kernel.consumer")

Handler = Callable[[Connection, dict], None]


def check_envelope(topic: str, env: dict) -> None:
    outbox.validate(env)
    prefix = topic.split(".", 1)[0]
    zone, cell = env.get("miaoqiazone"), env.get("miaoqiacell", "")
    if ((prefix == "public" and zone != reqctx.PUBLIC) or (prefix == "platform" and zone != reqctx.PLATFORM)
            or (prefix.startswith("cell-") and (zone != reqctx.TENANT or f"cell-{cell}" != prefix))):
        raise outbox.InvalidEnvelope(f"事件 {env.get('id')} 的域 {zone} / 单元 {cell!r} 与主题 {topic} 不符")


@dataclass
class Consumer:
    name: str                     # 消费者标识，同时作为消费组与去重键
    subsystem: str
    brokers: str
    topics: list[str]
    handlers: dict[str, Handler]  # 按事件类型；未登记的类型直接确认
    db: Database
    dead_letter: str = ""
    max_attempts: int = 5
    backoff: float = 0.2
    _stop: bool = field(default=False, init=False)

    def stop(self) -> None:
        self._stop = True

    def run(self) -> None:
        dlq = self.dead_letter or f"dlq.{self.subsystem}"
        kc = KafkaConsumer({"bootstrap.servers": self.brokers, "group.id": self.name, "enable.auto.commit": False,
                            "auto.offset.reset": "earliest"})
        prod = Producer({"bootstrap.servers": self.brokers, "enable.idempotence": True})
        kc.subscribe(self.topics)
        try:
            while not self._stop:
                msg = kc.poll(0.2)
                if msg is None:
                    continue
                if msg.error():
                    if msg.error().code() != KafkaError._PARTITION_EOF:
                        log.warning("拉取失败 consumer=%s error=%s", self.name, msg.error())
                    continue
                self._handle(msg, prod, dlq)
                kc.commit(message=msg, asynchronous=False)
        finally:
            kc.close()
            prod.flush(10)

    def _handle(self, msg: Message, prod: Producer, dlq: str) -> None:
        try:
            env = json.loads(msg.value())
            check_envelope(msg.topic(), env)
        except (ValueError, outbox.InvalidEnvelope) as e:
            return self._dead_letter(prod, dlq, msg, e)
        h = self.handlers.get(env["type"])
        if h is None:
            return
        ctx = reqctx.service(self.subsystem, reqctx.Context(principal=reqctx.Principal("", ""), tenant=env.get("miaoqiatenant", ""),
                                                             cell=env.get("miaoqiacell", ""), purpose="event"))
        span_ctx = tracing.context_from(env.get("traceparent", ""))
        with reqctx.use(ctx), tracing.tracer(self.subsystem).start_as_current_span(
                f"consume {env['type']}", context=span_ctx, kind=trace.SpanKind.CONSUMER,
                attributes={"messaging.destination.name": msg.topic(), "cloudevents.event_id": env["id"],
                            "cloudevents.event_subject": env.get("subject", ""),
                            "messaging.consumer.group.name": self.name}) as span:
            last: Exception | None = None
            for attempt in range(1, self.max_attempts + 1):
                try:
                    with self.db.tx() as conn:
                        res = conn.execute(text("INSERT INTO kernel_inbox (consumer, event_id) VALUES (:c, :e) ON CONFLICT DO NOTHING"),
                                           {"c": self.name, "e": env["id"]})
                        if res.rowcount == 0:
                            span.set_attribute("miaoqia.duplicate", True)
                            return
                        h(conn, env)
                    return
                except Exception as e:  # noqa: BLE001 — 处理失败一律重试，超过上限转死信
                    last = e
                    log.warning("事件处理失败，重试 consumer=%s event=%s attempt=%d error=%s", self.name, env["id"], attempt, e)
                    time.sleep(self.backoff * (2 ** (attempt - 1)))
            span.set_status(trace.Status(trace.StatusCode.ERROR, str(last)))
            self._dead_letter(prod, dlq, msg, last)

    def _dead_letter(self, prod: Producer, dlq: str, msg: Message, cause: Exception | None) -> None:
        log.error("事件转入死信主题 consumer=%s topic=%s offset=%s error=%s", self.name, msg.topic(), msg.offset(), cause)
        headers = list(msg.headers() or []) + [("dlq_consumer", self.name.encode()), ("dlq_source_topic", msg.topic().encode()),
                                               ("dlq_error", str(cause).encode())]
        prod.produce(dlq, value=msg.value(), key=msg.key(), headers=headers)
        prod.flush(10)
