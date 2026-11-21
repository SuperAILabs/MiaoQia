import asyncio
from dataclasses import dataclass, replace

import pytest

from miaoqia.acquisition.infrastructure.browser_capture import BrowserCaptureError
from miaoqia.acquisition.infrastructure.browser_session import observe_browser_navigation

from test_browser_capture import ADDRESS, FRAME, LOADER, PREFIX, SUFFIX, URL, FakeSession, observation, request

VIEW = "https://publisher.test/view"
NAVIGATION_ID = "navigation"


@dataclass(frozen=True)
class Selection:
    html: str
    navigation_id: str
    frame_id: str
    loader_id: str


class Session(FakeSession):
    def __init__(self, *, initial_url="about:blank", late=None, detach_error=False):
        super().__init__()
        self.listeners = {}
        self.url = initial_url
        self.frame_checks = 0
        self.late = late
        self.detach_error = detach_error
        self.detached = False

    def on(self, event, callback):
        self.listeners[event] = callback

    def remove_listener(self, event, callback):
        assert self.listeners.pop(event) == callback

    def emit(self, name, params):
        self.listeners[name](params)

    async def detach(self):
        self.detached = True
        if self.detach_error:
            raise ConnectionError("detach failure")

    async def send(self, method, params):
        if method.startswith("Network.set") or method in {"Network.enable", "Page.enable"}:
            self.commands.append((method, params))
            return {}
        if method == "Page.getFrameTree":
            self.frame_checks += 1
            if self.frame_checks == 4 and self.late is not None:
                self.emit(*self.late)
            return {"frameTree": {"frame": {"id": FRAME, "loaderId": self.loader, "url": self.url}}}
        return await super().send(method, params)


@dataclass
class Context:
    session: Session
    page: object

    async def new_cdp_session(self, page):
        assert page is self.page
        return self.session


def observe(session=None, *, network_events=None, after_selection=None, select_change=None,
            delayed_extra=False, stall=False, **options):
    if session is None:
        session = Session()
    if network_events is None:
        network_events = observation()
    page = object()
    context = Context(session, page)

    async def navigate():
        assert session.commands == [
            ("Page.enable", {}),
            ("Network.enable", {"maxTotalBufferSize": 12, "maxResourceBufferSize": 6, "maxPostDataSize": 0}),
            ("Network.setCacheDisabled", {"cacheDisabled": True}),
            ("Network.setBypassServiceWorker", {"bypass": True}),
        ]
        if stall:
            await asyncio.Future()
        session.emit(*request(NAVIGATION_ID, url=VIEW, type="Document"))
        session.url = VIEW
        session.emit("Page.frameNavigated", {"frame": {"id": FRAME, "loaderId": LOADER, "url": VIEW}})
        for event in network_events[:-1] if delayed_extra else network_events:
            session.emit(*event)

    async def select(navigation):
        if after_selection is not None:
            session.emit(*after_selection)
        if delayed_extra:
            asyncio.get_running_loop().call_soon(session.emit, *network_events[-1])
        selection = Selection("<h1>公告</h1>", navigation.navigation_id, navigation.frame_id, navigation.loader_id)
        return replace(selection, **select_change) if select_change else selection

    limits = {"timeout": 1.0, "max_body": 6} | options
    try:
        return asyncio.run(observe_browser_navigation(
            context, page, navigation_url=VIEW, body_url=URL, allowed_ips=frozenset({ADDRESS}),
            navigate=navigate, select=select, **limits,
        ))
    finally:
        assert session.detached
        assert session.listeners == {}


@pytest.mark.parametrize("delayed_extra", [False, True])
def test_drains_buffered_events_and_late_wire_metadata(delayed_extra):
    capture, selection = observe(delayed_extra=delayed_extra)
    assert capture.response.body == PREFIX + SUFFIX
    assert selection == Selection("<h1>公告</h1>", NAVIGATION_ID, FRAME, LOADER)


@pytest.mark.parametrize(("options", "message"), [
    ({"max_events": 4}, "event count cap"),
    ({"max_event_bytes": 600}, "event byte cap"),
    ({"max_html_bytes": 13}, "oversized HTML"),
])
def test_bounds_observation(options, message):
    with pytest.raises(BrowserCaptureError, match=message):
        observe(**options)


def test_byte_cap_applies_before_removing_non_evidence_headers():
    events = observation()
    events[0][1]["headers"]["Cookie"] = "a" * 4096
    with pytest.raises(BrowserCaptureError, match="event byte cap") as error:
        observe(network_events=events, max_event_bytes=1024)
    assert "aaaa" not in str(error.value)


def test_discards_session_metadata_before_handing_events_to_collector():
    events = observation()
    events[0][1]["headers"].update({"Cookie": "private", "cookie": "private"})
    events[-1][1]["headers"].update({"Set-Cookie": "private", "set-cookie": "private"})
    capture, _ = observe(network_events=events)
    assert capture.response.body == PREFIX + SUFFIX
    assert "private" not in repr(capture)


@pytest.mark.parametrize(("event", "message"), [
    (request("other", url=VIEW, loader="other", type="Document"), "unexpected main-frame navigation"),
    (("Page.frameNavigated", {"frame": {"id": FRAME, "loaderId": "other", "url": VIEW}}), "unexpected main-frame commit"),
    (("Page.navigatedWithinDocument", {"frameId": FRAME}), "history change"),
    (("Page.frameDetached", {"frameId": FRAME}), "history change"),
    (("Page.windowOpen", {}), "popup"),
    (("close", None), "session closed"),
    (request("second"), "ambiguous approved request"),
])
def test_rejects_navigation_churn_disconnect_and_duplicates(event, message):
    with pytest.raises(BrowserCaptureError, match=message):
        observe(after_selection=event)


def test_rejects_duplicate_arriving_during_collector_final_frame_check():
    with pytest.raises(BrowserCaptureError, match="ambiguous approved request"):
        observe(Session(late=request("late")))


@pytest.mark.parametrize("key", ["navigation_id", "frame_id", "loader_id"])
def test_rejects_selection_from_another_navigation(key):
    with pytest.raises(BrowserCaptureError, match="different navigation"):
        observe(select_change={key: "stale"})


def test_fail_closed_for_non_blank_page():
    with pytest.raises(BrowserCaptureError, match="fresh blank main frame"):
        observe(Session(initial_url=VIEW))


def test_fail_closed_for_missing_wire_metadata_and_deadline():
    with pytest.raises(BrowserCaptureError, match="deadline exceeded"):
        observe(network_events=observation()[:-1], timeout=0.03)


def test_detaches_when_navigation_stalls():
    with pytest.raises(BrowserCaptureError, match="deadline exceeded"):
        observe(stall=True, timeout=0.03)


def test_success_requires_detach():
    with pytest.raises(BrowserCaptureError, match="failed to detach"):
        observe(Session(detach_error=True))
