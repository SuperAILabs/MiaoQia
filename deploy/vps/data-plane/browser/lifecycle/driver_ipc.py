"""Launch the fixed Playwright stdio driver inside the synthetic browser container."""

import base64
from importlib.metadata import version
import os
from pathlib import Path
import subprocess
import sys

import playwright

from chromium_home import chromium_environment


def main():
    if sys.argv[1:] or version("playwright") != "1.60.0":
        raise ValueError("unexpected fixture launch")
    encoded = os.environ["MQ_FIXTURE_TEST_CA"]
    if not 0 < len(encoded) <= 8192:
        raise ValueError("invalid fixture CA length")
    ca = base64.b64decode(encoded, validate=True)
    driver = Path(playwright.__file__).resolve().parent / "driver"
    with chromium_environment(ca) as clean:
        clean.update(PLAYWRIGHT_BROWSERS_PATH="/ms-playwright", PW_LANG_NAME="python")
        return subprocess.run([str(driver / "node"), str(driver / "package/cli.js"), "run-driver"],
                              env=clean, timeout=30).returncode


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception:
        print("fixture driver unavailable", file=sys.stderr)
        raise SystemExit(1) from None
