import pytest

from miaoqia.kernel.internal_http import Problem, peer


def test_internal_peer_accepts_exact_service_spiffe_identity():
    cert = {"subjectAltName": [("URI", "spiffe://miaoqia/service/domain-intelligence")]}
    assert peer(cert, {"spiffe://miaoqia/service/domain-intelligence"}) == cert["subjectAltName"][0][1]


@pytest.mark.parametrize("cert, allowed", [
    ({"subjectAltName": []}, {"spiffe://miaoqia/service/domain-intelligence"}),
    ({"subjectAltName": [("URI", "spiffe://miaoqia/service/other")]}, {"spiffe://miaoqia/service/domain-intelligence"}),
    ({"subjectAltName": [("URI", "spiffe://miaoqia/service/domain-intelligence"), ("URI", "spiffe://miaoqia/service/extra")]}, {"spiffe://miaoqia/service/domain-intelligence"}),
    ({"subjectAltName": [("DNS", "domain-intelligence")]}, {"spiffe://miaoqia/service/domain-intelligence"}),
])
def test_internal_peer_rejects_missing_or_unallowlisted_certificate(cert, allowed):
    with pytest.raises(Problem) as exc:
        peer(cert, allowed)
    assert exc.value.status == 403


from miaoqia.kernel.internal_http import admit, delegation, delegations
from miaoqia.kernel.reqctx import Context, Principal

DATA = "spiffe://miaoqia/service/data-api"
CONSUMER = "spiffe://miaoqia/service/platform-consumer"
ALLOWED = frozenset({
    (DATA, "data.get_fact", "interactive", None),
    (CONSUMER, "data.bid_result_units", "monitoring", "svc:detection"),
    (CONSUMER, "data.bid_result_units", "simulation", "svc:learning"),
})


def test_delegations_parse_interactive_and_named_service_entries():
    parsed = delegations([
        {"peer": DATA, "operation": "data.get_fact", "purpose": "interactive"},
        {"peer": CONSUMER, "operation": "data.bid_result_units", "purpose": "monitoring", "principal": "svc:detection"},
    ], {"data.get_fact", "data.bid_result_units"})
    assert parsed == [(DATA, "data.get_fact", "interactive", None),
                      (CONSUMER, "data.bid_result_units", "monitoring", "svc:detection")]


@pytest.mark.parametrize("item", [
    {"peer": DATA, "operation": "data.get_fact", "purpose": "monitoring"},
    {"peer": DATA, "operation": "data.get_fact", "purpose": "interactive", "principal": "svc:detection"},
    {"peer": DATA, "operation": "data.get_fact", "purpose": "simulation", "principal": "detection"},
    {"peer": DATA, "operation": "data.get_fact", "purpose": "simulation", "principal": "svc:"},
    {"peer": DATA, "operation": "data.get_fact", "purpose": "research"},
    {"peer": "spiffe://other/service/x", "operation": "data.get_fact", "purpose": "interactive"},
    {"peer": DATA, "operation": "data.unknown", "purpose": "interactive"},
    {"peer": DATA, "operation": "data.get_fact", "purpose": "interactive", "extra": 1},
])
def test_delegations_reject_ambiguous_entries(item):
    with pytest.raises(ValueError):
        delegations([item], {"data.get_fact"})
    with pytest.raises(ValueError):
        delegations([], None)


def test_admit_user_needs_identity_and_service_does_not():
    assert admit(Context(Principal("u", "user"), "tnt", "cell-01", "interactive"), DATA, "data.get_fact", ALLOWED) is True
    assert admit(Context(Principal("svc:detection", "service"), "tnt", "cell-01", "monitoring"),
                 CONSUMER, "data.bid_result_units", ALLOWED) is False


@pytest.mark.parametrize("context, peer, op", [
    # service principal cannot use the interactive delegation
    (Context(Principal("svc:detection", "service"), "tnt", "cell-01", "interactive"), DATA, "data.get_fact"),
    # user cannot use a service purpose
    (Context(Principal("svc:detection", "user"), "tnt", "cell-01", "monitoring"), CONSUMER, "data.bid_result_units"),
    # wrong service principal for the purpose
    (Context(Principal("svc:learning", "service"), "tnt", "cell-01", "monitoring"), CONSUMER, "data.bid_result_units"),
    (Context(Principal("svc:detection", "service"), "tnt", "cell-01", "simulation"), CONSUMER, "data.bid_result_units"),
    # service account is never delegated
    (Context(Principal("svc:detection", "service_account"), "tnt", "cell-01", "monitoring"), CONSUMER, "data.bid_result_units"),
    # tenant-less service read
    (Context(Principal("svc:detection", "service"), "", "", "monitoring"), CONSUMER, "data.bid_result_units"),
    # other operation or other peer
    (Context(Principal("svc:detection", "service"), "tnt", "cell-01", "monitoring"), CONSUMER, "data.get_fact"),
    (Context(Principal("svc:detection", "service"), "tnt", "cell-01", "monitoring"), DATA, "data.bid_result_units"),
    # unknown or missing purpose
    (Context(Principal("u", "user"), "tnt", "cell-01", "research"), DATA, "data.get_fact"),
    (Context(Principal("u", "user"), "tnt", "cell-01", ""), DATA, "data.get_fact"),
])
def test_admit_rejects_every_mismatch(context, peer, op):
    with pytest.raises(Problem) as exc:
        admit(context, peer, op, ALLOWED)
    assert exc.value.status == 403


def test_server_constructor_validates_four_part_delegations():
    from miaoqia.kernel.internal_http import Server, TLS
    for bad in ({(DATA, "data.get_fact", "monitoring")}, {(DATA, "data.get_fact", "interactive", "svc:x")}):
        with pytest.raises(ValueError):
            Server(("127.0.0.1", 0), TLS("", "", ""), bad, object(), {})
    assert delegation(CONSUMER, "op", "simulation", "svc:learning")[3] == "svc:learning"
