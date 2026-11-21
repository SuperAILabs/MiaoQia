"""Public search owner ports. Approval applies only to the metadata in this DTO."""

from dataclasses import dataclass
from typing import Protocol


class InvalidProjectionEvent(ValueError):
    """The supplied CloudEvent does not match the registered owner contract."""


class ProjectionUnavailable(RuntimeError):
    """An owner dependency, consistency check or batch budget prevented export."""


@dataclass(frozen=True)
class DiscoveryDecision:
    epoch: int
    approved: frozenset[str]


class CurrentDiscoveryPolicy(Protocol):
    def resolve(self, source_ids: frozenset[str], *, deadline: float) -> DiscoveryDecision:
        """Return current explicit discovery grants from one authoritative snapshot."""
        ...


class PreparedMutations(Protocol):
    mutations: list[dict]
    next_after: str | None

    def confirm(self, *, deadline: float) -> None:
        """Recheck the same decision and revisions immediately before handoff."""
        ...


class PublicProjectionOwner(Protocol):
    def prepare(self, event: dict, *, deadline: float, after: str = "") -> PreparedMutations: ...

    def prepare_snapshot_page(self, handles: list[dict], *, deadline: float) -> PreparedMutations:
        """Reconcile a bounded explicit page of fixed current facts from a rebuild manifest."""
        ...


def owner(database, policy: CurrentDiscoveryPolicy, *, notice_chunks=None) -> PublicProjectionOwner:
    from ..application.search_projection import Projection

    return Projection(database, policy, notice_chunks=notice_chunks)
