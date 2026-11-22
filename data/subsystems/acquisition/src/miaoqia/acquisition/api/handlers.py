"""来源处理器 SPI（SourceHandler）：信息领域包描述一个来源怎样发现与获取公告，acquisition 执行请求。

处理器不访问网络、不读写存储：它只构造请求、解析列表响应、识别来源的访问阻断。限速、重试、退避、去重、
归档与事件都由 acquisition 的抓取运行时负责（docs/03-architecture/subsystems/acquisition.md 第3、7节），
因此处理器可以只用录制的响应离线测试。
"""
from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass, field, replace
from datetime import date, datetime
from typing import Literal, Protocol
from urllib.parse import urlsplit


class SourceBlocked(RuntimeError):
    """来源要求人工交互（验证码、登录）或明确拒绝访问：停止该来源，不尝试绕过。"""

    def __init__(self, code: str):
        super().__init__(code)
        self.code = code


@dataclass(frozen=True)
class FetchRequest:
    method: str                                   # GET | POST
    url: str
    form: tuple[tuple[str, str], ...] = ()        # application/x-www-form-urlencoded 请求体
    json_body: str | None = None                  # 已序列化的 JSON 请求体
    headers: tuple[tuple[str, str], ...] = ()

    def __post_init__(self):
        if self.method not in ("GET", "POST"):
            raise ValueError(f"unsupported method {self.method!r}")
        if not self.url.startswith(("https://", "http://")):
            raise ValueError("absolute http(s) URL required")
        if self.form and self.json_body is not None:
            raise ValueError("form and JSON body are exclusive")

    @staticmethod
    def post_json(url: str, body: dict, headers: tuple[tuple[str, str], ...] = ()) -> "FetchRequest":
        return FetchRequest("POST", url, json_body=json.dumps(body, ensure_ascii=False, sort_keys=True,
                                                              separators=(",", ":")), headers=headers)

    def fingerprint(self) -> str:
        """请求指纹：方法、地址与请求体决定；请求头不参与（不改变所请求的对象）。"""
        canonical = json.dumps([self.method, self.url, sorted(self.form), self.json_body],
                               ensure_ascii=False, separators=(",", ":"))
        return "sha256:" + hashlib.sha256(canonical.encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class ListRequestPlan:
    """Stable list identity, wire request and its mandatory same-origin preflight."""

    identity: FetchRequest
    request: FetchRequest
    preflight: FetchRequest


@dataclass(frozen=True)
class FetchResponse:
    status: int
    media_type: str                               # 不含参数的 Content-Type，例如 text/html
    body: bytes
    url: str
    retry_after: str | None = None
    redirect_to: str | None = None


@dataclass(frozen=True)
class DetailRef:
    """列表发现的一条公告。source_key 缺失时只归档待核对原件；source_url 用于定位。"""

    source_key: str | None
    source_url: str
    published: date | None
    title: str = ""

    def __post_init__(self):
        if self.source_key is None:
            return
        if (not isinstance(self.source_key, str) or not self.source_key.strip() or len(self.source_key) > 512
                or self.source_key == self.source_url or self.source_key.startswith(("http://", "https://"))):
            raise ValueError("source_key must be a nonempty stable source identifier of at most 512 characters")


@dataclass(frozen=True)
class BrowserSelection:
    """The publisher's uniquely selected element from the current main frame."""

    html: str
    navigation_id: str
    frame_id: str
    loader_id: str


@dataclass(frozen=True)
class BrowserPagePlan:
    """One new-page navigation and the exact data request expected from that page."""

    navigation_url: str
    body_request: FetchRequest
    body_selector: str | None = None

    def __post_init__(self) -> None:
        if (not isinstance(self.navigation_url, str) or not self.navigation_url.startswith("https://")
                or not isinstance(self.body_request, FetchRequest) or self.body_request.method != "GET"
                or not self.body_request.url.startswith("https://") or self.body_request.form
                or self.body_request.json_body is not None or self.body_request.headers):
            raise ValueError("browser capture requires a public HTTPS navigation and a data GET")
        if self.body_selector is not None and (
            not isinstance(self.body_selector, str) or not self.body_selector.strip() or len(self.body_selector) > 512
        ):
            raise ValueError("invalid browser capture selector")


@dataclass(frozen=True)
class BrowserDetailPlan(BrowserPagePlan):
    """Capture one notice body."""


@dataclass(frozen=True)
class BrowserListPlan(BrowserPagePlan):
    """Capture one publisher listing page."""


class BrowserNoticeValidator(Protocol):
    def __call__(self, ref: DetailRef, original: FetchResponse, selection: BrowserSelection) -> date:
        """Verify publisher notice identity and raw-to-rendered binding, and return the publisher's local date."""


def _group_key(value: str) -> bool:
    return (isinstance(value, str) and 1 <= len(value) <= 512 and value == value.strip()
            and not value.startswith(("http://", "https://")) and not re.search(r"[\x00-\x1f\x7f]", value))


@dataclass(frozen=True)
class NoticeGroup:
    """An upstream listing item that locates a response containing independent notices."""

    group_key: str
    request: FetchRequest

    def __post_init__(self) -> None:
        if not _group_key(self.group_key) or not isinstance(self.request, FetchRequest):
            raise ValueError("invalid notice group")


@dataclass(frozen=True)
class GroupSelector:
    """A versioned, auditable selector within one unmodified publisher response."""

    kind: str
    version: int
    group_key: str
    notice_key: str

    def __post_init__(self) -> None:
        if (not isinstance(self.kind, str) or not re.fullmatch(r"[a-z][a-z0-9_-]{0,95}", self.kind)
                or type(self.version) is not int or not 1 <= self.version <= 2_147_483_647
                or not _group_key(self.group_key) or not _group_key(self.notice_key)):
            raise ValueError("invalid notice group selector")


@dataclass(frozen=True)
class GroupedNotice:
    ref: DetailRef
    selector: GroupSelector
    record_digest: str

    def __post_init__(self) -> None:
        if (not isinstance(self.ref, DetailRef) or not isinstance(self.selector, GroupSelector)
                or self.ref.source_key != self.selector.notice_key or type(self.ref.published) is not date
                or not isinstance(self.ref.title, str) or not self.ref.title.strip()
                or not isinstance(self.record_digest, str)
                or not re.fullmatch(r"sha256:[0-9a-f]{64}", self.record_digest)):
            raise ValueError("invalid grouped notice")


@dataclass(frozen=True)
class OffsetPage:
    """Publisher total and raw row count for a fixed-size offset query."""

    total_records: int
    page_size: int
    row_count: int
    maximum_public_page: int | None = field(default=None, kw_only=True)

    def __post_init__(self) -> None:
        if (type(self.total_records) is not int or self.total_records < 0
                or type(self.page_size) is not int or self.page_size < 1
                or type(self.row_count) is not int or not 0 <= self.row_count <= self.page_size):
            raise ValueError("invalid offset pagination metadata")
        if self.maximum_public_page is not None and (
            type(self.maximum_public_page) is not int or self.maximum_public_page < 1
        ):
            raise ValueError("invalid public pagination boundary")

    def has_more(self, page: int) -> bool:
        start = (page - 1) * self.page_size
        if (page < 1 or (page > 1 and start >= self.total_records)
                or self.row_count != min(self.page_size, self.total_records - start)):
            raise ValueError("offset response row count does not match requested page")
        if self.maximum_public_page is not None:
            if page > self.maximum_public_page:
                raise ValueError("offset response is beyond the public pagination boundary")
            if page == self.maximum_public_page:
                return False
        return start + self.row_count < self.total_records


@dataclass(frozen=True)
class ListPage:
    """A discovered page and any known reason it cannot establish full coverage."""

    refs: tuple[DetailRef, ...]
    has_next: bool
    coverage_gap: str | None = None
    ordered_by_publication: bool = False
    reported_page: int | None = None
    offset: OffsetPage | None = None
    groups: tuple[NoticeGroup, ...] = field(default=(), kw_only=True)

    def __post_init__(self) -> None:
        if self.groups and (self.refs or self.ordered_by_publication
                            or any(not isinstance(group, NoticeGroup) for group in self.groups)):
            raise ValueError("group listings cannot contain notices or imply publication order")

    def for_request(self, page: int, *, window: Window | None) -> ListPage:
        """Verify response metadata and resolve continuation for the requested page."""
        self.verify_request(page, window=window)
        return self if self.offset is None else replace(self, has_next=self.offset.has_more(page))

    def verify_request(self, page: int, *, window: Window | None) -> None:
        """Check a response that supplies authoritative pagination metadata."""
        if self.reported_page is None and self.offset is None:
            return
        if self.reported_page is not None and (type(self.reported_page) is not int or self.reported_page != page):
            raise ValueError("list response page does not match request")
        if window is not None and any(
            ref.published is None or not window.since <= ref.published <= window.until for ref in self.refs
        ):
            raise ValueError("list response contains a notice outside the request window")


@dataclass(frozen=True)
class Window:
    """发现窗口（含两端，按发布日期）。"""

    since: date
    until: date

    def __post_init__(self):
        if self.since > self.until:
            raise ValueError("window since is after until")


@dataclass(frozen=True)
class DetailFetch:
    """获取一条公告原件的请求；artifact_key 是该原件在来源内的稳定键（一条公告可有多个原件）。"""

    artifact_key: str
    request: FetchRequest | None = None
    kind: Literal["body", "attachment"] = "body"
    required: bool = True
    name: str | None = None
    locator: FetchRequest | None = field(default=None, kw_only=True)

    def __post_init__(self):
        if self.kind not in ("body", "attachment") or type(self.required) is not bool:
            raise ValueError("invalid part classification")
        if self.kind == "body" and (self.request is None) == (self.locator is None):
            raise ValueError("body requires exactly one request or locator")
        if self.locator is not None and (self.kind != "body" or not isinstance(self.locator, FetchRequest)):
            raise ValueError("locator must be a body FetchRequest")
        if self.kind == "attachment" and (
            not isinstance(self.artifact_key, str) or not self.artifact_key.strip() or len(self.artifact_key) > 512
            or (self.name is not None and not 1 <= len(self.name) <= 512)
            or (self.request is not None and
                (self.request.method != "GET" or self.request.form or self.request.json_body is not None))
        ):
            raise ValueError("invalid attachment declaration")


@dataclass(frozen=True)
class AccessContract:
    """按来源的访问约束；值来自领域包的来源声明。"""

    min_interval_seconds: float
    throttle_backoff_seconds: float
    max_attempts: int = 3
    timeout_seconds: float = 30.0
    headers: tuple[tuple[str, str], ...] = field(default=())
    list_retention_days: int = 30
    attachment_origins: tuple[str, ...] = ()

    def __post_init__(self):
        if self.min_interval_seconds < 1 or self.throttle_backoff_seconds < self.min_interval_seconds:
            raise ValueError("access contract too aggressive")
        if not 1 <= self.max_attempts <= 5 or not 1 <= self.timeout_seconds <= 120:
            raise ValueError("invalid access contract bounds")
        if type(self.list_retention_days) is not int or not 0 <= self.list_retention_days <= 365:
            raise ValueError("list_retention_days must be between 0 and 365")
        if not isinstance(self.attachment_origins, (tuple, list)):
            raise ValueError("attachment_origins must be a list of exact HTTPS origins")
        for value in self.attachment_origins:
            if type(value) is not str:
                raise ValueError("invalid attachment origin")
            host = urlsplit(value).hostname
            if (not host or value != f"https://{host}" or not host.isascii() or host.endswith(".")
                    or not all(char.isalnum() or char in ".-" for char in host)):
                raise ValueError("invalid attachment origin")
        object.__setattr__(self, "attachment_origins", tuple(self.attachment_origins))


class SourceHandler(Protocol):
    source_id: str
    channels: tuple[str, ...]          # 来源内独立分页的列表（公告类型、工程类别等）
    windowed: bool                     # 是否由来源按日期窗口过滤

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest: ...

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        """解析列表；覆盖范围未证实时返回 coverage_gap，验证码或访问阻断抛出 SourceBlocked。"""

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]: ...

    def check_detail(self, response: FetchResponse) -> date | None:
        """校验原件，可返回已核实的原站发布日期。阻断和无效内容分别抛出异常。"""


class BrowserSourceHandler(Protocol):
    """The notice body contract shared by browser sources."""

    source_id: str
    channels: tuple[str, ...]
    windowed: bool
    contract_version: int

    def browser_detail(self, channel: str, ref: DetailRef) -> BrowserDetailPlan: ...

    def validate_browser_notice(self, ref: DetailRef, original: FetchResponse, selection: BrowserSelection) -> date: ...


class BrowserListSourceHandler(BrowserSourceHandler, Protocol):
    def browser_list(self, channel: str, window: Window, page: int) -> BrowserListPlan: ...

    def parse_browser_list(self, channel: str, original: FetchResponse, selection: BrowserSelection) -> ListPage:
        """Verify publisher data and the matching visible listing; raise SourceBlocked on refusal."""


def checked_detail_ref(handler: SourceHandler, ref: DetailRef, response: FetchResponse) -> DetailRef:
    check_with_ref = getattr(handler, "check_detail_with_ref", None)
    published = handler.check_detail(response) if check_with_ref is None else check_with_ref(ref, response)
    if published is None:
        return ref
    if type(published) is not date or (ref.published is not None and ref.published != published):
        raise ValueError("detail publication date disagrees with the listing")
    return replace(ref, published=published)


class ListPreflightHandler(SourceHandler, Protocol):
    def list_plan(self, channel: str, window: Window, page: int, now: datetime) -> ListRequestPlan: ...

    def check_list_preflight(self, response: FetchResponse) -> None:
        """Accept publisher approval, or raise SourceBlocked or ValueError."""


class LocatorSourceHandler(SourceHandler, Protocol):
    """Optional SPI for sources whose public lookup returns the original's address."""

    def resolve_detail(self, channel: str, ref: DetailRef, response: FetchResponse) -> FetchRequest:
        """校验公开定位响应并构造正文请求；阻断时抛出 SourceBlocked，内容无效时抛出 ValueError。"""


class GroupedSourceHandler(SourceHandler, Protocol):
    def group_request(self, channel: str, group_key: str) -> FetchRequest:
        """Rebuild the same anonymous request when recovering a recorded group task."""

    def expand_group(self, channel: str, group: NoticeGroup, response: FetchResponse) -> tuple[GroupedNotice, ...]:
        """Validate the response and describe each independently identified notice."""


def expand_notice_group(handler: SourceHandler, channel: str, group: NoticeGroup,
                        response: FetchResponse) -> tuple[GroupedNotice, ...]:
    expand = getattr(handler, "expand_group", None)
    if not callable(expand):
        raise ValueError("group discovery requires an expansion handler")
    notices = expand(channel, group, response)
    if not isinstance(notices, tuple) or len(notices) > 256 or any(
        not isinstance(notice, GroupedNotice) or notice.selector.group_key != group.group_key for notice in notices
    ):
        raise ValueError("invalid group expansion")
    if len({notice.selector.notice_key for notice in notices}) != len(notices):
        raise ValueError("duplicate notice key in group")
    return notices


def resolve_detail_request(handler: SourceHandler, channel: str, ref: DetailRef,
                           response: FetchResponse) -> FetchRequest:
    resolve = getattr(handler, "resolve_detail", None)
    if not callable(resolve):
        raise ValueError("detail locator requires a resolver")
    request = resolve(channel, ref, response)
    if not isinstance(request, FetchRequest):
        raise ValueError("detail locator must resolve to a FetchRequest")
    return request


class AttachmentHandler(SourceHandler, Protocol):
    def attachment_fetches(self, channel: str, ref: DetailRef, body: FetchResponse) -> tuple[DetailFetch, ...]:
        """只从已验证的公告内容容器或正文专用字段提取部件。独立 HTTPS 来源须明确登记。"""
