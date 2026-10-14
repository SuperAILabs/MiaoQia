"""中文公告常见取值的确定性解析：金额、时间。解析失败返回 None，由解析器决定降级或进入复核。"""
from __future__ import annotations

import re
from datetime import datetime, timedelta, timezone
from decimal import Decimal, InvalidOperation

UNITS = {"元": Decimal(1), "万元": Decimal(10_000), "亿元": Decimal(100_000_000)}
DIGIT = {"零": 0, "〇": 0, "壹": 1, "一": 1, "贰": 2, "二": 2, "两": 2, "叁": 3, "三": 3, "肆": 4, "四": 4,
         "伍": 5, "五": 5, "陆": 6, "六": 6, "柒": 7, "七": 7, "捌": 8, "八": 8, "玖": 9, "九": 9}
SMALL = {"拾": 10, "十": 10, "佰": 100, "百": 100, "仟": 1000, "千": 1000}
BIG = {"万": 10_000, "亿": 100_000_000}


def _upper_to_decimal(s: str) -> Decimal | None:
    """大写金额：叁佰贰拾万元整、壹仟零伍拾元伍角。"""
    s = s.replace("人民币", "").replace("整", "").replace("正", "").strip()
    if not s or any(ch not in DIGIT and ch not in SMALL and ch not in BIG and ch not in "元圆角分" for ch in s):
        return None
    if re.search(r"[元圆]", s):
        yuan_part, frac = re.split(r"[元圆]", s, maxsplit=1)
    else:
        yuan_part, frac = s, ""
    total, section, digit = 0, 0, 0
    for ch in yuan_part:
        if ch in DIGIT:
            digit = DIGIT[ch]
        elif ch in SMALL:
            section += (digit or 1) * SMALL[ch]
            digit = 0
        elif ch in BIG:
            section += digit
            total += section * BIG[ch]
            section, digit = 0, 0
    total += section + digit
    cents = Decimal(0)
    m = re.match(r"(?:([^角分])角)?(?:([^角分])分)?", frac or "")
    if m and m.group(1) in DIGIT:
        cents += Decimal(DIGIT[m.group(1)]) / 10
    if m and m.group(2) in DIGIT:
        cents += Decimal(DIGIT[m.group(2)]) / 100
    return Decimal(total) + cents


def parse_money(text: str, default_unit: str | None = None) -> tuple[str, str] | None:
    """返回 (十进制字符串（人民币元）, 币种)。支持“320 万元”“￥3,200,000.00”“3200000 元”与大写金额。

    原文未写单位时不猜测，返回 None；除非调用方给出 default_unit（例如表头“投标保证金（万元）”）。
    """
    if default_unit is not None and re.fullmatch(r"\s*[0-9][0-9,]*(?:\.[0-9]+)?\s*", text):
        try:
            return _fmt(Decimal(text.strip().replace(",", "")) * UNITS[default_unit]), "CNY"
        except InvalidOperation:
            return None
    t = text.strip().replace("，", ",").replace("（", "(").replace("）", ")")
    m = re.search(r"(?:人民币|￥|¥|RMB)?\s*([0-9][0-9,]*(?:\.[0-9]+)?)\s*(亿元|万元|元)", t)
    if not m:
        m2 = re.search(r"[￥¥]\s*([0-9][0-9,]*(?:\.[0-9]+)?)", t)
        if m2:
            m = m2
            unit = Decimal(1)
        else:
            up = _upper_to_decimal(re.sub(r"[\s(（].*$", "", t))
            return (_fmt(up), "CNY") if up is not None else None
    else:
        unit = UNITS[m.group(2)]
    try:
        value = Decimal(m.group(1).replace(",", "")) * unit
    except InvalidOperation:
        return None
    return _fmt(value), "CNY"


def _fmt(v: Decimal) -> str:
    s = format(v.normalize(), "f")
    return s.rstrip("0").rstrip(".") if "." in s else s


DATE = re.compile(
    r"(\d{4})\s*(?:年|-|/|\.)\s*(\d{1,2})\s*(?:月|-|/|\.)\s*(\d{1,2})\s*日?"
    r"(?:\s*(上午|下午)?\s*(\d{1,2})\s*(?:时|点|:|：)\s*(\d{1,2})?\s*(?:分)?\s*(?:(?::|：)\s*(\d{1,2})\s*秒?)?)?"
)


def parse_datetime(text: str, tz: str = "+08:00") -> tuple[str, str, str] | None:
    """返回 (UTC 时间 RFC 3339, 精度 day|minute|second, 原始时区)。精度为日时取原始时区当日零点。"""
    m = DATE.search(text)
    if not m:
        return None
    y, mo, d = int(m.group(1)), int(m.group(2)), int(m.group(3))
    ampm, hh, mm, ss = m.group(4), m.group(5), m.group(6), m.group(7)
    sign = 1 if tz[0] == "+" else -1
    offset = timezone(sign * timedelta(hours=int(tz[1:3]), minutes=int(tz[4:6])))
    try:
        if hh is None:
            local = datetime(y, mo, d, tzinfo=offset)
            precision = "day"
        else:
            h = int(hh)
            if ampm == "下午" and h < 12:
                h += 12
            local = datetime(y, mo, d, h, int(mm or 0), int(ss or 0), tzinfo=offset)
            precision = "second" if ss else "minute"
    except ValueError:
        return None
    utc = local.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    return utc, precision, tz
