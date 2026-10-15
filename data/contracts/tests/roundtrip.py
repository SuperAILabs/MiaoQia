"""把契约正例（mqc fixtures --export 导出）解析为生成的 Pydantic 模型，发现生成模型与 Schema 的偏差。

用法：python data/contracts/tests/roundtrip.py DIR
"""
import json
import sys
from pathlib import Path

from miaoqia.contracts import models

failed = 0
files = sorted(Path(sys.argv[1]).glob("*.json"))
if not files:
    sys.exit("没有找到样例")
for f in files:
    cls = getattr(models, f.name.split("__", 1)[0], None)
    if cls is None:
        print(f"✗ {f.name}：生成模型中没有该类型")
        failed += 1
        continue
    try:
        original = json.loads(f.read_text(encoding="utf-8"))
        model = cls.model_validate(original)
        encoded = json.loads(model.model_dump_json(exclude_unset=True))
        if encoded != original:
            raise ValueError("往返改变了字段或值")
    except Exception as e:  # noqa: BLE001
        print(f"✗ {f.name}：{str(e)[:300]}")
        failed += 1
print(f"往返测试：{len(files)} 个正例，{failed} 个失败")
sys.exit(1 if failed else 0)
