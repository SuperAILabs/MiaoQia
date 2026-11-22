"""采购公告首级标题的类型与证据定位。"""
from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Literal

from miaoqia.sdk import Document
from miaoqia.sdk.document import Span

EDITION = r"(?:[(（]第\S{1,3}次[)）]|\[\d{4}\])?"
TENDER_MARK = re.compile(r"^(招标公告|采购公告|招标采购公告|公开招标公告)$")
FULL_TENDER_TITLE = re.compile(rf"\S{{4,}}(招标公告|采购公告|磋商公告|谈判公告|询价公告|邀请书)\s*{EDITION}$")
NOT_TENDER = re.compile(r"(更正|变更|澄清|终止|废标|流标|结果|中标|成交|候选人|意向|合同)")
RESULT_MARK = r"中标|成交|入围"
RESULT_SUFFIX = r"(?:结果)?(?:公告|公示)"
CANDIDATE = re.compile(r"候选人公示")
PRIMARY_RESULT = re.compile(
    rf"(?:(?P<candidate>{CANDIDATE.pattern})|(?P<framework>入围){RESULT_SUFFIX}"
    rf"|(?:{RESULT_MARK}){RESULT_SUFFIX}|中标[(（]成交[)）]{RESULT_SUFFIX}|采购(?:的)?结果公告){EDITION}$"
)
OTHER_TITLE = re.compile(
    rf"(?:(?:更正|变更|澄清|补充|终止|废标|流标|失败|验收|合同)(?:结果)?(?:公告|公示|通知)"
    rf"|采购意向(?:公开|公告|公示)?){EDITION}$"
)


@dataclass(frozen=True)
class PrimaryTitle:
    kind: Literal["tender", "tender_marker", "award", "candidate", "framework", "other"]
    span: Span


def primary_title(doc: Document) -> PrimaryTitle | None:
    if not doc.blocks:
        return None
    first = doc.blocks[0]
    if first["kind"] != "heading" or first.get("level") != 1:
        return None
    span = doc.block_span(first["index"])
    title = "".join(span.text.split())
    if TENDER_MARK.fullmatch(title):
        return PrimaryTitle("tender_marker", span)
    if OTHER_TITLE.search(title):
        return PrimaryTitle("other", span)
    if FULL_TENDER_TITLE.fullmatch(title) and not NOT_TENDER.search(title):
        return PrimaryTitle("tender", span)
    if match := PRIMARY_RESULT.search(title):
        if match["candidate"]:
            return PrimaryTitle("candidate", span)
        if match["framework"]:
            return PrimaryTitle("framework", span)
        return PrimaryTitle("award", span)
    return None
