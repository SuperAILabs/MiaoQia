"""规范化文档的只读访问：遍历文本单元、按标签查找取值，并给出精确定位。

文本单元是可被定位的最小文本：段落 / 标题 / 列表项各为一个单元，表格的每个单元格为一个单元。
字符区间以 Unicode 码点计，左闭右开，与 contracts/schemas/evidence/normalized-document.schema.json 一致。
"""
from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Iterator

# 标签与取值之间的分隔：全角或半角冒号
SEP = r"\s*[：:]\s*"


@dataclass(frozen=True)
class Span:
    """文档中的一段原文及其定位。"""

    text: str
    block: int
    char_start: int
    char_end: int
    row: int | None = None
    col: int | None = None
    block_end: int | None = None        # 跨多个连续块时的最后一个块

    def locator(self) -> dict:
        loc = {"block": self.block, "char_start": self.char_start, "char_end": self.char_end}
        if self.row is not None:
            loc.update(row=self.row, col=self.col)
        if self.block_end is not None:
            loc["block_end"] = self.block_end
        return loc


@dataclass(frozen=True)
class TextUnit:
    text: str
    block: int
    row: int | None = None
    col: int | None = None

    def span(self, start: int, end: int) -> Span:
        return Span(self.text[start:end], self.block, start, end, self.row, self.col)


class Document:
    def __init__(self, data: dict):
        self.data = data
        self.blocks = data["blocks"]

    @property
    def snapshot_ref(self) -> str:
        return self.data["snapshot_ref"]

    @property
    def pipeline_version(self) -> str:
        return self.data["pipeline_version"]

    def units(self) -> Iterator[TextUnit]:
        for b in self.blocks:
            if b["kind"] == "table":
                for r, row in enumerate(b.get("rows", [])):
                    for c, cell in enumerate(row):
                        yield TextUnit(cell["text"], b["index"], r, c)
            elif b.get("text"):
                yield TextUnit(b["text"], b["index"])

    def quote(self, locator: dict) -> str:
        if "block_end" in locator:
            first, last = locator["block"], locator["block_end"]
            texts = [self.blocks[i].get("text", "") for i in range(first, last + 1)]
            head = sum(len(t) for t in texts[:-1])
            joined = "".join(texts)
            return joined[locator.get("char_start", 0): head + locator.get("char_end", len(texts[-1]))]
        b = self.blocks[locator["block"]]
        text = b["rows"][locator["row"]][locator["col"]]["text"] if "row" in locator else b.get("text", "")
        return text[locator.get("char_start", 0): locator.get("char_end", len(text))]

    def join_blocks(self, first: int, last: int) -> Span:
        """把连续多个文本块作为一段原文（如分两行排版的标题）。"""
        texts = [self.blocks[i]["text"] for i in range(first, last + 1)]
        return Span("".join(texts), first, 0, len(texts[-1]), block_end=last)

    def block_span(self, index: int) -> Span:
        text = self.blocks[index]["text"]
        s, e = _trim(text, 0, len(text))
        return Span(text[s:e], index, s, e)

    def table_grid(self, index: int) -> list[list[Span | None]]:
        """表格的完整网格；合并单元格覆盖的位置返回起始单元格的 Span，空单元格为 None。"""
        rows = self.blocks[index]["rows"]
        out = []
        for r, row in enumerate(rows):
            line = []
            for c, cell in enumerate(row):
                src_r, src_c = (cell["covered_by"]["row"], cell["covered_by"]["col"]) if "covered_by" in cell else (r, c)
                text = rows[src_r][src_c]["text"]
                s, e = _trim(text, 0, len(text))
                line.append(Span(text[s:e], index, s, e, src_r, src_c) if e > s else None)
            out.append(line)
        return out

    def find_labeled(self, labels: list[str]) -> Span | None:
        """查找“标签：取值”。依次尝试：同一文本单元内的冒号分隔；表格中标签单元格右侧相邻单元格。

        取值截止到行尾或下一个“标签：”之前，并去掉首尾空白与句末标点。
        """
        alt = "|".join(re.escape(x) for x in sorted(labels, key=len, reverse=True))
        # 标签前可以是行首、空白、标点，或编号（“（1）招标人：”“6.2投标文件递交截止时间：”）
        inline = re.compile(rf"(?:^|[\s　，,；;。）)】\].、0-9])(?:{alt}){SEP}")
        for u in self.units():
            for m in inline.finditer(u.text):
                start = m.end()
                end = len(u.text)
                nxt = re.search(r"[\s　，,；;。]\s*[一-龥（）()A-Za-z]{2,12}" + SEP, u.text[start:])
                if nxt:
                    end = start + nxt.start()
                nl = u.text.find("\n", start)
                if 0 <= nl < end:
                    end = nl
                start, end = _trim(u.text, start, end)
                if end > start:
                    return u.span(start, end)
        exact = re.compile(rf"^\s*(?:{alt})\s*[：:]?\s*$")
        for b in self.blocks:
            if b["kind"] != "table":
                continue
            for r, row in enumerate(b.get("rows", [])):
                for c, cell in enumerate(row[:-1]):
                    if exact.match(cell["text"]):
                        val = row[c + 1]["text"]
                        s, e = _trim(val, 0, len(val))
                        if e > s:
                            return TextUnit(val, b["index"], r, c + 1).span(s, e)
        return None


def _trim(text: str, start: int, end: int) -> tuple[int, int]:
    while start < end and text[start] in " \t　":
        start += 1
    while end > start and text[end - 1] in " \t　。；;，,":
        end -= 1
    return start, end
