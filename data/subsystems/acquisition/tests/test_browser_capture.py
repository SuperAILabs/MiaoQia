import asyncio
from base64 import b64encode
from copy import deepcopy

import pytest

from miaoqia.acquisition.infrastructure.browser_capture import BrowserCaptureError, capture_browser_response

URL = "https://publisher.test/announcements/90071992547409931"
REQUEST_ID = "request"
FRAME = "main"
LOADER = "current"
ADDRESS = "192.0.2.1"
PREFIX = b"\x80\x00"
SUFFIX = b"\xff\x83\r\n"


def encoded(body):
    return b64encode(body).decode("ascii")


def network(name, request_id=REQUEST_ID, **fields):
    return f"Network.{name}", {"requestId": request_id, **fields}


def request(request_id=REQUEST_ID, *, url=URL, method="GET", frame=FRAME, loader=LOADER, **extra):
    return network("requestWillBeSent", request_id, request={"url": url, "method": method},
                   frameId=frame, loaderId=loader, **extra)


def observation(headers=None):
    if headers is None:
        headers = {"Content-Length": "6", "Content-Type": "text/html; charset=windows-1252"}
    return [
        network("requestWillBeSentExtraInfo", headers={"Host": "publisher.test"}),
        request(),
        network("responseReceived", frameId=FRAME, loaderId=LOADER, response={
            "url": URL, "status": 200, "remoteIPAddress": ADDRESS, "remotePort": 443, "headers": headers,
        }),
        network("dataReceived", dataLength=len(PREFIX), encodedDataLength=0),
        network("dataReceived", dataLength=len(SUFFIX), encodedDataLength=0, data=encoded(SUFFIX)),
        network("dataReceived", dataLength=0, encodedDataLength=12),
        network("loadingFinished", encodedDataLength=123),
        network("responseReceivedExtraInfo", statusCode=200, headers=deepcopy(headers)),
    ]


class FakeSession:
    def __init__(self, *, buffered=encoded(PREFIX), loader=LOADER, stall=None):
        self.buffered = buffered
        self.loader = loader
        self.stall = stall
        self.commands = []

    async def send(self, method, params):
        self.commands.append((method, params))
        if method == self.stall:
            await asyncio.Future()
        if method == "Network.streamResourceContent":
            assert params == {"requestId": REQUEST_ID}
            return {"bufferedData": self.buffered}
        assert method == "Page.getFrameTree" and params == {}
        return {"frameTree": {"frame": {"id": FRAME, "loaderId": self.loader}}}


async def event_stream(events, *, stall=False, disconnect=False):
    for event in events:
        yield event
    if stall:
        await asyncio.Future()
    if disconnect:
        raise ConnectionError("disconnected instead of completing the observation")


def collect(session, events, **options):
    target = {
        "method": "GET", "url": URL, "frame_id": FRAME, "loader_id": LOADER,
        "allowed_ips": frozenset({ADDRESS}), "timeout": 1, "max_body": 6,
    }
    return asyncio.run(capture_browser_response(session, events, **(target | options)))


@pytest.mark.parametrize("headers", [
    None,
    {"content-encoding": "IDENTITY", "content-length": "06"},
    {"Transfer-Encoding": "chunked"},
    {},
])
def test_captures_exact_binary_from_one_completed_current_request(headers):
    session = FakeSession()
    events = observation(headers)
    events[0][1]["headers"] = {":authority": "publisher.test:443"}
    events = [request("old", loader="old"), request("frame", frame="child"),
              request("decoy", url=URL + "?decoy=1"), request("post", method="POST"), *events]
    capture = collect(session, event_stream(events), max_events=len(events))
    assert capture.response.body == PREFIX + SUFFIX
    assert (capture.response.url, capture.response.status) == (URL, 200)
    assert capture.response.media_type == ("text/html" if headers is None else "application/octet-stream")
    assert (capture.method, capture.request_id, capture.frame_id, capture.loader_id) == ("GET", REQUEST_ID, FRAME, LOADER)
    assert (capture.remote_ip, capture.remote_port) == (ADDRESS, 443)
    assert session.commands == [("Network.streamResourceContent", {"requestId": REQUEST_ID}), ("Page.getFrameTree", {})]


def test_captures_response_already_buffered_by_the_request_gate():
    session = FakeSession()
    events = observation()
    del events[4][1]["data"]

    async def buffered(request_id):
        assert request_id == REQUEST_ID
        return PREFIX + SUFFIX

    capture = collect(session, event_stream(events), buffered_body=buffered)
    assert capture.response.body == PREFIX + SUFFIX
    assert session.commands == [("Page.getFrameTree", {})]
    with pytest.raises(BrowserCaptureError, match="buffer length mismatch"):
        collect(FakeSession(), event_stream(events[:4] + events[5:]), buffered_body=buffered)


@pytest.mark.parametrize(("field", "value"), [
    ("url", URL + "?other=1"), ("status", 206), ("status", 403), ("status", 302),
    ("fromDiskCache", True), ("fromPrefetchCache", True), ("fromServiceWorker", True),
    ("remoteIPAddress", "192.0.2.2"), ("remotePort", 444),
    ("headers", {"Content-Encoding": "gzip"}),
    ("headers", {"Content-Encoding": "identity, identity"}),
    ("headers", {"Content-Encoding": "identity", "content-encoding": "gzip"}),
    ("headers", {"Content-Length": "6, 6"}),
    ("headers", {"Content-Length": "7"}),
    ("headers", {"Content-Length": "6", "Transfer-Encoding": "chunked"}),
    ("headers", {"Content-Range": "bytes 0-5/45"}),
])
def test_rejects_ineligible_response_before_streaming(field, value):
    events = observation()
    events[2][1]["response"][field] = value
    session = FakeSession()
    with pytest.raises(BrowserCaptureError):
        collect(session, event_stream(events))
    assert session.commands == []


@pytest.mark.parametrize("changed_request", [request(frame="child"), request(loader="old"),
                                               request(method="POST"), request(url=URL + "?other=1")])
def test_requires_request_identity(changed_request):
    events = observation()
    events[1] = changed_request
    with pytest.raises(BrowserCaptureError, match="no completed approved response"):
        collect(FakeSession(), event_stream(events))


@pytest.mark.parametrize("field", ["frameId", "loaderId"])
def test_rejects_response_from_different_frame_or_loader(field):
    events = observation()
    events[2][1][field] = "different"
    with pytest.raises(BrowserCaptureError, match="ineligible response"):
        collect(FakeSession(), event_stream(events))


@pytest.mark.parametrize("headers", [
    {}, {"Host": "wrong.test"}, {"Host": "publisher.test:444"},
    {":authority": "publisher.test", "Host": "wrong.test"},
])
def test_requires_actual_wire_authority(headers):
    events = observation()
    events[0][1]["headers"] = headers
    with pytest.raises(BrowserCaptureError, match="wire authority"):
        collect(FakeSession(), event_stream(events))


@pytest.mark.parametrize(("position", "event", "error"), [
    (1, request(url=URL + "?redirect=1"), "redirected"),
    (4, request(url=URL + "?redirected=1", redirectResponse={"status": 302}), "redirected"),
    (8, request("second"), "ambiguous"),
    (1, network("requestServedFromCache"), "disqualified"),
    (8, network("loadingFailed"), "failed"),
    (8, network("loadingFinished"), "completion"),
    (8, network("requestWillBeSentExtraInfo", headers={"Host": "publisher.test"}), "wire metadata"),
])
def test_rejects_conflicting_lifecycle_events_including_late_events(position, event, error):
    events = observation()
    events.insert(position, event)
    with pytest.raises(BrowserCaptureError, match=error):
        collect(FakeSession(), event_stream(events))


def test_rejects_redirect_into_approved_url():
    events = observation()
    events[1] = request(redirectResponse={"status": 302})
    with pytest.raises(BrowserCaptureError, match="redirected"):
        collect(FakeSession(), event_stream(events))


@pytest.mark.parametrize(("index", "error"), [
    (0, "wire metadata"), (3, "buffer length"), (6, "no completed"), (7, "wire metadata"),
])
def test_rejects_incomplete_observation(index, error):
    events = observation()
    del events[index]
    with pytest.raises(BrowserCaptureError, match=error):
        collect(FakeSession(), event_stream(events))


@pytest.mark.parametrize(("wire_fields", "error"), [
    ({"statusCode": 304}, "wire metadata"),
    ({"headers": {"content-encoding": "gzip", "content-length": "6"}}, "non-identity"),
    ({"headers": {"content-length": "5"}}, "response length mismatch"),
])
def test_checks_wire_status_encoding_and_length(wire_fields, error):
    events = observation()
    events[-1][1].update(wire_fields)
    with pytest.raises(BrowserCaptureError, match=error):
        collect(FakeSession(), event_stream(events))


@pytest.mark.parametrize(("chunk", "length", "error"), [
    ("!!!!", 4, "invalid CDP binary"), (encoded(SUFFIX), 5, "chunk length mismatch"),
    (encoded(SUFFIX + b"!"), 5, "size cap"),
])
def test_rejects_bad_chunks_even_without_content_length(chunk, length, error):
    events = observation({})
    events[4][1].update(data=chunk, dataLength=length)
    with pytest.raises(BrowserCaptureError, match=error):
        collect(FakeSession(), event_stream(events))


def test_rejects_missing_stream_bytes_even_without_content_length():
    events = observation({})
    events[5][1]["dataLength"] = 1
    with pytest.raises(BrowserCaptureError, match="missing CDP stream chunk"):
        collect(FakeSession(), event_stream(events))


def test_checks_assembled_content_length():
    events = observation()
    events[4][1].update(data=encoded(SUFFIX[:-1]), dataLength=3)
    with pytest.raises(BrowserCaptureError, match="response length mismatch"):
        collect(FakeSession(), event_stream(events))


def test_bounds_buffered_data():
    session = FakeSession(buffered=encoded(b"1234567"))
    with pytest.raises(BrowserCaptureError, match="size cap"):
        collect(session, event_stream(observation({})))


def test_bounds_event_count():
    with pytest.raises(BrowserCaptureError, match="event cap"):
        collect(FakeSession(), event_stream(observation()), max_events=7)


@pytest.mark.parametrize("stall", ["observation", "Network.streamResourceContent", "Page.getFrameTree"])
def test_deadline_covers_commands_and_observation_barrier(stall):
    with pytest.raises(BrowserCaptureError, match="deadline exceeded"):
        collect(FakeSession(stall=stall), event_stream(observation(), stall=stall == "observation"), timeout=0.02)


def test_rejects_disconnect_after_successful_loading():
    with pytest.raises(BrowserCaptureError, match="interrupted CDP observation"):
        collect(FakeSession(), event_stream(observation(), disconnect=True))


def test_rejects_stale_loader_at_observation_end():
    with pytest.raises(BrowserCaptureError, match="main frame or loader changed"):
        collect(FakeSession(loader="next"), event_stream(observation()))
