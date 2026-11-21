"""安徽省公共资源交易平台-工程建设公告（anhui-ggzy-construction）。

列表：POST /jsgc/list（服务端渲染 HTML，按发布日期倒序，每页 10 条），按公告类型 × 工程类别分别分页。
正文：POST /jsgc/newDetailSub 返回当前环节的 HTML 片段；公告类型 1/2/3 对应环节 tender/pbjg/zbjg。
"""
from __future__ import annotations

import html
import re
from datetime import date
from urllib.parse import urlencode, urljoin, urlsplit

from miaoqia.acquisition.api.handlers import DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, Window
from miaoqia.sdk.source_html import parse_document

from . import html_text

BASE = "https://ggzy.ah.gov.cn"
NATURES = ("1", "2", "3")            # 招标公告、中标候选人公示、中标结果公告
PROJECT_TYPES = ("1", "2", "3", "4", "5")   # 建筑、水利、交通、其他、农业工程
STAGE = {"1": "tender", "2": "pbjg", "3": "zbjg"}

ITEM = re.compile(r'<li class="list-item">\s*<a href="/jsgc/newDetail\?guid=([0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12})&(?:amp;)?tenderProjectType=(\d)'
                  r'&(?:amp;)?bulletinNature=(\d)"[^>]*>.*?<span class="title[^"]*" title="([^"]*)".*?'
                  r'<span class="date[^"]*">\s*(\d{4}-\d{2}-\d{2})\s*</span>', re.S)
NEXT = re.compile(r'onclick="pagination\([^)]*\);return false;">下一页')
EMPTY_STAGE = "此流程暂无信息"
ATTACHMENT_BODY = "//div[contains(concat(' ', normalize-space(@class), ' '), ' article-text-box ')]"


class AnhuiGgzyConstruction:
    source_id = "anhui-ggzy-construction"
    channels = tuple(f"{nature}:{ptype}" for nature in NATURES for ptype in PROJECT_TYPES)
    windowed = False
    # D1b observed only body HTML and links to other publishers. An admitted route
    # needs a full path pattern with explicit guid, nature and part_key groups.
    attachment_path: re.Pattern[str] | None = None

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        nature, ptype = channel.split(":")
        return FetchRequest("POST", f"{BASE}/jsgc/list", form=(
            ("currentPage", str(page)), ("tenderProjectType", ptype), ("bulletinNature", nature),
            ("jyptId", ""), ("region", "")))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        text = html_text(response)
        if 'class="list clear"' not in text:
            raise ValueError("list container missing")
        refs = []
        for guid, ptype, nature, title, published in ITEM.findall(text):
            if ptype not in PROJECT_TYPES or nature not in NATURES:
                raise ValueError("unsupported project type or notice type")
            query = urlencode({"guid": guid, "tenderProjectType": ptype, "bulletinNature": nature})
            refs.append(DetailRef(source_key=f"{guid}:{nature}", source_url=f"{BASE}/jsgc/newDetail?{query}",
                                  published=date.fromisoformat(published), title=html.unescape(title)))
        return ListPage(tuple(refs), bool(NEXT.search(text)), ordered_by_publication=True)

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        guid, nature = ref.source_key.split(":")
        stage = STAGE[nature]
        request = FetchRequest("POST", f"{BASE}/jsgc/newDetailSub", form=(
            ("type", stage), ("bulletinNature", nature), ("guid", guid), ("statusGuid", "")))
        return (DetailFetch(f"{guid}/{stage}", request),)

    def check_detail(self, response: FetchResponse) -> None:
        text = html_text(response)
        if EMPTY_STAGE in text or "article-title" not in text:
            raise ValueError("stage has no announcement body")

    def attachment_fetches(self, channel: str, ref: DetailRef, body: FetchResponse) -> tuple[DetailFetch, ...]:
        if self.attachment_path is None or ref.source_key is None:
            return ()
        guid, nature = ref.source_key.split(":")
        origin = urlsplit(BASE)
        containers = parse_document(html_text(body)).xpath(ATTACHMENT_BODY)
        if len(containers) != 1:
            raise ValueError("announcement attachment container is missing or ambiguous")
        parts = {}
        for anchor in containers[0].xpath(".//a[@download and @href]"):
            href = anchor.get("href")
            if "%" in href or "\\" in href or any(char.isspace() or ord(char) < 32 for char in href):
                continue
            try:
                relative = urlsplit(href)
                address = urlsplit(urljoin(body.url, href))
            except ValueError:
                continue
            if (any(segment in (".", "..") for segment in relative.path.split("/"))
                    or (address.scheme, address.netloc) != (origin.scheme, origin.netloc)
                    or address.query or address.fragment):
                continue
            match = self.attachment_path.fullmatch(address.path)
            if match is None or match["guid"].lower() != guid.lower() or match["nature"] != nature:
                continue
            name = anchor.get("download").strip()
            part = DetailFetch(match["part_key"], FetchRequest("GET", address.geturl()),
                               kind="attachment", name=name if 0 < len(name) <= 512 else None)
            if part.artifact_key in parts and parts[part.artifact_key] != part:
                raise ValueError("conflicting declaration for attachment key")
            parts[part.artifact_key] = part
        return tuple(parts.values())
