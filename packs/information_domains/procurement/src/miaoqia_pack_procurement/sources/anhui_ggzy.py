"""安徽省公共资源交易平台-工程建设公告（anhui-ggzy-construction）。

列表：POST /jsgc/list（服务端渲染 HTML，按发布日期倒序，每页 10 条），按公告类型 × 工程类别分别分页。
正文：POST /jsgc/newDetailSub 返回当前环节的 HTML 片段；公告类型 1/2/3 对应环节 tender/pbjg/zbjg。
"""
from __future__ import annotations

import html
import re
from datetime import date
from urllib.parse import urlencode

from miaoqia.acquisition.api.handlers import DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, Window

from . import html_text

BASE = "https://ggzy.ah.gov.cn"
NATURES = ("1", "2", "3")            # 招标公告、中标候选人公示、中标结果公告
PROJECT_TYPES = ("1", "2", "3", "4", "5")   # 建筑、水利、交通、其他、农业工程
STAGE = {"1": "tender", "2": "pbjg", "3": "zbjg"}

ITEM = re.compile(r'<li class="list-item">\s*<a href="/jsgc/newDetail\?guid=([0-9a-fA-F-]{36})&(?:amp;)?tenderProjectType=(\d)'
                  r'&(?:amp;)?bulletinNature=(\d)"[^>]*>.*?<span class="title[^"]*" title="([^"]*)".*?'
                  r'<span class="date[^"]*">\s*(\d{4}-\d{2}-\d{2})\s*</span>', re.S)
NEXT = re.compile(r'onclick="pagination\([^)]*\);return false;">下一页')
EMPTY_STAGE = "此流程暂无信息"


class AnhuiGgzyConstruction:
    source_id = "anhui-ggzy-construction"
    channels = tuple(f"{nature}:{ptype}" for nature in NATURES for ptype in PROJECT_TYPES)
    windowed = False

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
            query = urlencode({"guid": guid, "tenderProjectType": ptype, "bulletinNature": nature})
            refs.append(DetailRef(source_key=f"{guid}:{nature}", source_url=f"{BASE}/jsgc/newDetail?{query}",
                                  published=date.fromisoformat(published), title=html.unescape(title)))
        return ListPage(tuple(refs), bool(NEXT.search(text)))

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
