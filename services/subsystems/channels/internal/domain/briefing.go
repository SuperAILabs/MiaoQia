package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// 简报的周期、发送、截止与卡片渲染规则（PRD 第5段 F1 第2条、F2 第2条、F3、F9、第9节、第13节第4、7、11、12项）。

const (
	// ActionTokenTTL 是卡片按钮动作令牌的有效期（第13节第7项）。
	ActionTokenTTL = 24 * time.Hour
	// UndoTokenTTL 是卡片“撤销”的有效期（第13节第5项）。
	UndoTokenTTL = 10 * time.Minute
	// BriefingLinkTTL 是简报与卡片上链接的有效期（第13节第7项）。
	BriefingLinkTTL = 7 * 24 * time.Hour
	// MaxActionAttempts 是一次点击内对业务接口的最多尝试次数（F5 第4条）。
	MaxActionAttempts = 3
	// CardsPerSection 与 CardsPerBriefing 是每区与每期推送的信号卡片上限（第13节第1项）。
	CardsPerSection  = 3
	CardsPerBriefing = 6
	// MaxWindow 是一区覆盖窗口的最长跨度（detection listBriefingSignals 的上限）。
	MaxWindow = 62 * 24 * time.Hour
	// DefaultTimezone 是租户时区的缺省值。
	DefaultTimezone = "Asia/Shanghai"
)

// Preferences 是一个偏好版本（版本 0 为缺省值）。EffectiveFrom 为空表示自始生效。
type Preferences struct {
	Version       int64
	SendTime      string // HH:MM
	Timezone      string
	SendDays      []int // 1（周一）–7（周日）
	QuietStart    string
	QuietEnd      string
	PushEmpty     bool
	SectionLimits map[string]int
	EffectiveFrom *time.Time
}

// Defaults 是从未保存时的偏好（第13节第4项）。
func Defaults() Preferences {
	return Preferences{SendTime: "07:30", Timezone: DefaultTimezone, SendDays: []int{1, 2, 3, 4, 5}, QuietStart: "21:00", QuietEnd: "07:00",
		SectionLimits: map[string]int{}}
}

// ParseClock 解析 HH:MM。
func ParseClock(v string) (h, m int, err error) {
	if len(v) != 5 || v[2] != ':' {
		return 0, 0, fmt.Errorf("invalid clock %q", v)
	}
	if _, err := fmt.Sscanf(v, "%02d:%02d", &h, &m); err != nil || h > 23 || m > 59 {
		return 0, 0, fmt.Errorf("invalid clock %q", v)
	}
	return h, m, nil
}

// ValidatePreferences 校验偏好（F9）：发送时间为 06:00–10:00 的整点或半点；安静时段为整点或半点；时区可解析；发送日非空且不重复；
// 各目标区条数在 limits 声明的范围内（limits 为目标编号 → [min, max]）。
func ValidatePreferences(p Preferences, limits map[string][2]int) error {
	h, m, err := ParseClock(p.SendTime)
	if err != nil || (m != 0 && m != 30) || h < 6 || h > 10 || (h == 10 && m != 0) {
		return fmt.Errorf("send_time")
	}
	for _, q := range []string{p.QuietStart, p.QuietEnd} {
		if _, m, err := ParseClock(q); err != nil || (m != 0 && m != 30) {
			return fmt.Errorf("quiet")
		}
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil || p.Timezone == "" || p.Timezone == "Local" {
		return fmt.Errorf("timezone")
	}
	if len(p.SendDays) == 0 {
		return fmt.Errorf("send_days")
	}
	seen := map[int]bool{}
	for _, d := range p.SendDays {
		if d < 1 || d > 7 || seen[d] {
			return fmt.Errorf("send_days")
		}
		seen[d] = true
	}
	for code, n := range p.SectionLimits {
		r, ok := limits[code]
		if !ok || n < r[0] || n > r[1] {
			return fmt.Errorf("section_limits")
		}
	}
	return nil
}

func (p Preferences) location() *time.Location {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		loc, _ = time.LoadLocation(DefaultTimezone)
	}
	return loc
}

// Location 返回偏好的时区（不可解析时为租户缺省时区）。
func (p Preferences) Location() *time.Location { return p.location() }

// sendBefore 返回按该偏好的计划发送中早于 t 的最近一次（最多回看 15 天；没有时为零值）。
func (p Preferences) sendBefore(t time.Time) time.Time {
	h, m, err := ParseClock(p.SendTime)
	if err != nil {
		return time.Time{}
	}
	loc := p.location()
	local := t.In(loc)
	for i := 0; i <= 15; i++ {
		d := local.AddDate(0, 0, -i)
		wd := int(d.Weekday())
		if wd == 0 {
			wd = 7
		}
		if !slices.Contains(p.SendDays, wd) {
			continue
		}
		at := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
		if at.Before(t) {
			return at
		}
	}
	return time.Time{}
}

// Schedule 是一名成员的偏好版本序列（按版本升序；第一个版本的 EffectiveFrom 为空，即缺省值自始生效）。
type Schedule []Preferences

// at 返回在时刻 t 之前（不含 t）生效的版本：最后一个 EffectiveFrom < t 的版本。
func (s Schedule) at(t time.Time) Preferences {
	cur := s[0]
	for _, p := range s[1:] {
		if p.EffectiveFrom != nil && p.EffectiveFrom.Before(t) {
			cur = p
		}
	}
	return cur
}

// sameDay 判断两个时刻在 loc 中是否同一天。
func sameDay(a, b time.Time, loc *time.Location) bool {
	x, y := a.In(loc), b.In(loc)
	return x.Year() == y.Year() && x.YearDay() == y.YearDay()
}

// prevBoundary 返回早于 t 的最近一个周期边界：生效版本的计划发送时间，或版本的生效起点（它本身是上一版本的一次计划发送）。
// 新版本在生效起点当天不再发送（改晚发送时间不会使同一天有两期简报）。
func (s Schedule) prevBoundary(t time.Time) time.Time {
	p := s.at(t)
	b := p.sendBefore(t)
	if p.EffectiveFrom != nil && (!b.After(*p.EffectiveFrom) || sameDay(b, *p.EffectiveFrom, p.location())) {
		return *p.EffectiveFrom
	}
	return b
}

// PeriodAt 返回时刻 at 时最近一个已到计划发送时间的周期 [start, end)：end 为不晚于 at 的最近一次计划发送，start 为它之前的
// 一次（F1 第2条：周期是上一次计划发送到本次计划发送，周一从上周五起；修改偏好从下一期生效，新旧周期首尾相接）。
// 第二个返回值是 end 所用的偏好版本（周期以 end 时生效的偏好为准）。
func (s Schedule) PeriodAt(at time.Time) (start, end time.Time, p Preferences, ok bool) {
	end = s.prevBoundary(at.Add(time.Nanosecond))
	if end.IsZero() {
		return time.Time{}, time.Time{}, Preferences{}, false
	}
	start = s.prevBoundary(end)
	if start.IsZero() {
		return time.Time{}, time.Time{}, Preferences{}, false
	}
	return start, end, s.at(end), true
}

// NextBoundary 返回晚于 t 的第一个周期边界（修改偏好时新版本的生效起点：旧偏好下修改时刻之后的第一个计划发送）。
func (s Schedule) NextBoundary(t time.Time) time.Time {
	p := s.at(t.Add(time.Nanosecond))
	h, m, err := ParseClock(p.SendTime)
	if err != nil {
		return time.Time{}
	}
	loc := p.location()
	local := t.In(loc)
	for i := 0; i <= 15; i++ {
		d := local.AddDate(0, 0, i)
		wd := int(d.Weekday())
		if wd == 0 {
			wd = 7
		}
		if !slices.Contains(p.SendDays, wd) {
			continue
		}
		at := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
		if at.After(t) && (p.EffectiveFrom == nil || !sameDay(at, *p.EffectiveFrom, loc)) {
			return at
		}
	}
	return time.Time{}
}

// localClock 返回 t 在 loc 中当天的某个钟点。
func localClock(t time.Time, loc *time.Location, h, m int) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), h, m, 0, 0, loc)
}

// ComposeLate 判断组装是否晚于周期终点当日（成员时区）12:00（第13节第12项：之后不推送，下一期窗口顺延）。
func ComposeLate(end, at time.Time, loc *time.Location) bool {
	return !at.Before(localClock(end, loc, 12, 0))
}

// DeliveryCutoff 是投递停止渠道重试的时刻：周期终点当日（成员时区）18:00（第13节第11项）。
func DeliveryCutoff(end time.Time, loc *time.Location) time.Time { return localClock(end, loc, 18, 0) }

// QuietUntil 返回 t 落在安静时段内时安静时段的结束时刻；不在安静时段内时为零值。安静时段可以跨午夜（如 21:00–07:00）。
func QuietUntil(p Preferences, t time.Time) time.Time {
	sh, sm, err1 := ParseClock(p.QuietStart)
	eh, em, err2 := ParseClock(p.QuietEnd)
	if err1 != nil || err2 != nil || (sh == eh && sm == em) {
		return time.Time{}
	}
	loc := p.location()
	start, end := localClock(t, loc, sh, sm), localClock(t, loc, eh, em)
	if !end.After(start) { // 跨午夜
		if t.Before(end) {
			return end
		}
		if !t.Before(start) {
			return end.AddDate(0, 0, 1)
		}
		return time.Time{}
	}
	if !t.Before(start) && t.Before(end) {
		return end
	}
	return time.Time{}
}

// Backoff 是投递第 attempt 次失败后的退避（1、2、4… 分钟，至多 30 分钟）。
func Backoff(attempt int) time.Duration {
	d := time.Minute
	for i := 1; i < attempt && d < 30*time.Minute; i++ {
		d *= 2
	}
	return min(d, 30*time.Minute)
}

// 展示级别（数据架构第12节），从宽到严。
var displayOrder = map[string]int{"full_text": 0, "excerpt": 1, "citation_only": 2, "none": 3}

// StrictestLevel 返回多项来源级别中最严格的一个；没有或含未识别的级别时为 none（失败关闭）。
func StrictestLevel(levels []string) string {
	if len(levels) == 0 {
		return "none"
	}
	out := "full_text"
	for _, l := range levels {
		r, ok := displayOrder[l]
		if !ok {
			return "none"
		}
		if r > displayOrder[out] {
			out = l
		}
	}
	return out
}

// ExcerptMax 是 excerpt 级别的字段在简报中保留的最多字符数。
const ExcerptMax = 40

// Truncate 在字符边界截断文本并以“…”结尾（F3 第2条）；max 为字符数上限（含“…”）。
func Truncate(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	if max == 1 {
		return "…"
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

// CardLine 是信号卡片的一行：Text 为内容；Keep 为永不省略的行（处置、金额与金额类型、“未公布”“未知”、链接说明）；Atomic 为不可截断的行
// （数字、金额、日期与单位不截断：放不下时整行省略，Keep 的行则保留原文）；Priority 越大越先省略。
type CardLine struct {
	Text     string
	Keep     bool
	Atomic   bool
	Priority int
}

// FitCard 按渠道能力声明把卡片正文放进 linesMax 行、每行 lineMax 字符（F3 第2条）：文本行在字符边界截断并加“…”；不可截断的行
// 放不下时整行省略（Keep 的除外）；行数超出时按优先级从低到高整项省略（Priority 大者先省略），Keep 的行永不省略。
// 返回保留的行与被省略的行数。
func FitCard(lines []CardLine, lineMax, linesMax int) ([]string, int) {
	type entry struct {
		CardLine
		index int
	}
	var kept []entry
	dropped := 0
	for i, l := range lines {
		if lineMax > 0 && utf8.RuneCountInString(l.Text) > lineMax {
			switch {
			case l.Atomic && !l.Keep:
				dropped++
				continue
			case !l.Atomic:
				l.Text = Truncate(l.Text, lineMax)
			}
		}
		kept = append(kept, entry{l, i})
	}
	for linesMax > 0 && len(kept) > linesMax {
		victim := -1
		for i, e := range kept {
			if e.Keep {
				continue
			}
			if victim < 0 || e.Priority > kept[victim].Priority || (e.Priority == kept[victim].Priority && e.index > kept[victim].index) {
				victim = i
			}
		}
		if victim < 0 {
			break
		}
		kept = slices.Delete(kept, victim, victim+1)
		dropped++
	}
	out := make([]string, len(kept))
	for i, e := range kept {
		out[i] = e.Text
	}
	return out, dropped
}

// FormatAmountCNY 把人民币元的十进制金额文本格式化为“1,800 万”或“86.5 万”等简报写法；不能解析时原样返回。
func FormatAmountCNY(v string) string {
	var yuan float64
	if _, err := fmt.Sscanf(v, "%g", &yuan); err != nil {
		return v
	}
	if yuan >= 10000 {
		w := yuan / 10000
		s := fmt.Sprintf("%.2f", w)
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		return groupThousands(s) + " 万"
	}
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", yuan), "0"), ".")
	return groupThousands(s) + " 元"
}

func groupThousands(s string) string {
	intPart, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if frac != "" {
		b.WriteString("." + frac)
	}
	return b.String()
}
