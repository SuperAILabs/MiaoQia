"""Isolated test CA and SPIFFE peer certificates (openssl CLI).

For tests and local replays only; never use these for deployed services.
"""
from __future__ import annotations

from pathlib import Path
import subprocess

from .internal_http import TLS

_EXTENSIONS = (
    "\nextendedKeyUsage=serverAuth,clientAuth\nbasicConstraints=critical,CA:FALSE"
    "\nkeyUsage=critical,digitalSignature,keyEncipherment\nsubjectKeyIdentifier=hash"
    "\nauthorityKeyIdentifier=keyid,issuer\n"
)


class TestCA:
    __test__ = False  # not a pytest test class

    def __init__(self, directory: Path, *, days: str = "2"):
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self._openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key",
                      "-out", "ca.crt", "-days", days, "-subj", "/CN=Tests",
                      "-addext", "basicConstraints=critical,CA:TRUE",
                      "-addext", "keyUsage=critical,keyCertSign,cRLSign")

    def _openssl(self, *args):
        subprocess.run(["openssl", *args], cwd=self.directory, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    def peer(self, name: str, uri: str | None = None, *, extra_uri: str | None = None, days: str = "1") -> TLS:
        """Issue a localhost/127.0.0.1 certificate, optionally with SPIFFE URI SANs."""
        self._openssl("req", "-newkey", "rsa:2048", "-nodes", "-keyout", name + ".key",
                      "-out", name + ".csr", "-subj", "/CN=localhost")
        sans = ["DNS:localhost", "IP:127.0.0.1"] + ["URI:" + u for u in (uri, extra_uri) if u]
        (self.directory / (name + ".ext")).write_text("subjectAltName=" + ",".join(sans) + _EXTENSIONS)
        self._openssl("x509", "-req", "-in", name + ".csr", "-CA", "ca.crt", "-CAkey", "ca.key",
                      "-CAcreateserial", "-out", name + ".crt", "-days", "0" if days == "-1" else days,
                      "-extfile", name + ".ext")
        return TLS(str(self.directory / (name + ".crt")), str(self.directory / (name + ".key")),
                   str(self.directory / "ca.crt"))
