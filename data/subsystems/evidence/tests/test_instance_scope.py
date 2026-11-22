"""Evidence consumer wiring by deployment role and scope."""
import pytest

from miaoqia.evidence import module
from miaoqia.evidence.application.live import BROWSER_NOTICE_ARCHIVED, GROUPED_NOTICE_ARCHIVED, NOTICE_ARCHIVED
from miaoqia.kernel import runtime


def test_tenant_document_instance_has_no_public_probe_consumer():
    env = runtime.Env(role="tenant-document-worker", brokers="unused", db=None,
                      subsystem="evidence", data_scope="tenant", cell="cell-01")
    assert module().consumers(env) == []


@pytest.mark.parametrize("role", ["data-api", "data-consumer", "document-worker", "data-workflow-worker"])
def test_live_consumer_only_runs_in_data_consumer(role, tmp_path, monkeypatch):
    monkeypatch.setenv("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR", str(tmp_path))
    monkeypatch.setenv("MIAOQIA_PACKS_DIR", str(tmp_path / "empty-packs"))
    env = runtime.Env(role=role, brokers="unused", db=None, subsystem="evidence", data_scope="public")
    expected = ["evidence.probe", "evidence.live"] if role == "data-consumer" else ["evidence.probe"]
    consumers = module().consumers(env)
    assert [consumer.name for consumer in consumers] == expected
    if role == "data-consumer":
        assert set(consumers[1].handlers) == {NOTICE_ARCHIVED, GROUPED_NOTICE_ARCHIVED, BROWSER_NOTICE_ARCHIVED}
