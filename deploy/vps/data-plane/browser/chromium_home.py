"""Disposable trust and home directory for the synthetic Chromium publisher."""

from contextlib import contextmanager
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory


@contextmanager
def chromium_environment(ca, *, trust=True):
    with TemporaryDirectory(dir="/tmp") as home:
        clean = {"HOME": home, "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "TMPDIR": "/tmp"}
        nss = Path(home) / ".local/share/pki/nssdb"
        nss.mkdir(parents=True)
        # Resolve both Chromium generations to the same empty NSS database.
        legacy = Path(home) / ".pki"
        legacy.mkdir()
        (legacy / "nssdb").symlink_to(nss)
        cert = Path(home) / "fixture-ca.pem"
        cert.write_bytes(ca)
        certutil = "/opt/nss/usr/bin/certutil"
        subprocess.run([certutil, "-N", "-d", f"sql:{nss}", "--empty-password"],
                       env=clean, check=True, capture_output=True, timeout=3)
        if trust:
            subprocess.run([certutil, "-A", "-d", f"sql:{nss}", "-n", "fixture",
                            "-t", "C,,", "-i", str(cert)],
                           env=clean, check=True, capture_output=True, timeout=3)
        yield clean
