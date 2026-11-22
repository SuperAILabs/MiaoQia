"""Trusted organization owner port for public search construction and replay."""

from collections.abc import Callable
from typing import TYPE_CHECKING, Protocol

from .organizations import RegistrationRef

if TYPE_CHECKING:
    from ..application.organizations import PublicOrganizations

MAX_BATCH = 32


class ProjectionUnavailable(RuntimeError):
    """A dependency, stale page, or budget prevented a confirmed export."""


class DiscoveryDecision(Protocol):
    @property
    def epoch(self) -> int: ...

    @property
    def approved(self) -> frozenset[str]: ...


class DiscoveryPolicy(Protocol):
    def resolve(self, source_ids: frozenset[str], *, deadline: float) -> DiscoveryDecision:
        """Resolve current source approvals in one authoritative view."""
        ...


class PublicRegistrationAdmission(Protocol):
    def __call__(self, ref: RegistrationRef, source_id: str, *, deadline: float) -> Callable[[], bool] | None:
        """Authorize the fixed registration and both evidence fields for the current caller.

        Require a complete, direct closure whose only source group is exactly
        (source_id, (f"public:{source_id}",)), with fixed dependencies and no
        reference bindings. Return None for a definitive ineligibility or
        denial. Raise on missing/unverifiable authority. The receipt reconfirms
        the same caller, operation, closure and public discovery/display grant.
        """
        ...


class PreparedMutations(Protocol):
    mutations: list[dict]
    next_after: str | None

    def confirm(self, *, deadline: float) -> None: ...


class OrganizationProjection(Protocol):
    def prepare_ids(self, ids: list[str] | tuple[str, ...], *, deadline: float) -> PreparedMutations: ...

    def prepare_registration(self, fact_id: str, *, deadline: float) -> PreparedMutations: ...

    def prepare_source(self, source_id: str, *, after: str = "", rights_version: int,
                       deadline: float) -> PreparedMutations: ...

    def prepare_snapshot_page(self, handles, *, deadline: float) -> PreparedMutations: ...

    def capture(self, *, page_size: int = MAX_BATCH, max_handles: int = 100_000,
                timeout_seconds: int = 30) -> dict: ...


def owner(organizations: "PublicOrganizations", policy: DiscoveryPolicy, *,
          admit_public: PublicRegistrationAdmission) -> OrganizationProjection:
    """Bind a trusted PublicOrganizations instance and independently verified current rights.

    The runtime supplies a public service reqctx. PublicOrganizations.admit
    must admit this context for get_entity with an empty registration tuple.
    Use admit_public for the same context and full per-registration discovery
    approval. Re-enter the same context when confirming a prepared result.
    """
    from ..application.search_projection import Projection

    return Projection(organizations, policy, admit_public)
