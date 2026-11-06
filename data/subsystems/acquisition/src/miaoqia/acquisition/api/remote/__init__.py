"""Cross-unit rights client. Has no database dependency or acquisition DSN configuration."""

from miaoqia.kernel.internal_http import Client, Problem
from miaoqia.acquisition.api import decision


class RightsClient:
    def __init__(self, client: Client):
        self.client = client

    def _decision(self, path, request, deadline):
        try:
            return decision(
                self.client.request("POST", path, request, deadline=deadline)
            )
        except (ValueError, TypeError):
            raise Problem(503) from None

    def resolve(self, request, *, deadline=None):
        return self._decision("/source-rights/resolve", request, deadline)

    def confirm(self, request, *, deadline=None):
        return self._decision("/source-rights/confirm", request, deadline)

    def status(self, *, deadline=None):
        value = self.client.request(
            "POST", "/source-rights/status", {}, deadline=deadline
        )
        if value != {
            "domain_service": "not_live",
            "capability": "frozen_source_rights_only",
        }:
            raise Problem(503)
        return value
