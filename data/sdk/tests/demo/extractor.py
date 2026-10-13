"""仅用于测试 SDK 机制的演示解析器：按通用标签取几个字段。不针对任何真实来源，不作为 INF-PROC 解析规则。"""
from miaoqia.sdk import ExtractionResult


class DemoNoticeExtractor:
    id = "proc.demo"
    version = "0.0.1"
    fact_types = ("tendering.notice",)

    def applies(self, doc, source):
        return True

    def extract(self, doc, ctx):
        title = doc.find_labeled(["项目名称"])
        buyer = doc.find_labeled(["采购人"])
        budget = doc.find_labeled(["预算金额"])
        deadline = doc.find_labeled(["投标截止时间"])
        fact = {"notice_type": "tender", "title": ctx.text(title), "buyer": ctx.org(buyer),
                "lots": [{"lot_key": "1"}]}
        if budget and (m := ctx.money(budget)):
            fact["budget"] = m
        if deadline and (t := ctx.instant(deadline)):
            fact["lots"][0]["deadlines"] = [{"id": ctx.new_id("ddl", "1", "bid_submission"), "type": "bid_submission", "at": t}]
        return ExtractionResult(facts=[fact])
