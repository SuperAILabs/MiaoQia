"""招标公告解析器：面向“编号章节 + 标签：取值 + 附件包件表”结构的招标公告。

只依赖规范化文档，不依赖具体站点的页面结构；准确率以语料评测为准（mq-corpus check）。
输出 contracts/schemas/domains/procurement/notice.schema.json 中的领域字段（公共字段由平台补齐）。
"""
from __future__ import annotations

import re

from miaoqia.sdk import Document, ExtractionContext, ExtractionResult
from miaoqia.sdk.document import Span

TITLE_MARK = re.compile(r"^(招标公告|采购公告|招标采购公告|公开招标公告)$")
SECTION = re.compile(r"^\d+[.．、](?!\d)\s*\S")                # 一级章节“5.招标文件的获取”，不含“5.1”
METHODS = [("邀请招标", "invited_tender"), ("公开招标", "open_tender"), ("竞争性谈判", "negotiation"),
           ("竞争性磋商", "consultation"), ("询价", "inquiry"), ("单一来源", "single_source")]
REQ_KINDS = [("营业范围", "qualification"), ("资质", "qualification"), ("财务", "financial"),
             ("质量", "quality"), ("信用", "credit"), ("信誉", "credit"), ("业绩", "performance"),
             ("人员", "personnel"), ("授权", "authorization"), ("联合体", "consortium")]
MOBILE = re.compile(r"1[3-9]\d{9}|0\d{2,3}-?\d{7,8}")
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
DATE_ONLY = re.compile(r"^\d{4}\s*年\s*\d{1,2}\s*月\s*\d{1,2}\s*日$")


class TenderNoticeExtractor:
    id = "proc.tender-notice"
    version = "0.3.0"
    fact_types = ("tendering.notice",)

    def applies(self, doc: Document, source: dict) -> bool:
        return any(TITLE_MARK.match(b.get("text", "")) or b.get("text", "").endswith("非政府采购招标公告") for b in doc.blocks[:5])

    def extract(self, doc: Document, ctx: ExtractionContext) -> ExtractionResult:
        res = ExtractionResult()
        fact: dict = {"notice_type": "tender"}
        title = self._title(doc)
        if title:
            fact["title"] = ctx.text(title)
        for key, labels in (("notice_code", ["招标编号", "项目编号", "采购编号"]),):
            if (s := doc.find_labeled(labels)):
                fact[key] = ctx.text(s)
        if (s := doc.find_labeled(["招标方式", "采购方式"])):
            for word, code in METHODS:
                if word in s.text:
                    fact["method"] = code
                    break
        if (s := doc.find_labeled(["项目编号"])):
            fact["project_code"] = ctx.text(s)
        for key, labels in (("buyer", ["招标人", "采购人", "采购人名称"]),
                            ("agency", ["招标组织单位", "招标代理机构", "采购代理机构", "代理机构"])):
            if (s := doc.find_labeled(labels)):
                fact[key] = ctx.org(s)
        if (s := self._owner(doc)):
            fact["project_owner"] = ctx.org(s, confidence=0.9)
        if (s := self._signature_date(doc)) and (t := ctx.instant(s)):
            fact["published_at"] = t
        fact["lots"] = self._lots(doc, ctx, res)
        contacts = self._contacts(doc, ctx)
        if contacts:
            fact["contacts"] = contacts
        if "title" not in fact or "buyer" not in fact:
            res.warnings.append({"code": "MISSING_KEY_FIELD", "message": "未识别标题或招标人"})
        res.facts.append(fact)
        return res

    # ---------- 字段 ----------
    def _title(self, doc: Document) -> Span | None:
        """标题标记（“招标公告”）之后、第一行“标签：”之前的居中标题行，可能跨多块。"""
        mark = next((b["index"] for b in doc.blocks[:5] if TITLE_MARK.match(b.get("text", ""))), None)
        if mark is None:
            title = next((b for b in doc.blocks[:5] if b.get("text", "").endswith("非政府采购招标公告")), None)
            return doc.block_span(title["index"]) if title else None
        end = mark
        for b in doc.blocks[mark + 1: mark + 4]:
            text = b.get("text", "")
            if not text or "：" in text or ":" in text or SECTION.match(text):
                break
            end = b["index"]
        return doc.join_blocks(mark + 1, end) if end > mark else None

    def _owner(self, doc: Document) -> Span | None:
        for u in doc.units():
            m = re.search(r"建设单位(?:为|：|:)\s*([^，,。；;（(]{4,40})", u.text)
            if m:
                return u.span(m.start(1), m.end(1))
        return None

    def _signature_date(self, doc: Document) -> Span | None:
        """落款日期：附件之前最后一个只有日期的段落。"""
        last = None
        for b in doc.blocks:
            text = b.get("text", "")
            if text.startswith("附件"):
                break
            if b["kind"] == "paragraph" and DATE_ONLY.match(text.strip()):
                last = doc.block_span(b["index"])
        return last

    def _deadlines(self, doc: Document, ctx: ExtractionContext, lot_key: str) -> list[dict]:
        out = []
        for typ, labels in (("bid_submission", ["投标文件递交截止时间", "投标截止时间", "递交投标文件截止时间"]),
                            ("bid_opening", ["开标时间", "电子投标文件解密时间", "解密时间"]),
                            ("doc_obtain_end", ["招标文件售卖截止时间为", "招标文件获取截止时间", "招标文件售卖截止时间"])):
            s = doc.find_labeled(labels)
            if s and (t := ctx.instant(s)):
                out.append({"id": ctx.new_id("ddl", lot_key, typ), "type": typ, "at": t})
        return out

    def _requirements(self, doc: Document, ctx: ExtractionContext, lot_key: str) -> list[dict]:
        """资格要求章节中的“（n）类别：要求”条目。"""
        start = next((b["index"] for b in doc.blocks if b["kind"] != "table" and "资格" in b.get("text", "")
                      and SECTION.match(b.get("text", ""))), None)
        if start is None:
            return []
        out = []
        for b in doc.blocks[start + 1:]:
            text = b.get("text", "")
            if b["kind"] == "table" or SECTION.match(text):
                break
            m = re.match(r"^[（(]\s*\d+\s*[）)]\s*([^：:]{2,10})[：:]", text)
            if not m:
                continue
            label = m.group(1)
            kind = next((k for word, k in REQ_KINDS if word in label), "other")
            out.append({"id": ctx.new_id("req", lot_key, str(b["index"])), "kind": kind,
                        "text": ctx.text(doc.block_span(b["index"]), confidence=0.9)})
        return out

    def _lots(self, doc: Document, ctx: ExtractionContext, res: ExtractionResult) -> list[dict]:
        consortium = None
        for u in doc.units():
            if "不接受联合体" in u.text:
                consortium = False
                break
            if re.search(r"(?<!不)接受联合体", u.text):
                consortium = True
        lots: dict[str, dict] = {}
        for b in doc.blocks:
            if b["kind"] != "table":
                continue
            grid = doc.table_grid(b["index"])
            header = [s.text if s else "" for s in grid[0]]
            col = {name: i for i, name in enumerate(header)}
            if "包件号" not in col or "物资名称" not in col:
                continue
            fee_col = next((i for i, h in enumerate(header) if "招标文件售价" in h), None)
            bond_col = next((i for i, h in enumerate(header) if "投标保证金" in h), None)
            bond_unit = "万元" if bond_col is not None and "万元" in header[bond_col] else "元"
            total = None
            for row in grid[1:]:
                first = row[0].text if row[0] else ""
                if first.startswith("合计"):
                    qcol = col.get("包件数量", col.get("数量"))
                    total = row[qcol].text if qcol is not None and row[qcol] else None
                    continue
                key = row[col["包件号"]].text if row[col["包件号"]] else "1"
                lot = lots.setdefault(key, {"lot_key": key, "items": []})
                name = row[col["物资名称"]]
                if name is None:
                    continue
                item = {"id": ctx.new_id("itm", key, str(len(lot["items"]))), "name": ctx.text(name)}
                if "规格型号" in col and row[col["规格型号"]]:
                    item["spec"] = ctx.text(row[col["规格型号"]])
                qcol = col.get("包件数量", col.get("数量"))
                if qcol is not None and row[qcol]:
                    unit = row[col["计量单位"]] if "计量单位" in col else None
                    if (q := ctx.quantity(row[qcol], unit)):
                        item["quantity"] = q
                lot["items"].append(item)
                if fee_col is not None and row[fee_col] and "document_fee" not in lot:
                    if (m := ctx.money(row[fee_col], default_unit="元")):
                        lot["document_fee"] = m
                if bond_col is not None and row[bond_col] and "bid_bond" not in lot:
                    if (m := ctx.money(row[bond_col], default_unit=bond_unit)):
                        lot["bid_bond"] = m
            if total is not None:
                got = sum(float(i["quantity"]["value"]) for lt in lots.values() for i in lt["items"] if "quantity" in i)
                if abs(got - float(total)) > 1e-6:
                    res.warnings.append({"code": "QUANTITY_TOTAL_MISMATCH", "message": f"数量合计 {got} 与表中合计 {total} 不一致"})
        if not lots:
            lots["1"] = {"lot_key": "1"}
        for key, lot in lots.items():
            if consortium is not None:
                lot["consortium_allowed"] = consortium
            if (dl := self._deadlines(doc, ctx, key)):
                lot["deadlines"] = dl
            if (rq := self._requirements(doc, ctx, key)):
                lot["requirements"] = rq
        return list(lots.values())

    def _contacts(self, doc: Document, ctx: ExtractionContext) -> list[dict]:
        """“招标人……联系方式”与“招标组织单位/代理……联系方式”小节中的联系人、电话、邮箱。"""
        out: list[dict] = []
        role = None
        cur: dict | None = None
        for b in doc.blocks:
            text = b.get("text", "")
            if b["kind"] == "table":
                break
            if "联系方式" in text and len(text) <= 30:
                if "招标人" in text and "相关方" not in text:
                    role = "buyer"
                elif "组织" in text or "代理" in text:
                    role = "agency"
                cur = None
                continue
            if role is None:
                continue
            if (m := re.search(r"联系人[：:]\s*([一-龥·]{2,4})", text)):
                cur = {"role": role, "name": m.group(1), "name_prov": ctx.prov(Span(m.group(1), b["index"], m.start(1), m.end(1)), 0.95)}
                out.append(cur)
            if cur is not None:
                if (m := MOBILE.search(text)) and "phone" not in cur and "技术支持" not in text:
                    cur["phone"] = m.group(0)
                    cur["phone_prov"] = ctx.prov(Span(m.group(0), b["index"], m.start(), m.end()), 0.95)
                if (m := EMAIL.search(text)) and "email" not in cur:
                    cur["email"] = m.group(0)
                    cur["email_prov"] = ctx.prov(Span(m.group(0), b["index"], m.start(), m.end()), 0.95)
        return out
