"""来源处理器 SPI（SourceHandler）：信息领域包描述一个来源怎样发现与获取公告，acquisition 执行请求。

处理器不访问网络、不读写存储：它只构造请求、解析列表响应、识别来源的访问阻断。限速、重试、退避、去重、
归档与事件都由 acquisition 的抓取运行时负责（docs/03-architecture/subsystems/acquisition.md 第3、7节），
因此处理器可以只用录制的响应离线测试。
"""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from datetime import date
from typing import Protocol


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
class FetchResponse:
    status: int
    media_type: str                               # 不含参数的 Content-Type，例如 text/html
    body: bytes
    url: str


@dataclass(frozen=True)
class DetailRef:
    """列表发现的一条公告。source_key 在来源内稳定唯一；source_url 是可公开访问的原文地址。"""

    source_key: str
    source_url: str
    published: date | None
    title: str = ""


@dataclass(frozen=True)
class ListPage:
    refs: tuple[DetailRef, ...]
    has_next: bool


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
    request: FetchRequest


@dataclass(frozen=True)
class AccessContract:
    """按来源的访问约束；值来自领域包的来源声明。"""

    min_interval_seconds: float
    throttle_backoff_seconds: float
    max_attempts: int = 3
    timeout_seconds: float = 30.0
    headers: tuple[tuple[str, str], ...] = field(default=())

    def __post_init__(self):
        if self.min_interval_seconds < 1 or self.throttle_backoff_seconds < self.min_interval_seconds:
            raise ValueError("access contract too aggressive")
        if not 1 <= self.max_attempts <= 5 or not 1 <= self.timeout_seconds <= 120:
            raise ValueError("invalid access contract bounds")


class SourceHandler(Protocol):
    source_id: str
    channels: tuple[str, ...]          # 来源内独立分页的列表（公告类型、工程类别等）
    windowed: bool                     # True：列表请求按窗口过滤；False：列表按发布日期倒序，运行时按日期截止

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest: ...

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        """解析列表；遇到验证码或访问阻断时抛出 SourceBlocked。"""

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]: ...

    def check_detail(self, response: FetchResponse) -> None:
        """原件响应的来源特定校验；阻断时抛出 SourceBlocked，内容无效时抛出 ValueError。"""
