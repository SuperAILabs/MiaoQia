"""No reusable positive permits: confirmation binds every input and caller and rechecks time/current rights."""

from __future__ import annotations

import hashlib
import hmac
import json
import secrets

from miaoqia.acquisition.api import DISPLAYS, OPERATIONS, inputs, instant
from miaoqia.kernel import reqctx
from miaoqia.kernel.jsoncodec import closed


class Decisions:
    def __init__(self, registry, *, secret=None):
        self.registry = registry
        # Ephemeral response-integrity key; restart invalidates receipts, never restores permission.
        self.secret = secret or secrets.token_bytes(32)

    def _mac(self, context, request, epoch, expires, nonce):
        body = json.dumps(
            [context.encode(), request, epoch, expires, nonce],
            sort_keys=True,
            separators=(",", ":"),
        ).encode()
        return hmac.new(self.secret, body, hashlib.sha256).hexdigest()

    def evaluate(self, request, *, confirm=False):
        closed(
            request,
            {"operation", "inputs", "receipt"} if confirm else {"operation", "inputs"},
        )
        operation = request["operation"]
        if operation not in OPERATIONS:
            raise ValueError("unsupported operation")
        sources = inputs(request["inputs"])
        c = reqctx.current()
        # Current membership is independently checked at EVERY HTTP route before reaching here.
        if (
            c.principal.kind != "user"
            or not c.tenant
            or not c.cell
            or c.purpose != "interactive"
        ):
            raise PermissionError("invalid scope")
        original = {"operation": operation, "inputs": [s.dto() for s in sources]}
        with self.registry.snapshot() as snap:
            display, redistribute, allowed = "full_text", True, True
            selected = [(source, snap.versions(source)) for source in sources]
            now = snap.current_time()
            for source, versions in selected:
                if versions is None:
                    allowed = False
                    continue
                for p in versions:
                    if (
                        p["source_id"] != source.source_id
                        or {"tenant": c.tenant, "cell": c.cell} not in p["scopes"]
                        or c.purpose not in p["purposes"]
                        or not instant(p["effective_at"])
                        <= now
                        < instant(p["expires_at"])
                    ):
                        allowed = False
                    display = DISPLAYS[
                        min(DISPLAYS.index(display), DISPLAYS.index(p["display"]))
                    ]
                    redistribute = redistribute and p["redistribution"]
            allowed = allowed and display != "none"
            receipt = None
            if confirm:
                r = closed(request["receipt"], {"epoch", "expires", "nonce", "mac"})
                if (
                    type(r["epoch"]) is not int
                    or type(r["expires"]) is not int
                    or not isinstance(r["nonce"], str)
                    or len(r["nonce"]) != 32
                    or not isinstance(r["mac"], str)
                    or len(r["mac"]) != 64
                ):
                    raise ValueError("invalid receipt")
                expected = self._mac(c, original, r["epoch"], r["expires"], r["nonce"])
                allowed = (
                    allowed
                    and r["epoch"] == snap.epoch
                    and now.timestamp() < r["expires"]
                    and hmac.compare_digest(r["mac"], expected)
                )
            elif allowed:
                expires = int(now.timestamp()) + 30
                nonce = secrets.token_hex(16)
                receipt = {
                    "epoch": snap.epoch,
                    "expires": expires,
                    "nonce": nonce,
                    "mac": self._mac(c, original, snap.epoch, expires, nonce),
                }
            result = {
                "allowed": bool(allowed),
                "display": display if allowed else "none",
                "redistribution": bool(redistribute and allowed),
                "epoch": snap.epoch,
            }
            if receipt is not None:
                result["receipt"] = receipt
            return result

    def status(self):
        with self.registry.snapshot():
            return {
                "domain_service": "not_live",
                "capability": "frozen_source_rights_only",
            }
