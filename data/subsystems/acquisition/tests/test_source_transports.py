"""Source declarations partition execution without splitting the catalog or plan ledger."""
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
import yaml

from miaoqia.acquisition import module
from miaoqia.acquisition.api.handlers import BrowserDetailPlan, BrowserListPlan, FetchRequest, ListPage
from miaoqia.acquisition.application import dispatch, sources
from miaoqia.kernel.runtime import Env
from miaoqia.runtime_acquisition import fetch

PACKS = Path(__file__).resolve().parents[4] / "packs"


class Listing:
    channels = ("notices",)
    windowed = False
    contract_version = 1

    def list_request(self, channel, window, page):
        return FetchRequest("GET", "https://publisher.example/list")

    def parse_list(self, channel, response):
        return ListPage((), False)


class HttpHandler(Listing):
    source_id = "http-source"

    def detail_fetches(self, channel, ref):
        return ()

    def check_detail(self, response):
        return None


class BrowserHandler(Listing):
    source_id = "browser-source"

    def browser_detail(self, channel, ref):
        return BrowserDetailPlan(ref.source_url, FetchRequest("GET", "https://publisher.example/body"))

    def validate_browser_notice(self, ref, original, selection):
        return ref.published


@pytest.fixture
def make_pack(monkeypatch, tmp_path):
    root = tmp_path / "packs"
    pack = root / "information_domains" / "example"
    pack.mkdir(parents=True)
    (pack / "pack.yaml").write_text("pack: {provides: {sources: sources.yaml}}")
    monkeypatch.setattr(sources, "load_pack_module", lambda *_: SimpleNamespace(
        HttpHandler=HttpHandler, BrowserHandler=BrowserHandler,
    ))

    def write(*, http=None, browser=None, inventory=None):
        access = {"min_interval_seconds": 2, "throttle_backoff_seconds": 60}
        schedule = {"every_minutes": 30, "lookback_days": 1, "max_pages": 1}
        declarations = [
            {"source_id": "http-source", "handler": "demo_pack.handlers:HttpHandler",
             "access": access, "schedule": schedule} | (http or {}),
            {"source_id": "browser-source", "handler": "demo_pack.handlers:BrowserHandler",
             "access": access, "schedule": schedule, "transport": "browser"} | (browser or {}),
        ]
        entries = [{"source_id": "http-source", "readiness": "capability_verified"},
                   {"source_id": "browser-source", "readiness": "capability_verified"} | (inventory or {})]
        (pack / "sources.yaml").write_text(yaml.safe_dump({"sources": declarations}))
        (pack / "source-inventory.yaml").write_text(yaml.safe_dump({"sources": entries}))
        return root

    return write


def test_active_transports_share_the_catalog_and_split_execution(make_pack, monkeypatch, tmp_path):
    root = make_pack()
    declared = sources.load(root)
    http = sources.load(root, transport="http")
    browser = sources.load(root, transport="browser")
    assert set(http) == {"http-source"}
    assert set(browser) == {"browser-source"}
    assert {key: value.transport for key, value in declared.items()} == {
        "http-source": "http", "browser-source": "browser",
    }
    assert sources.for_validation(root, "http-source")[1].transport == "http"

    monkeypatch.setenv("MIAOQIA_PACKS_DIR", str(root))
    monkeypatch.setenv("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR", str(tmp_path))
    monkeypatch.delenv("MIAOQIA_ACQUISITION_ARCHIVE_SPOOL_DIR", raising=False)
    planner_job = Mock()
    workers = Mock(return_value=[])
    monkeypatch.setattr(dispatch, "planner_job", planner_job)
    monkeypatch.setattr(dispatch, "workers", workers)
    scheduler = Env(role="acq-scheduler", brokers="", db=None, subsystem="acquisition")
    acquisition = module()
    acquisition.jobs(scheduler)
    acquisition.workers(replace(scheduler, role="acq-fetcher"))
    assert planner_job.call_args.args[1].keys() == declared.keys()
    assert workers.call_args.args[1].keys() == http.keys()


def test_browser_staging_and_live_admission(make_pack):
    root = make_pack(browser={"admission": "staged"}, inventory={"readiness": "candidate"})
    assert sources.catalog(root)["browser-source"].admission == "staged"
    assert sources.load(root, transport="browser") == {}
    assert set(sources.load(root)) == {"http-source"}
    with pytest.raises(ValueError, match="browser acquisition role is required"):
        sources.for_validation(root, "browser-source")

    root = make_pack(inventory={"readiness": "candidate"})
    with pytest.raises(ValueError, match="not capability_verified"):
        sources.load(root)
    root = make_pack(inventory={"reference_access_mode": "authenticated"})
    with pytest.raises(ValueError, match="requires an authorized source transport"):
        sources.load(root)


@pytest.mark.parametrize("value", ["ftp", None, ["browser"]])
def test_unknown_declaration_transport_fails_closed(make_pack, value):
    root = make_pack(browser={"transport": value})
    with pytest.raises(ValueError, match="unknown source transport"):
        sources.load(root, transport="http")


def test_unknown_load_filter_is_rejected(make_pack):
    with pytest.raises(ValueError, match="unknown source transport"):
        sources.load(make_pack(), transport="ftp")


@pytest.mark.parametrize("method", ["list_request", "parse_list", "browser_detail", "validate_browser_notice"])
def test_browser_entry_requires_callable_methods(make_pack, monkeypatch, method):
    monkeypatch.setattr(BrowserHandler, method, None)
    with pytest.raises(ValueError, match=f"requires callable {method}"):
        sources.load(make_pack())


def test_browser_source_declares_exactly_one_discovery_contract(make_pack, monkeypatch):
    def browser_list(self, channel, window, page):
        return BrowserListPlan("https://publisher.example", FetchRequest("GET", "https://publisher.example/feed"),
                               "#listing")

    monkeypatch.setattr(BrowserHandler, "browser_list", browser_list, raising=False)
    with pytest.raises(ValueError, match="exactly one listing contract"):
        sources.load(make_pack())

    class RenderedListHandler:
        source_id = BrowserHandler.source_id
        channels = BrowserHandler.channels
        windowed = BrowserHandler.windowed
        contract_version = BrowserHandler.contract_version
        browser_detail = BrowserHandler.browser_detail
        validate_browser_notice = BrowserHandler.validate_browser_notice

    monkeypatch.setattr(sources, "load_pack_module", lambda *_: SimpleNamespace(
        HttpHandler=HttpHandler, BrowserHandler=RenderedListHandler,
    ))
    monkeypatch.setattr(RenderedListHandler, "browser_list", browser_list, raising=False)
    with pytest.raises(ValueError, match="requires callable parse_browser_list"):
        sources.load(make_pack(), transport="browser")
    monkeypatch.setattr(RenderedListHandler, "parse_browser_list", lambda *_: ListPage((), False, reported_page=1),
                        raising=False)
    assert isinstance(sources.load(make_pack(), transport="browser")["browser-source"].handler, RenderedListHandler)


@pytest.mark.parametrize(("field", "value", "error"), [
    ("source_id", "another-source", "does not serve browser-source"),
    ("windowed", 1, "invalid channels or windowed"),
    ("channels", ("",), "invalid channels or windowed"),
    ("channels", ("notices", "notices"), "invalid channels or windowed"),
    ("contract_version", None, "contract_version must be a positive integer"),
    ("contract_version", True, "contract_version must be a positive integer"),
    ("contract_version", 0, "contract_version must be a positive integer"),
])
def test_browser_entry_rejects_invalid_contract_fields(make_pack, monkeypatch, field, value, error):
    monkeypatch.setattr(BrowserHandler, field, value)
    with pytest.raises(ValueError, match=error):
        sources.load(make_pack())


@pytest.mark.parametrize("method", [
    "detail_fetches", "check_detail", "resolve_detail", "attachment_fetches",
    "group_request", "expand_group", "to_selected_html",
])
def test_browser_entry_rejects_http_detail_hooks(make_pack, monkeypatch, method):
    monkeypatch.setattr(BrowserHandler, method, lambda *_: None, raising=False)
    with pytest.raises(ValueError, match=f"cannot implement {method}"):
        sources.load(make_pack())


@pytest.mark.parametrize("key", ["headers", "cookies", "attachment_origins"])
def test_browser_entry_rejects_request_state_and_attachment_origins(make_pack, key):
    root = make_pack(browser={"access": {
        "min_interval_seconds": 2, "throttle_backoff_seconds": 60, key: ["https://publisher.example"],
    }})
    with pytest.raises(ValueError, match="access keys|cannot declare attachment origins"):
        sources.load(root)


def test_http_entry_rejects_browser_hooks(make_pack, monkeypatch):
    monkeypatch.setattr(HttpHandler, "browser_detail", BrowserHandler.browser_detail, raising=False)
    with pytest.raises(ValueError, match="http handler .* cannot implement browser_detail"):
        sources.load(make_pack(), transport="http")


def test_http_default_and_explicit_fingerprints_match_and_browser_fingerprint_is_versioned(make_pack, monkeypatch):
    original = sources.load(make_pack())["http-source"]
    declared = sources.load(make_pack(http={"transport": "http"}))
    assert sources.config_fingerprint(original) == sources.config_fingerprint(declared["http-source"])
    browser = declared["browser-source"]
    first = sources.config_fingerprint(browser)
    assert first != sources.config_fingerprint(replace(browser, transport="http"))
    monkeypatch.setattr(BrowserHandler, "contract_version", 2)
    assert first != sources.config_fingerprint(sources.load(make_pack())["browser-source"])


@pytest.mark.parametrize(("args", "error"), [
    (["--source", "browser-source"], "has no active HTTP declaration"),
    (["--validate-source", "browser-source", "--channel", "notices"], "browser acquisition role is required"),
])
def test_http_cli_rejects_browser_before_opening_resources(make_pack, monkeypatch, capsys, args, error):
    monkeypatch.setenv("MIAOQIA_ACQUISITION_DB_DSN", "unused")
    monkeypatch.setenv("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR", "unused")
    database = Mock(side_effect=AssertionError("unexpected database"))
    transport = Mock(side_effect=AssertionError("unexpected HTTP transport"))
    monkeypatch.setattr(fetch.db, "Database", database)
    monkeypatch.setattr(fetch, "UrllibTransport", transport)
    with pytest.raises(SystemExit) as exc:
        fetch.main(["--packs", str(make_pack()), *args])
    assert exc.value.code == 2
    assert error in capsys.readouterr().err
    database.assert_not_called()
    transport.assert_not_called()


def test_repository_loads_every_active_http_declaration():
    config = PACKS / "information_domains" / "procurement" / "sources.yaml"
    expected = {
        row["source_id"] for row in yaml.safe_load(config.read_text())["sources"]
        if row.get("admission", "active") == "active" and row.get("transport", "http") == "http"
    }
    active = sources.load(PACKS, transport="http")
    assert expected
    assert active.keys() == expected
    assert all(source.transport == "http" for source in active.values())
