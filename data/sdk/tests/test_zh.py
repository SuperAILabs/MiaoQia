import pytest

from miaoqia.sdk.zh import parse_datetime, parse_money


@pytest.mark.parametrize("text,want", [
    ("320万元", "3200000"),
    ("预算金额：320.5 万元（含税）", "3205000"),
    ("￥3,200,000.00", "3200000"),
    ("人民币 3,200,000.00 元", "3200000"),
    ("1.5亿元", "150000000"),
    ("人民币叁佰贰拾万元整", "3200000"),
    ("壹仟零伍拾元伍角", "1050.5"),
    ("壹亿贰仟万元", "120000000"),
    ("2980000.50元", "2980000.5"),
])
def test_money(text, want):
    assert parse_money(text) == (want, "CNY")


@pytest.mark.parametrize("text", ["3200000", "详见附件", ""])
def test_money_without_unit_is_not_guessed(text):
    assert parse_money(text) is None


@pytest.mark.parametrize("text,value,precision", [
    ("2026年11月20日 09:30", "2026-11-20T01:30:00Z", "minute"),
    ("2026-11-20 09:30:15", "2026-11-20T01:30:15Z", "second"),
    ("2026年11月20日上午9时30分（北京时间）", "2026-11-20T01:30:00Z", "minute"),
    ("2026年11月20日下午2点", "2026-11-20T06:00:00Z", "minute"),
    ("2026/11/20", "2026-11-19T16:00:00Z", "day"),
    ("截止时间：2026.11.20", "2026-11-19T16:00:00Z", "day"),
])
def test_datetime(text, value, precision):
    assert parse_datetime(text) == (value, precision, "+08:00")


def test_invalid_date():
    assert parse_datetime("2026年2月30日") is None


def test_money_with_default_unit_from_header():
    assert parse_money("20", default_unit="万元") == ("200000", "CNY")
    assert parse_money("500", default_unit="元") == ("500", "CNY")
    assert parse_money("20") is None                       # 没有单位来源时仍不猜测
