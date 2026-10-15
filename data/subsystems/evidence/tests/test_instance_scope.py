"""租户文档组装不能订阅公共探针。"""
from miaoqia.kernel import runtime

def test_tenant_document_instance_has_no_public_probe_consumer():
    from miaoqia.evidence import module
    env = runtime.Env(role="tenant-document-worker", brokers="unused", db=None,
                      subsystem="evidence", data_scope="tenant", cell="cell-01")
    assert module().consumers(env) == []
