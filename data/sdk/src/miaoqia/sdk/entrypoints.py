"""扩展包入口的静态登记与加载后校验：格式、位置与签名的唯一定义。

只用标准库，不导入领域包代码：语料加载（corpus）直接调用，契约检查（mqc）按文件路径加载。
"""
from __future__ import annotations

import ast
import inspect
import re
from pathlib import Path

ENTRY_PATTERN = re.compile(r"[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*:[A-Za-z_]\w*")


def resolve_semantic_validator(pack_dir: Path, ref) -> tuple[str, str, Path]:
    """返回（模块名、函数名、源文件）；入口不合规时抛出 ValueError。"""
    if not isinstance(ref, str) or not ENTRY_PATTERN.fullmatch(ref):
        raise ValueError(f"领域校验入口格式无效：{ref}")
    module, function = ref.split(":")
    root = (pack_dir / "src").resolve()
    source = (root / Path(*module.split("."))).with_suffix(".py").resolve()
    if not source.is_relative_to(root) or not source.is_file():
        raise ValueError(f"领域校验入口不存在：{ref}")
    try:
        tree = ast.parse(source.read_text())
    except SyntaxError as exc:
        raise ValueError(f"领域校验模块语法错误：{ref}") from exc
    # 静态定义以最后一个为准；装饰器和重绑定后的实际函数另经加载后校验。
    node = {n.name: n for n in tree.body if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))}.get(function)
    if node is None:
        raise ValueError(f"领域校验函数不存在：{ref}")
    if isinstance(node, ast.AsyncFunctionDef):
        raise ValueError(f"领域校验入口必须是同步函数：{ref}")
    args = node.args
    count = len(args.posonlyargs) + len(args.args)
    if count - len(args.defaults) > 3 or (count < 3 and args.vararg is None) or any(d is None for d in args.kw_defaults):
        raise ValueError(f"领域校验函数必须接受document/facts/evidence：{ref}")
    return module, function, source


def validate_loaded_semantic_validator(callback, source: Path, ref: str) -> None:
    """校验实际函数，不执行它；装饰器或重绑定不能绕过入口契约。"""
    if not inspect.isfunction(callback) or Path(inspect.getsourcefile(callback) or "").resolve() != source.resolve():
        raise ValueError(f"领域校验函数来源与登记包不一致：{ref}")
    if inspect.iscoroutinefunction(callback) or inspect.isasyncgenfunction(callback):
        raise ValueError(f"领域校验入口必须是同步函数：{ref}")
    try:
        # 不跟随 functools.wraps 的 __wrapped__：实际执行的是包装函数。
        inspect.signature(callback, follow_wrapped=False).bind(None, None, None)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"领域校验函数必须接受document/facts/evidence：{ref}") from exc
