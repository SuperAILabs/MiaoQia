"""Opt-in runtime wiring for the authenticated public organization read."""
from contextlib import ExitStack, contextmanager

from miaoqia import evidence
from miaoqia.kernel import db

from .organizations import bind_organizations, parse_registry_sources


class _TimedDatabase:
    def __init__(self, database, deadline):
        self.database = database
        self.deadline = deadline

    def tx(self, *, deadline=None):
        return self.database.tx(deadline=self.deadline if deadline is None else min(self.deadline, deadline))


class _TimedObjects:
    def __init__(self, store, deadline):
        self.store = store
        self.deadline = deadline

    def get(self, key, *, max_bytes=None, deadline=None):
        bound = self.deadline if deadline is None else min(self.deadline, deadline)
        return self.store.get(key, max_bytes=max_bytes, deadline=bound)


@contextmanager
def configured_public_organizations(environ, domain, documents, store, evidence_dsn, *, pool_timeout):
    dsn = environ.get("MIAOQIA_ENTITY_GRAPH_DB_DSN")
    raw_sources = environ.get("MIAOQIA_PUBLIC_REGISTRY_SOURCES")
    if not raw_sources:
        yield None
        return
    approved = parse_registry_sources(raw_sources)
    if not dsn:
        yield None
        return
    with ExitStack() as stack:
        entities = db.Database(dsn, pool_timeout=pool_timeout)
        stack.callback(entities.close)
        extractions = evidence.extractions(evidence_dsn)
        stack.callback(extractions.close)

        def bind(authorize, deadline):
            return bind_organizations(
                _TimedDatabase(entities, deadline), _TimedDatabase(domain, deadline),
                _TimedDatabase(documents, deadline), _TimedObjects(store, deadline),
                extractions=extractions, approved_sources=approved, authorize=authorize,
            )

        yield bind
