"""miaoqia.sdk：信息领域包的扩展接口（docs/03-architecture/04-extensibility.md 第 3.1 节）。

解析器只能读取规范化文档；产出的每个字段都必须经由 ExtractionContext 取得证据引用，
从而满足“每个解析字段都可回到原文”。
"""
from .document import Document, Span
from .extraction import ExtractionContext, ExtractionResult, Extractor

__all__ = ["Document", "Span", "ExtractionContext", "ExtractionResult", "Extractor"]
