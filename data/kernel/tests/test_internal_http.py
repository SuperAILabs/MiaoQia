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
