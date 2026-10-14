from __future__ import annotations

from unittest.mock import Mock

import pytest

from miaoqia.kernel import consumer, outbox


@pytest.fixture
def message():
    msg = Mock()
    msg.value.return_value = b"invalid JSON"
    msg.topic.return_value = "public.test"
    msg.offset.return_value = 1
    msg.headers.return_value = None
    msg.key.return_value = b"key"
    msg.error.return_value = None
    return msg


def make_consumer():
    return consumer.Consumer(name="test.consumer", subsystem="test", brokers="unused", topics=["public.test"],
                             handlers={}, db=Mock())


@pytest.mark.parametrize("delivery", ["success", "timeout", "error", "no_callback"])
def test_commit_only_after_dead_letter_delivery(monkeypatch, message, delivery):
    c = make_consumer()
    kc, prod = Mock(), Mock()
    kc.poll.return_value = message
    kc.commit.side_effect = lambda **_: c.stop()
    on_delivery = None

    def produce(*args, **kwargs):
        nonlocal on_delivery
        on_delivery = kwargs.get("on_delivery")

    def flush(_timeout):
        if delivery in ("success", "error") and on_delivery is not None:
            on_delivery("delivery failed" if delivery == "error" else None, message)
        return 1 if delivery == "timeout" else 0

    prod.produce.side_effect = produce
    prod.flush.side_effect = flush
    monkeypatch.setattr(consumer, "KafkaConsumer", lambda _: kc)
    monkeypatch.setattr(consumer, "Producer", lambda _: prod)
    if delivery == "success":
        c.run()
        kc.commit.assert_called_once_with(message=message, asynchronous=False)
    else:
        with pytest.raises((RuntimeError, TimeoutError)):
            c.run()
        kc.commit.assert_not_called()
    kc.close.assert_called_once()


@pytest.mark.parametrize("payload", [b"null", b"[]", b'"text"', b"42", b"true",
                                     b'{"specversion":"1.0","id":"e","source":"s","type":["t"],"miaoqiazone":"public"}'])
def test_invalid_envelope_goes_to_dead_letter(message, payload):
    c = make_consumer()
    message.value.return_value = payload
    c._dead_letter = Mock()
    c._handle(message, Mock(), "dlq.test")
    c._dead_letter.assert_called_once()
    assert isinstance(c._dead_letter.call_args.args[-1], outbox.InvalidEnvelope)
    c.db.tx.assert_not_called()
