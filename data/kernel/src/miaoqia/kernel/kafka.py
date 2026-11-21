"""Kafka connection settings shared by the consumers and topic provisioning."""
from __future__ import annotations

import os
from collections.abc import Callable, Mapping
from pathlib import Path

from confluent_kafka import KafkaException

type KafkaConfig = dict[str, str | bool | int]


def connection_config(brokers: str, *, environ: Mapping[str, str] = os.environ) -> KafkaConfig:
    """Build librdkafka options using the caller's brokers and the Kafka security environment."""
    config: KafkaConfig = {"bootstrap.servers": brokers}
    ca = environ.get("MIAOQIA_KAFKA_TLS_CA")
    username = environ.get("MIAOQIA_KAFKA_SASL_USERNAME")
    password = environ.get("MIAOQIA_KAFKA_SASL_PASSWORD")
    mechanism = environ.get("MIAOQIA_KAFKA_SASL_MECHANISM")
    use_sasl = username is not None or password is not None or mechanism is not None
    if ca is None and not use_sasl:
        return config
    if not ca:
        raise ValueError("Kafka TLS requires a nonempty MIAOQIA_KAFKA_TLS_CA")

    config.update({
        "security.protocol": "SSL",
        # An absolute path prevents librdkafka from treating "probe" as system CA discovery.
        "ssl.ca.location": str(Path(ca).absolute()),
        "enable.ssl.certificate.verification": True,
        "ssl.endpoint.identification.algorithm": "https",
        # Keep native startup and authentication diagnostics out of stderr.
        "log_level": 0,
    })
    if use_sasl:
        if not username or not password or not mechanism:
            raise ValueError("Kafka SCRAM requires a nonempty username, password and mechanism")
        if mechanism not in ("SCRAM-SHA-256", "SCRAM-SHA-512"):
            raise ValueError("Kafka SCRAM mechanism must be SCRAM-SHA-256 or SCRAM-SHA-512")
        config.update({
            "security.protocol": "SASL_SSL",
            "sasl.mechanism": mechanism,
            "sasl.username": username,
            "sasl.password": password,
        })
    return config


def open_client[T](factory: Callable[[KafkaConfig], T], config: KafkaConfig) -> T:
    """Initialize a client without reporting native errors that may include configuration values."""
    try:
        return factory(config)
    except (KafkaException, ValueError):
        if "security.protocol" not in config:
            raise
        raise RuntimeError("Kafka client initialization failed. Check the TLS CA and Kafka settings.") from None
