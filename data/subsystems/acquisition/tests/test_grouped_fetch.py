"""Exercise grouped collection through the existing fetcher and durable archive path."""
from __future__ import annotations

import hashlib
import json
from dataclasses import replace
from datetime import date, timedelta

import pytest

from miaoqia.acquisition.api.frozen_batches import FrozenBatch, FrozenBatchDenied, FrozenOriginal, ObservationRef
from miaoqia.acquisition.api.handlers import (DetailRef, FetchRequest, FetchResponse, GroupedNotice, GroupSelector,
                                            ListPage, NoticeGroup, Window)
from miaoqia.acquisition.application import grouped_observations
from miaoqia.acquisition.application.fetch import NOTICE_ARCHIVED, Fetcher
from miaoqia.acquisition.application.frozen_batches import FrozenBatches
from miaoqia.acquisition.application.sources import Schedule
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool
from miaoqia.kernel import reqctx

from test_fetch import (DAY, LIST_URL, SOURCE, Script, StreamingScript, env, events, expire_lease, listing,
                        ok, plan_sources, rows, run_planned)  # noqa: F401

GROUP_URL = "https://src.example/group/section"
WINDOW = Window(DAY, DAY)


class GroupHandler:
    source_id = SOURCE.source_id
    channels = ("published",)
    windowed = True

    def list_request(self, channel, window, page):
        return FetchRequest("GET", LIST_URL)

    def parse_list(self, channel, response):
        group_keys = json.loads(response.body)
        return ListPage((), False, groups=tuple(NoticeGroup(key, self.group_request(channel, key)) for key in group_keys))

    def group_request(self, channel, group_key):
        return FetchRequest("GET", f"https://src.example/group/{group_key}")

    def expand_group(self, channel, group, response):
        return tuple(GroupedNotice(
            DetailRef(key, group.request.url, date.fromisoformat(published), "Synthetic notice"),
            GroupSelector("fixture", 1, group.group_key, key),
            "sha256:" + hashlib.sha256(body.encode()).hexdigest(),
        ) for key, published, body in json.loads(response.body)["members"])


SOURCE_GROUPS = replace(SOURCE, handler=GroupHandler())


def _list(*keys):
    return FetchResponse(200, "application/json", json.dumps(keys).encode(), LIST_URL)


def _group(first="body-a", timestamp="initial"):
    members = [["a", DAY.isoformat(), first], ["b", DAY.isoformat(), "body-b"],
               ["outside", (DAY - timedelta(days=1)).isoformat(), "outside window"]]
    raw = json.dumps({"timestamp": timestamp, "members": members}).encode()
    return FetchResponse(200, "application/json", raw, GROUP_URL)


def _run(env, routes, *, spool=None):
    database, store, clock = env
    transport = StreamingScript(routes) if spool is not None else Script(routes)
    return Fetcher(database, store, transport, clock=clock, spool=spool), transport


def test_grouped_collection_preserves_full_bytes_and_versions_each_notice(env):
    database, store, _ = env
    first, envelope_changed, revision, reversion = _group(), _group(timestamp="retry"), _group("revised"), _group()
    collector, transport = _run(env, {LIST_URL: [_list("section")] * 4,
                                      GROUP_URL: [first, envelope_changed, revision, reversion]})
    reports = [collector.run(SOURCE_GROUPS, WINDOW, max_pages=1, refetch=True) for _ in range(4)]
    assert [(report.archived, report.unchanged, report.listed) for report in reports] == [
        (2, 0, 2), (0, 2, 2), (1, 1, 2), (1, 1, 2),
    ]
    assert [req.url for req in transport.sent] == [LIST_URL, GROUP_URL] * 4
    observations = events(database, grouped_observations.GROUPED_NOTICE_ARCHIVED)
    assert [(env["data"]["notice_key"], env["data"]["observation_version"]) for env in observations] == [
        ("a", 1), ("b", 1), ("a", 2), ("a", 3),
    ]
    assert observations[0]["subject"] == observations[1]["subject"] == observations[3]["subject"]
    assert store.get(observations[0]["subject"]) == first.body
    assert store.get(observations[2]["subject"]) == revision.body
    assert rows(database, "SELECT count(*) FROM acq_group_receipts") == [(4,)]
    assert rows(database, "SELECT count(*) FROM acq_raw_artifacts") == [(3,)]

    ref = ObservationRef(SOURCE_GROUPS.source_id, "a", 1)
    binding = FrozenBatch((FrozenOriginal(ref, observations[0]["data"]["content_hash"]),))
    service = FrozenBatches(database, store, authorize=lambda *_: True)
    with reqctx.use(reqctx.service("frozen-batch-test")):
        with pytest.raises(FrozenBatchDenied):
            service.read(binding, 0)
        with pytest.raises(FrozenBatchDenied):
            service.freeze((ref,))
        with pytest.raises(FrozenBatchDenied):
            service.restore(binding, 0, backup=store)


def test_standalone_capture_cannot_advance_a_grouped_notice(env):
    database, _, _ = env
    collector, _ = _run(env, {LIST_URL: [_list("section")], GROUP_URL: [_group()]})
    assert collector.run(SOURCE_GROUPS, WINDOW, max_pages=1).archived == 2
    ordinary, _ = _run(env, {LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"<html>Unbound</html>")]})
    report = ordinary.run(SOURCE, WINDOW, max_pages=1, refetch=True)
    assert report.archived == 0 and report.failed == 1
    assert "group_observation_conflict" in {gap["reason"] for gap in report.gaps}
    assert rows(database, "SELECT state FROM acq_request_tasks WHERE kind = 'body'") == [("failed",)]
    assert not events(database, NOTICE_ARCHIVED)
    assert len(events(database, grouped_observations.GROUPED_NOTICE_ARCHIVED)) == 2


def test_historical_group_limit_records_gap_without_repeating_the_same_front_pages(env):
    database, _, clock = env

    class BoundedGroups(GroupHandler):
        def parse_list(self, channel, response):
            page = super().parse_list(channel, response)
            if not page.groups:
                return page
            return replace(page, has_next=True, coverage_gap="section_notice_coverage_unverified")

    schedule = Schedule(every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=2, backfill_max_pages=1)
    source = replace(SOURCE_GROUPS, handler=BoundedGroups(), schedule=schedule)
    declared = {source.source_id: source}
    collector, _ = _run(env, {LIST_URL: [_list(), _list("section"), _list()], GROUP_URL: [_group()]})
    now = clock.now()
    assert plan_sources(database, declared, now) == 2
    assert run_planned(collector, declared, now).pages == 1
    report = run_planned(collector, declared, now, "backfill")
    assert report.archived == 1 and report.failed == 0
    assert {gap["reason"] for gap in report.gaps} == {"section_notice_coverage_unverified", "page_limit"}
    assert rows(database, "SELECT report->'wait_for_source_change' FROM acq_collection_runs "
                          "WHERE mode = 'backfill'") == [(True,)]
    assert plan_sources(database, declared, now) == 0
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == [(DAY - timedelta(days=1),)]

    wider = {source.source_id: replace(source, schedule=replace(schedule, backfill_max_pages=2))}
    assert plan_sources(database, wider, now) == 1
    assert run_planned(collector, wider, now, "backfill").gaps == []
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == []


def test_grouped_recovery_uses_original_object_when_new_list_omits_group(env, monkeypatch):
    database, _, _ = env
    collector, _ = _run(env, {LIST_URL: [_list("section")], GROUP_URL: [_group()]})
    submit = grouped_observations.submit_group

    def rollback(conn, **kwargs):
        submit(conn, **kwargs)
        raise RuntimeError("fixture transaction interrupted")

    with monkeypatch.context() as patch:
        patch.setattr(grouped_observations, "submit_group", rollback)
        with pytest.raises(RuntimeError, match="transaction interrupted"):
            collector.run(SOURCE_GROUPS, WINDOW, max_pages=1, refetch=True)
    assert rows(database, "SELECT count(*) FROM acq_group_receipts") == [(0,)]
    assert not events(database, grouped_observations.GROUPED_NOTICE_ARCHIVED)
    [(run_id,)] = rows(database, "SELECT run_id FROM acq_request_tasks WHERE kind = 'group'")
    expire_lease(database)

    resumed, transport = _run(env, {LIST_URL: [_list()]})
    report = resumed.run(SOURCE_GROUPS, WINDOW, max_pages=1, refetch=True)
    assert report.run_id == run_id and report.archived == 2
    assert [req.url for req in transport.sent] == [LIST_URL]
    assert "recovered_group_requires_rescan" in {gap["reason"] for gap in report.gaps}
    assert rows(database, "SELECT count(*) FROM acq_group_receipts") == [(1,)]


def test_grouped_recovery_reuses_spool_when_archive_write_failed(env, tmp_path, monkeypatch):
    database, store, _ = env
    spool = ArchiveSpool(tmp_path / "spool")
    collector, _ = _run(env, {LIST_URL: [_list("section")], GROUP_URL: [_group()]}, spool=spool)
    put = store.put

    def fail_raw(ref, raw):
        if ref.startswith("raw/"):
            raise OSError("fixture archive unavailable")
        put(ref, raw)

    with monkeypatch.context() as patch:
        patch.setattr(store, "put", fail_raw)
        first = collector.run(SOURCE_GROUPS, WINDOW, max_pages=1, refetch=True)
    assert first.deferred == "archive_object_pending" and not events(database, grouped_observations.GROUPED_NOTICE_ARCHIVED)
    resumed, transport = _run(env, {LIST_URL: [_list()]}, spool=spool)
    second = resumed.run(SOURCE_GROUPS, WINDOW, max_pages=1, refetch=True)
    assert second.run_id == first.run_id and second.archived == 2
    assert all(req.url == LIST_URL for req in transport.sent)
    assert rows(database, "SELECT count(*) FROM acq_group_receipts") == [(1,)]
