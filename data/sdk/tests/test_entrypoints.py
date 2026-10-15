import pytest

from miaoqia.sdk.entrypoints import resolve_semantic_validator


def _pack(tmp_path, source):
    (tmp_path / "src").mkdir()
    (tmp_path / "src" / "rules.py").write_text(source)
    return tmp_path


def test_valid_entry_resolves_to_pack_source(tmp_path):
    pack = _pack(tmp_path, "def validate(document, facts, evidence):\n    return []\n")
    module, function, source = resolve_semantic_validator(pack, "rules:validate")
    assert (module, function) == ("rules", "validate")
    assert source == (pack / "src" / "rules.py").resolve()


@pytest.mark.parametrize("ref, message", [
    (None, "格式无效"), ("bad/path:validate", "格式无效"),
    ("absent:validate", "入口不存在"), ("rules:missing", "函数不存在"),
])
def test_invalid_entries_fail(tmp_path, ref, message):
    pack = _pack(tmp_path, "def validate(document, facts, evidence):\n    return []\n")
    with pytest.raises(ValueError, match=message):
        resolve_semantic_validator(pack, ref)


@pytest.mark.parametrize("source, message", [
    ("async def validate(document, facts, evidence):\n    return []\n", "同步函数"),
    ("def validate(document, facts, evidence):\n    return []\ndef validate(doc):\n    return []\n", "document/facts/evidence"),
    ("def validate(:\n", "语法错误"),
])
def test_signature_follows_runtime_definition(tmp_path, source, message):
    with pytest.raises(ValueError, match=message):
        resolve_semantic_validator(_pack(tmp_path, source), "rules:validate")
