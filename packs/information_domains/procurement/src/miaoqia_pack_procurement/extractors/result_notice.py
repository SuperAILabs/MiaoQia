"""结果公告解析器：中标（成交）结果公告、框架协议入围结果公告与中标候选人公示。

只依赖规范化文档，不依赖具体站点的页面结构；准确率以语料对照人工映射的评测为准（mq-corpus check --gold reviewed）。
输出 contracts/schemas/domains/procurement/notice.schema.json 中的领域字段（公共字段由平台补齐）。

按原文表达，不推断：
  - 候选人只记 stage=candidate 与名次，不记 is_winner；
  - 框架协议第一阶段入围记 result_kind=framework_shortlist、is_winner=false，不当作第二阶段成交；
  - “年单价合计”等非总价报价记 price_kind，不当作项目成交总额；
  - 中标人只在附件中（“详见附件”）时不伪造中标人，记 result_details_pending 与质量警告；
  - 标包未编号时记为 "1"；原文明确“多名中标人共享同一采购”而无包号时记为 unpartitioned（与人工映射一致）。
"""
from __future__ import annotations

import re
from decimal import Decimal, InvalidOperation

from miaoqia.sdk import Document, ExtractionContext, ExtractionResult
from miaoqia.sdk.document import Span, TextUnit

RESULT_TITLE = re.compile(r"(中标|成交|入围)[^\n]{0,12}(结果)?(公告|公示)|候选人公示")
CANDIDATE = re.compile(r"候选人公示")
NOT_RESULT = re.compile(r"更正|澄清|变更|补充|终止|废标|流标|失败|验收|合同")   # 结果的更正与其他阶段由各自解析器处理
CODE = re.compile(r"(?:项目编号|招标编号|采购编号)[：:]\s*([A-Za-z0-9][A-Za-z0-9_\-/]*)")
CN_NUM = {"一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6, "七": 7, "八": 8, "九": 9, "十": 10}
LOT_HEAD = re.compile(r"^第([一二三四五六七八九十\d]+)包[：:]?$")
FRAMEWORK_LOT = re.compile(r"^包(\d+)（排序及入围供应商名称）[：:]$")
RANKED_LINE = re.compile(r"^(\d{1,3})\s+(\S.{1,60})$")
PDF_WINNER = re.compile(r"中标人\d+[：:]\s*([^，,\n]{2,60})[，,]\s*投标报价[：:]\s*(年单价合计)?\s*([0-9][0-9.,]*\s*(?:亿元|万元|元))")
SUPPLIER = re.compile(r"^供应商名称[：:]\s*(.+?)\s*$")
AMOUNT = re.compile(r"^中标（成交）金额[：:]\s*(?:人民币)?\s*(.+?)\s*$")
PENDING = re.compile(r"详见附件")
VOIDED = re.compile(r"第([一二三四五六七八九十\d]+)包[^。]*?废标")
CONSORTIUM = re.compile(r"中标供应商([^，,。]{4,60})[，,]\s*联合体成员(?:单位)?为[：:]\s*([^。]{4,200})")
MONEY_PAREN = re.compile(r"^([0-9][0-9,]*(?:\.[0-9]+)?)\s*[（(](亿元|万元|元)[）)]$")
UNITS = {"元": Decimal(1), "万元": Decimal(10000), "亿元": Decimal(100000000)}


def _head(doc: Document) -> str:
    """标题区：前三个文本块各自的首行（PDF 文本层一页为一块，不能把正文当标题）。"""
    return "\n".join(b.get("text", "").split("\n", 1)[0] for b in doc.blocks[:3] if b["kind"] != "table")


def _lot_key(raw: str) -> str:
    return str(CN_NUM[raw]) if raw in CN_NUM else raw


class ResultNoticeExtractor:
    id = "proc.result-notice"
    version = "0.1.0"
    fact_types = ("tendering.notice",)
    gold_notice_types = ("award", "candidate")
    # 与人工映射对照评测的关键字段（mq-corpus check --gold reviewed）；* 匹配数组下标。
    gold_fields = (
        "/notice_type", "/title/value", "/project_code/value", "/buyer/name_raw", "/method",
        "/award_amount/value", "/result_details_pending",
        "/lots/*/lot_key", "/lots/*/ending/ending_type", "/lots/*/conditions/contract_pricing/value",
        "/lots/*/bid_results/*/stage", "/lots/*/bid_results/*/result_kind", "/lots/*/bid_results/*/is_winner",
        "/lots/*/bid_results/*/rank", "/lots/*/bid_results/*/bidder/name_raw", "/lots/*/bid_results/*/bidder/uscc",
        "/lots/*/bid_results/*/price/value", "/lots/*/bid_results/*/price_kind",
        "/lots/*/bid_results/*/consortium_members/*/name_raw",
    )

    def applies(self, doc: Document, source: dict) -> bool:
        head = _head(doc)
        return bool(RESULT_TITLE.search(head)) and not NOT_RESULT.search(head)

    def extract(self, doc: Document, ctx: ExtractionContext) -> ExtractionResult:
        res = ExtractionResult()
        head = _head(doc)
        candidate = bool(CANDIDATE.search(head))
        framework = "入围" in head and not candidate
        fact: dict = {"notice_type": "candidate" if candidate else "award"}
        if (title := self._title(doc)) is not None:
            fact["title"] = ctx.text(title)
        if (code := self._first(doc, CODE)) is not None:
            fact["project_code"] = ctx.text(code)
        if framework and self._first(doc, re.compile(r"框架协议采购")) is not None:
            fact["method"] = "framework"
        if (buyer := self._buyer(doc)) is not None:
            fact["buyer"] = ctx.org(buyer)
        warnings: list[dict] = []
        if candidate:
            lots = self._candidates(doc, ctx)
        elif framework:
            lots = self._framework(doc, ctx)
        else:
            lots = self._awards(doc, ctx, fact, warnings)
        fact["lots"] = lots or [{"lot_key": "1"}]
        if warnings:
            fact["quality"] = {"status": "warning", "warnings": warnings}
        if "title" not in fact:
            res.warnings.append({"code": "MISSING_KEY_FIELD", "message": "未识别标题"})
        if not any(lot.get("bid_results") for lot in fact["lots"]) and not fact.get("result_details_pending"):
            res.warnings.append({"code": "NO_RESULT", "message": "未识别任何结果"})
        res.facts.append(fact)
        return res

    # ---------- 通用 ----------
    def _title(self, doc: Document) -> Span | None:
        """首个文本块的第一行（站点页面为公告标题；PDF 文本层的首行为标题）。"""
        for b in doc.blocks[:2]:
            text = b.get("text", "")
            if b["kind"] == "table" or not text.strip():
                continue
            line = text.split("\n", 1)[0]
            unit = TextUnit(text, b["index"])
            start = len(line) - len(line.lstrip())
            end = len(line.rstrip())
            return unit.span(start, end) if end > start else None
        return None

    def _first(self, doc: Document, pattern: re.Pattern, group: int = 1) -> Span | None:
        for u in doc.units():
            if (m := pattern.search(u.text)):
                g = group if pattern.groups >= group else 0
                return u.span(m.start(g), m.end(g))
        return None

    def _buyer(self, doc: Document) -> Span | None:
        """招标人/采购人：“招标人：X”或“采购人信息”小节中的“名称/单位名称：X”。"""
        label = re.compile(r"(?:^|[\s.．、\d])(?:招\s*标\s*人|采\s*购\s*人)[：:]\s*([^\s，,。；;：:]{4,60})", re.M)
        for u in doc.units():
            if (m := label.search(u.text)):
                return u.span(m.start(1), m.end(1))
        section = False
        for b in doc.blocks:
            text = b.get("text", "")
            if b["kind"] == "table":
                continue
            if re.search(r"采购人信息", text):
                section = True
                continue
            if section:
                if (m := re.match(r"^\s*(?:单位)?名\s*称[：:]\s*(\S.{2,60}?)\s*$", text)):
                    return TextUnit(text, b["index"]).span(m.start(1), m.end(1))
                if re.search(r"代理机构信息|项目联系方式", text):
                    break
        return None

    def _money(self, ctx: ExtractionContext, span: Span, default_unit: str | None = None) -> dict | None:
        if (m := ctx.money(span, default_unit=default_unit)) is not None:
            return m
        if (p := MONEY_PAREN.match(span.text)):     # “284.8630000（万元）”
            try:
                value = Decimal(p.group(1).replace(",", "")) * UNITS[p.group(2)]
            except InvalidOperation:
                return None
            text = format(value.normalize(), "f")
            return {"value": text, "currency": "CNY", "raw": span.text, "prov": ctx.prov(span, 0.95)}
        return None

    def _result(self, ctx: ExtractionContext, lot_key: str, stage: str, bidder: Span, seq: int) -> dict:
        return {"id": ctx.new_id("bdr", lot_key, stage, str(seq), bidder.text), "stage": stage, "bidder": ctx.org(bidder)}

    # ---------- 中标（成交）结果 ----------
    def _awards(self, doc: Document, ctx: ExtractionContext, fact: dict, warnings: list[dict]) -> list[dict]:
        lots: dict[str, dict] = {}
        self._award_paragraphs(doc, ctx, lots, fact, warnings)
        if not lots:
            self._award_tables(doc, ctx, lots)
        if not lots:
            self._award_pdf_lines(doc, ctx, lots, warnings)
        self._consortium(doc, ctx, lots)
        self._voided(doc, ctx, lots)
        if fact.get("result_details_pending"):
            lots.setdefault("1", {"lot_key": "1"})
        return list(lots.values())

    def _award_paragraphs(self, doc, ctx, lots, fact, warnings) -> None:
        """“第N包：/供应商名称：X/中标（成交）金额：Y”；无包头时为未分包。"""
        current = None
        pending_result = None
        for b in doc.blocks:
            if b["kind"] == "table":
                continue
            text = b.get("text", "")
            if (m := LOT_HEAD.match(text.strip())):
                current = _lot_key(m.group(1))
                pending_result = None
                continue
            unit = TextUnit(text, b["index"])
            if (m := SUPPLIER.match(text)):
                if PENDING.search(m.group(1)):
                    fact["result_details_pending"] = True
                    warnings.append({"code": "RESULT_DETAILS_PENDING", "message": "中标人仅在附件中，不将“详见附件”当作组织"})
                    pending_result = None
                    continue
                key = current or "1"
                lot = lots.setdefault(key, {"lot_key": key})
                results = lot.setdefault("bid_results", [])
                pending_result = self._result(ctx, key, "award", unit.span(m.start(1), m.end(1)), len(results))
                pending_result.update(result_kind="contract_award", is_winner=True)
                results.append(pending_result)
                continue
            if (m := AMOUNT.match(text)):
                span = unit.span(m.start(1), m.end(1))
                if pending_result is not None and "price" not in pending_result:
                    if (money := self._money(ctx, span)) is not None:
                        pending_result["price"] = money
                        pending_result["price_kind"] = "total"
                        pending_result["price_kind_prov"] = money["prov"]
                elif fact.get("result_details_pending") and "award_amount" not in fact:
                    if (money := self._money(ctx, span)) is not None:
                        fact["award_amount"] = money

    def _award_tables(self, doc, ctx, lots) -> None:
        """结果表：表头含“供应商名称”与“中标/成交金额”列。"""
        for b in doc.blocks:
            if b["kind"] != "table":
                continue
            grid = doc.table_grid(b["index"])
            header = [s.text if s else "" for s in grid[0]]
            name_col = next((i for i, h in enumerate(header) if "供应商名称" in h or "中标人名称" in h), None)
            price_col = next((i for i, h in enumerate(header) if "金额" in h and ("中标" in h or "成交" in h)), None)
            if name_col is None or price_col is None:
                continue
            lot_col = next((i for i, h in enumerate(header) if h in ("包号", "包件号", "标包号")), None)
            uscc_col = next((i for i, h in enumerate(header) if "信用代码" in h), None)
            addr_col = next((i for i, h in enumerate(header) if "地址" in h), None)
            for row in grid[1:]:
                name = row[name_col]
                if name is None or PENDING.search(name.text):
                    continue
                key = _lot_key(row[lot_col].text) if lot_col is not None and row[lot_col] else "1"
                lot = lots.setdefault(key, {"lot_key": key})
                results = lot.setdefault("bid_results", [])
                result = self._result(ctx, key, "award", name, len(results))
                result.update(result_kind="contract_award", is_winner=True)
                if uscc_col is not None and (u := row[uscc_col]) and re.fullmatch(r"[0-9A-HJ-NPQRTUWXY]{2}[0-9]{6}[0-9A-HJ-NPQRTUWXY]{10}", u.text):
                    result["bidder"]["uscc"] = u.text
                    result["bidder"]["uscc_prov"] = ctx.prov(u, 0.95)
                if addr_col is not None and (a := row[addr_col]):
                    result["bidder"]["address"] = ctx.text(a)
                if (p := row[price_col]) and (money := self._money(ctx, p, default_unit="元" if "元" in header[price_col] else None)):
                    result["price"] = money
                    result["price_kind"] = "total"
                    result["price_kind_prov"] = money["prov"]
                results.append(result)

    def _award_pdf_lines(self, doc, ctx, lots, warnings) -> None:
        """文本层中的“中标人N：X，投标报价：[年单价合计]Y”（多名中标人共享同一采购、未分包）。"""
        found = []
        for u in doc.units():
            for m in PDF_WINNER.finditer(u.text):
                found.append((u, m))
        if not found:
            return
        key = "unpartitioned" if len(found) > 1 else "1"
        lot = lots.setdefault(key, {"lot_key": key})
        results = lot.setdefault("bid_results", [])
        unit_sum = False
        for u, m in found:
            result = self._result(ctx, key, "award", u.span(m.start(1), m.end(1)), len(results))
            result.update(result_kind="contract_award", is_winner=True)
            price_span = u.span(m.start(3), m.end(3))
            if (money := self._money(ctx, price_span)) is not None:
                result["price"] = money
                kind_span = u.span(m.start(2), m.end(2)) if m.group(2) else price_span
                result["price_kind"] = "annual_unit_sum" if m.group(2) else "total"
                result["price_kind_prov"] = ctx.prov(kind_span, 0.95)
                unit_sum = unit_sum or bool(m.group(2))
            results.append(result)
        for u in doc.units():
            if (m := re.search(r"中标签约价[：:][^\n]*", u.text)):
                lot.setdefault("conditions", {})["contract_pricing"] = ctx.text(u.span(m.start(), m.end()), 0.9)
                break
        if unit_sum:
            warnings.append({"code": "UNIT_SUM_NOT_TOTAL", "message": "报价为年单价合计，不能当项目总额或累加为总成交额"})

    def _consortium(self, doc, ctx, lots) -> None:
        for u in doc.units():
            if not (m := CONSORTIUM.search(u.text)):
                continue
            lead = m.group(1).strip()
            for lot in lots.values():
                for result in lot.get("bid_results", []):
                    if result["bidder"]["name_raw"] != lead:
                        continue
                    members, offset = [], m.start(2)
                    for part in re.split(r"[、，,]", m.group(2)):
                        start = u.text.index(part, offset)
                        offset = start + len(part)
                        if part.strip():
                            s = start + len(part) - len(part.lstrip())
                            members.append(ctx.org(u.span(s, s + len(part.strip()))))
                    if members:
                        result["consortium_members"] = members

    def _voided(self, doc, ctx, lots) -> None:
        """“第N包……废标”：仅该标包废标，不传播到其他标包。"""
        for u in doc.units():
            for m in VOIDED.finditer(u.text):
                key = _lot_key(m.group(1))
                lot = lots.get(key)
                if lot is None:
                    lot = lots.setdefault(key, {"lot_key": key})
                if lot.get("bid_results"):
                    continue
                sentence = re.search(r"[^：:。]*" + re.escape(m.group(0)) + r"[^。]*。?", u.text)
                s, e = (sentence.start(), sentence.end()) if sentence else (m.start(), m.end())
                lot["ending"] = {"ending_type": "voided", "lot_keys": [key], "reason": ctx.text(u.span(s, e), 0.9)}

    # ---------- 框架协议入围 ----------
    def _framework(self, doc: Document, ctx: ExtractionContext) -> list[dict]:
        lots: dict[str, dict] = {}
        current = None
        for b in doc.blocks:
            if b["kind"] == "table":
                continue
            text = b.get("text", "")
            unit = TextUnit(text, b["index"])
            if (m := FRAMEWORK_LOT.match(text.strip())):
                current = m.group(1)
                lots.setdefault(current, {"lot_key": current, "bid_results": []})
                continue
            if current is None:
                continue
            if (m := re.match(rf"^包{current}最高入围价格[：:].+$", text.strip())):
                lots[current].setdefault("conditions", {})["contract_pricing"] = ctx.text(unit.span(m.start(), m.end()), 0.9)
                current = None
                continue
            if (m := RANKED_LINE.match(text)):
                results = lots[current]["bid_results"]
                name = unit.span(m.start(2), m.end(2))
                result = self._result(ctx, current, "award", name, len(results))
                result.update(result_kind="framework_shortlist", is_winner=False, rank=int(m.group(1)))
                results.append(result)
            elif text.strip():
                current = None
        return [lot for lot in lots.values() if lot["bid_results"]]

    # ---------- 中标候选人 ----------
    def _candidates(self, doc: Document, ctx: ExtractionContext) -> list[dict]:
        """候选人表：首列为“名次/中标候选人名称/投标报价”等行标签，其后各列为第一名、第二名……"""
        for b in doc.blocks:
            if b["kind"] != "table":
                continue
            grid = doc.table_grid(b["index"])
            label = {(row[0].text.replace(" ", "") if row[0] else ""): r for r, row in enumerate(grid)}
            name_row = next((r for k, r in label.items() if "候选人名称" in k), None)
            if name_row is None:
                continue
            rank_row = next((r for k, r in label.items() if k in ("名次", "排名", "排序")), None)
            price_row = next((r for k, r in label.items() if "报价" in k), None)
            unit = "元" if price_row is not None and "元" in label_text(grid[price_row][0]) else None
            results = []
            for c in range(1, len(grid[name_row])):
                name = grid[name_row][c]
                if name is None or name.col != c:          # 合并单元格覆盖的位置不是新的候选人
                    continue
                rank = len(results) + 1
                if rank_row is not None and grid[rank_row][c] is not None:
                    raw = grid[rank_row][c].text.replace("第", "").replace("名", "")
                    rank = CN_NUM.get(raw, int(raw) if raw.isdigit() else rank)
                result = self._result(ctx, "1", "candidate", name, len(results))
                result["rank"] = rank
                if price_row is not None and (p := grid[price_row][c]) is not None:
                    if (money := self._money(ctx, p, default_unit=unit)) is not None:
                        result["price"] = money
                results.append(result)
            if results:
                return [{"lot_key": "1", "bid_results": results}]
        return []


def label_text(span: Span | None) -> str:
    return span.text if span else ""
