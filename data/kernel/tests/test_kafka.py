from __future__ import annotations

import importlib
import traceback
from pathlib import Path
from unittest.mock import Mock, call

import pytest
from confluent_kafka import KafkaError, KafkaException

from miaoqia.kernel import consumer, kafka

CA = "MIAOQIA_KAFKA_TLS_CA"
USERNAME = "MIAOQIA_KAFKA_SASL_USERNAME"
PASSWORD = "MIAOQIA_KAFKA_SASL_PASSWORD"
MECHANISM = "MIAOQIA_KAFKA_SASL_MECHANISM"
USER_VALUE = "synthetic kafka user "
PASSWORD_VALUE = " synthetic kafka password"


@pytest.fixture(autouse=True)
def clean_kafka_environment(monkeypatch):
    for key in ("MIAOQIA_KAFKA_BROKERS", CA, USERNAME, PASSWORD, MECHANISM):
        monkeypatch.delenv(key, raising=False)


@pytest.fixture
def provision(monkeypatch):
    monkeypatch.syspath_prepend(str(Path(__file__).resolve().parents[3] / "tools" / "dev"))
    return importlib.import_module("ensure_topics")


def consumer_instance(brokers="one.fixture:9092,two.fixture:9092"):
    instance = consumer.Consumer(name="test.consumer", subsystem="test", brokers=brokers,
                                 topics=["public.test"], handlers={}, db=Mock())
    instance.stop()
    return instance


@pytest.mark.parametrize("protocol, mechanism", [
    (None, None), ("SSL", None), ("SASL_SSL", "SCRAM-SHA-256"), ("SASL_SSL", "SCRAM-SHA-512"),
])
def test_entrypoints_share_connection_settings(monkeypatch, provision, protocol, mechanism):
    native_consumer = Mock()
    native_producer = Mock()
    native_admin = Mock()
    connection_config = Mock(wraps=kafka.connection_config)
    monkeypatch.setattr(kafka, "connection_config", connection_config)
    future = Mock()
    if protocol == "SSL":
        future.result.side_effect = KafkaException(KafkaError(KafkaError.TOPIC_ALREADY_EXISTS))
    native_admin.return_value.create_topics.return_value = {"public.test": future}
    monkeypatch.setattr(consumer, "KafkaConsumer", native_consumer)
    monkeypatch.setattr(consumer, "Producer", native_producer)
    monkeypatch.setattr(provision, "AdminClient", native_admin)
    monkeypatch.setattr(provision, "topics", lambda: ["public.test"])
    # These librdkafka options are never taken from the environment.
    monkeypatch.setenv("MIAOQIA_KAFKA_SECURITY_PROTOCOL", "SASL_PLAINTEXT")
    monkeypatch.setenv("MIAOQIA_KAFKA_ENABLE_SSL_CERTIFICATE_VERIFICATION", "false")

    settings = {}
    if protocol is not None:
        monkeypatch.setenv("MIAOQIA_KAFKA_BROKERS", "admin.fixture:9093")
        monkeypatch.setenv(CA, "/fixture/ca.pem")
        settings.update({
            "security.protocol": protocol,
            "ssl.ca.location": "/fixture/ca.pem",
            "enable.ssl.certificate.verification": True,
            "ssl.endpoint.identification.algorithm": "https",
            "log_level": 0,
        })
    if mechanism is not None:
        monkeypatch.setenv(USERNAME, USER_VALUE)
        monkeypatch.setenv(PASSWORD, PASSWORD_VALUE)
        monkeypatch.setenv(MECHANISM, mechanism)
        settings.update({"sasl.username": USER_VALUE, "sasl.password": PASSWORD_VALUE, "sasl.mechanism": mechanism})

    instance = consumer_instance()
    instance.run()
    provision.main()

    settings["bootstrap.servers"] = instance.brokers
    native_consumer.assert_called_once_with({**settings, "group.id": instance.name, "enable.auto.commit": False,
                                             "auto.offset.reset": "earliest"})
    native_producer.assert_called_once_with({**settings, "enable.idempotence": True})
    native_consumer.return_value.subscribe.assert_called_once_with(instance.topics)
    native_consumer.return_value.close.assert_called_once_with()
    native_producer.return_value.flush.assert_called_once_with(10)
    admin_brokers = "admin.fixture:9093" if protocol is not None else "localhost:9092"
    assert connection_config.call_args_list == [call(instance.brokers), call(admin_brokers)]
    settings["bootstrap.servers"] = admin_brokers
    native_admin.assert_called_once_with(settings)
    requested, = native_admin.return_value.create_topics.call_args.args[0]
    assert (requested.topic, requested.num_partitions, requested.replication_factor) == ("public.test", 12, 1)
    assert native_admin.return_value.create_topics.call_args.kwargs == {"request_timeout": 30}
    future.result.assert_called_once_with()


@pytest.mark.parametrize("settings, reason", [
    ({CA: ""}, "TLS"),
    ({USERNAME: ""}, "TLS"),
    ({PASSWORD: ""}, "TLS"),
    ({MECHANISM: ""}, "TLS"),
    ({USERNAME: USER_VALUE, PASSWORD: PASSWORD_VALUE, MECHANISM: "SCRAM-SHA-256"}, "TLS"),
    ({CA: "", USERNAME: USER_VALUE, PASSWORD: PASSWORD_VALUE, MECHANISM: "SCRAM-SHA-256"}, "TLS"),
    ({CA: "ca.pem", PASSWORD: PASSWORD_VALUE, MECHANISM: "SCRAM-SHA-256"}, "nonempty"),
    ({CA: "ca.pem", USERNAME: USER_VALUE, MECHANISM: "SCRAM-SHA-256"}, "nonempty"),
    ({CA: "ca.pem", USERNAME: USER_VALUE, PASSWORD: PASSWORD_VALUE}, "nonempty"),
    ({CA: "ca.pem", USERNAME: "", PASSWORD: PASSWORD_VALUE, MECHANISM: "SCRAM-SHA-256"}, "nonempty"),
    ({CA: "ca.pem", USERNAME: USER_VALUE, PASSWORD: "", MECHANISM: "SCRAM-SHA-256"}, "nonempty"),
    ({CA: "ca.pem", USERNAME: USER_VALUE, PASSWORD: PASSWORD_VALUE, MECHANISM: ""}, "nonempty"),
    ({CA: "ca.pem", USERNAME: USER_VALUE, PASSWORD: PASSWORD_VALUE, MECHANISM: "PLAIN"}, "mechanism"),
    ({CA: "ca.pem", USERNAME: USER_VALUE, PASSWORD: PASSWORD_VALUE, MECHANISM: PASSWORD_VALUE}, "mechanism"),
])
def test_invalid_settings_fail_before_constructing_clients(monkeypatch, provision, settings, reason, capfd, caplog):
    for name, value in settings.items():
        monkeypatch.setenv(name, value)
    native_consumer, native_producer, native_admin = Mock(), Mock(), Mock()
    monkeypatch.setattr(consumer, "KafkaConsumer", native_consumer)
    monkeypatch.setattr(consumer, "Producer", native_producer)
    monkeypatch.setattr(provision, "AdminClient", native_admin)

    for entrypoint in (consumer_instance().run, provision.main):
        with pytest.raises(ValueError, match=reason) as exc:
            entrypoint()
        assert USER_VALUE.strip() not in str(exc.value)
        assert PASSWORD_VALUE.strip() not in str(exc.value)
    for client in (native_consumer, native_producer, native_admin):
        client.assert_not_called()
    assert capfd.readouterr() == ("", "")
    assert caplog.text == ""


@pytest.mark.parametrize("entrypoint", ["consumer", "producer", "admin"])
@pytest.mark.parametrize("ca_file", ["missing.pem", "probe"])
def test_invalid_ca_fails_with_real_librdkafka(monkeypatch, provision, tmp_path, capfd, entrypoint, ca_file):
    # With no bootstrap address, even an unexpected success cannot reach a broker.
    monkeypatch.setenv("MIAOQIA_KAFKA_BROKERS", "")
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv(CA, ca_file)
    monkeypatch.setenv(USERNAME, USER_VALUE)
    monkeypatch.setenv(PASSWORD, PASSWORD_VALUE)
    monkeypatch.setenv(MECHANISM, "SCRAM-SHA-512")
    native_consumer = Mock()
    if entrypoint == "producer":
        monkeypatch.setattr(consumer, "KafkaConsumer", native_consumer)

    run = provision.main if entrypoint == "admin" else consumer_instance(brokers="").run
    with pytest.raises(RuntimeError, match="Kafka client initialization failed"):
        run()
    if entrypoint == "producer":
        native_consumer.return_value.close.assert_called_once_with()
    assert capfd.readouterr() == ("", "")


def test_client_configuration_error_suppresses_native_exception(monkeypatch, provision, capfd):
    monkeypatch.setenv(CA, "/fixture/ca.pem")
    monkeypatch.setenv(USERNAME, USER_VALUE)
    monkeypatch.setenv(PASSWORD, PASSWORD_VALUE)
    monkeypatch.setenv(MECHANISM, "SCRAM-SHA-256")
    error = KafkaException(KafkaError(KafkaError._INVALID_ARG, f"{USER_VALUE} {PASSWORD_VALUE}"))
    monkeypatch.setattr(provision, "AdminClient", Mock(side_effect=error))

    with pytest.raises(RuntimeError, match="Kafka client initialization failed") as exc:
        provision.main()
    diagnostic = "".join(traceback.format_exception(exc.value))
    assert USER_VALUE.strip() not in diagnostic
    assert PASSWORD_VALUE.strip() not in diagnostic
    assert capfd.readouterr() == ("", "")
