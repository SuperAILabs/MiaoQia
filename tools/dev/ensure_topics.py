"""Create the contract-derived Kafka topics on the configured broker."""
from __future__ import annotations

import os
import sys
from pathlib import Path

from confluent_kafka import KafkaError, KafkaException
from confluent_kafka.admin import AdminClient, NewTopic

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools/dev"))
from gen_local import topics  # noqa: E402


def main() -> None:
    brokers = os.environ.get("MIAOQIA_KAFKA_BROKERS", "localhost:9092")
    client = AdminClient({"bootstrap.servers": brokers})
    requested = topics()
    futures = client.create_topics([NewTopic(name, 12, 1) for name in requested], request_timeout=30)
    for future in futures.values():
        try:
            future.result()
        except KafkaException as exc:
            if exc.args[0].code() != KafkaError.TOPIC_ALREADY_EXISTS:
                raise
    print(f"Kafka 主题：{len(requested)} 个")


if __name__ == "__main__":
    main()
