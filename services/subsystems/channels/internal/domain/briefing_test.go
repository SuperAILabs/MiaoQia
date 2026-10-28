package domain

import (
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// PRD 第5段用例3：周期按成员时区的固定区间：周二为 [周一 07:30, 周二 07:30)，周一从上周五 07:30 起。
func TestPeriodIsFixedInterval(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	s := Schedule{Defaults()}
	// 2026-10-20 是周二
	start, end, _, ok := s.PeriodAt(time.Date(2026, 10, 20, 7, 31, 0, 0, sh))
	if !ok || !start.Equal(time.Date(2026, 10, 19, 7, 30, 0, 0, sh)) || !end.Equal(time.Date(2026, 10, 20, 7, 30, 0, 0, sh)) {
		t.Fatalf("周二：%v – %v", start, end)
	}
	if got := end.In(sh).Format(time.RFC3339); got != "2026-10-20T07:30:00+08:00" {
		t.Fatal(got)
	}
	start, end, _, _ = s.PeriodAt(time.Date(2026, 10, 19, 8, 0, 0, 0, sh))
	if !start.Equal(time.Date(2026, 10, 16, 7, 30, 0, 0, sh)) || !end.Equal(time.Date(2026, 10, 19, 7, 30, 0, 0, sh)) {
		t.Fatalf("周一：%v – %v", start, end)
	}
	// 计划发送时刻本身属于以它为终点的周期
	_, end, _, _ = s.PeriodAt(time.Date(2026, 10, 20, 7, 30, 0, 0, sh))
	if !end.Equal(time.Date(2026, 10, 20, 7, 30, 0, 0, sh)) {
		t.Fatal(end)
	}
}

// PRD 第5段用例3、F9：修改发送时间从下一期生效，新旧周期首尾相接、不重叠不留空。
func TestPreferenceChangeIsContiguous(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	old := Schedule{Defaults()}
	changedAt := time.Date(2026, 10, 20, 12, 0, 0, 0, sh) // 周二中午改为 09:00
	eff := old.NextBoundary(changedAt)
	if !eff.Equal(time.Date(2026, 10, 21, 7, 30, 0, 0, sh)) {
		t.Fatalf("生效起点 %v", eff)
	}
	v1 := Defaults()
	v1.Version, v1.SendTime, v1.EffectiveFrom = 1, "09:00", &eff
	s := Schedule{Defaults(), v1}
	// 周三 07:30 的一期仍按旧偏好：[周二 07:30, 周三 07:30)
	start, end, p, _ := s.PeriodAt(time.Date(2026, 10, 21, 8, 0, 0, 0, sh))
	if !start.Equal(time.Date(2026, 10, 20, 7, 30, 0, 0, sh)) || !end.Equal(eff) || p.Version != 0 {
		t.Fatalf("旧周期 %v – %v（版本 %d）", start, end, p.Version)
	}
	// 周三 07:30–09:00 之间没有新的周期终点
	if _, e, _, _ := s.PeriodAt(time.Date(2026, 10, 21, 8, 59, 0, 0, sh)); !e.Equal(eff) {
		t.Fatal(e)
	}
	// 下一期从旧周期终点起：[周三 07:30, 周四 09:00)
	start, end, p, _ = s.PeriodAt(time.Date(2026, 10, 22, 9, 0, 0, 0, sh))
	if !start.Equal(eff) || !end.Equal(time.Date(2026, 10, 22, 9, 0, 0, 0, sh)) || p.Version != 1 {
		t.Fatalf("新周期 %v – %v（版本 %d）", start, end, p.Version)
	}
	// 再下一期首尾相接
	s2, _, _, _ := s.PeriodAt(time.Date(2026, 10, 23, 9, 0, 0, 0, sh))
	if !s2.Equal(end) {
		t.Fatal(s2)
	}
	// 生效前再次修改：新的版本在同一边界生效，后者为准
	v2 := Defaults()
	v2.Version, v2.SendTime = 2, "08:00"
	eff2 := s.NextBoundary(time.Date(2026, 10, 20, 18, 0, 0, 0, sh))
	v2.EffectiveFrom = &eff2
	s3 := Schedule{Defaults(), v1, v2}
	if !eff2.Equal(eff) {
		t.Fatal(eff2)
	}
	start, end, p, _ = s3.PeriodAt(time.Date(2026, 10, 22, 9, 0, 0, 0, sh))
	if !start.Equal(eff) || !end.Equal(time.Date(2026, 10, 22, 8, 0, 0, 0, sh)) || p.Version != 2 {
		t.Fatalf("%v – %v（版本 %d）", start, end, p.Version)
	}
}

// PRD 第5段用例4：成员时区 Asia/Tokyo 时按东京 07:30 发送，周期以带偏移的时刻保存。
func TestMemberTimezone(t *testing.T) {
	tk := mustLoc(t, "Asia/Tokyo")
	p := Defaults()
	p.Timezone = "Asia/Tokyo"
	_, end, _, _ := Schedule{p}.PeriodAt(time.Date(2026, 10, 20, 7, 45, 0, 0, tk))
	if end.In(tk).Format(time.RFC3339) != "2026-10-20T07:30:00+09:00" || end.In(mustLoc(t, "Asia/Shanghai")).Hour() != 6 {
		t.Fatal(end)
	}
}

// PRD 第5段第9节、第13节第11、12项：12:00 后组装不推送；18:00 停止渠道重试；安静时段跨午夜。
func TestLateCutoffQuiet(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	end := time.Date(2026, 10, 20, 7, 30, 0, 0, sh)
	if ComposeLate(end, time.Date(2026, 10, 20, 11, 59, 0, 0, sh), sh) || !ComposeLate(end, time.Date(2026, 10, 20, 12, 0, 0, 0, sh), sh) {
		t.Fatal("12:00 截止")
	}
	if !DeliveryCutoff(end, sh).Equal(time.Date(2026, 10, 20, 18, 0, 0, 0, sh)) {
		t.Fatal("18:00 截止")
	}
	p := Defaults()
	if u := QuietUntil(p, time.Date(2026, 10, 20, 22, 0, 0, 0, sh)); !u.Equal(time.Date(2026, 10, 21, 7, 0, 0, 0, sh)) {
		t.Fatal(u)
	}
	if u := QuietUntil(p, time.Date(2026, 10, 20, 6, 0, 0, 0, sh)); !u.Equal(time.Date(2026, 10, 20, 7, 0, 0, 0, sh)) {
		t.Fatal(u)
	}
	if u := QuietUntil(p, time.Date(2026, 10, 20, 7, 30, 0, 0, sh)); !u.IsZero() {
		t.Fatal(u)
	}
	if Backoff(1) != time.Minute || Backoff(3) != 4*time.Minute || Backoff(20) != 30*time.Minute {
		t.Fatal("退避")
	}
}

func TestValidatePreferences(t *testing.T) {
	limits := map[string][2]int{"APP-TENDER.A02": {5, 20}}
	ok := Defaults()
	ok.SectionLimits = map[string]int{"APP-TENDER.A02": 12}
	if err := ValidatePreferences(ok, limits); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Preferences){
		"发送时间不是整点或半点":  func(p *Preferences) { p.SendTime = "07:15" },
		"发送时间早于 06:00": func(p *Preferences) { p.SendTime = "05:30" },
		"发送时间晚于 10:00": func(p *Preferences) { p.SendTime = "10:30" },
		"时区不可解析":       func(p *Preferences) { p.Timezone = "Mars/Base" },
		"发送日为空":        func(p *Preferences) { p.SendDays = nil },
		"发送日重复":        func(p *Preferences) { p.SendDays = []int{1, 1} },
		"条数超出范围":       func(p *Preferences) { p.SectionLimits = map[string]int{"APP-TENDER.A02": 21} },
		"未声明的目标":       func(p *Preferences) { p.SectionLimits = map[string]int{"APP-TENDER.ZZ": 10} },
	} {
		p := Defaults()
		mut(&p)
		if ValidatePreferences(p, limits) == nil {
			t.Fatal(name)
		}
	}
}

// PRD 第5段用例13：项目名称超长时在字符边界截断并加“…”；金额、处置与“查看”不被省略；放不下时按优先级整项省略。
func TestFitCard(t *testing.T) {
	long := strings.Repeat("某某市政管网改造工程", 6)
	lines := []CardLine{
		{Text: "A02 进入供货 · 跟进候选", Keep: true},
		{Text: long, Priority: 1},
		{Text: "中标金额 1,800 万", Keep: true, Atomic: true},
		{Text: "地区 330106 · 品类 CAT-PIPE-PE", Priority: 5},
		{Text: "理由：中标方近三年采购过该品类", Priority: 3},
		{Text: "理由：金额高于门槛", Priority: 4},
		{Text: "标记：已更新", Priority: 6},
	}
	out, dropped := FitCard(lines, 20, 4)
	if len(out) != 4 || dropped != 3 {
		t.Fatalf("%d %v", dropped, out)
	}
	if out[0] != "A02 进入供货 · 跟进候选" || out[2] != "中标金额 1,800 万" || !strings.HasSuffix(out[1], "…") || len([]rune(out[1])) != 20 {
		t.Fatalf("%q", out)
	}
	// 先省略标记，再省略第二条理由，再省略地区与品类
	if out[3] != "理由：中标方近三年采购过该品类" {
		t.Fatalf("%q", out)
	}
	// 不可截断的金额行永不截断
	out, _ = FitCard([]CardLine{{Text: "中标金额 12,345,678.90 元（同标包共 3 家中标）", Keep: true, Atomic: true}}, 10, 3)
	if out[0] != "中标金额 12,345,678.90 元（同标包共 3 家中标）" {
		t.Fatal(out)
	}
	if Truncate("一二三四五", 3) != "一二…" || Truncate("短", 3) != "短" {
		t.Fatal("截断")
	}
}

// PRD 第5段用例14：展示级别逐维取最严格；未识别或没有级别时失败关闭为 none。
func TestStrictestLevel(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"full_text"}, "full_text"},
		{[]string{"full_text", "citation_only", "excerpt"}, "citation_only"},
		{[]string{"excerpt", "full_text"}, "excerpt"},
		{nil, "none"},
		{[]string{"full_text", "bogus"}, "none"},
		{[]string{"none", "full_text"}, "none"},
	} {
		if got := StrictestLevel(c.in); got != c.want {
			t.Fatalf("%v：%s", c.in, got)
		}
	}
	if FormatAmountCNY("18000000") != "1,800 万" || FormatAmountCNY("865000") != "86.5 万" || FormatAmountCNY("1234.50") != "1,234.5 元" {
		t.Fatal(FormatAmountCNY("18000000"), FormatAmountCNY("865000"), FormatAmountCNY("1234.50"))
	}
}

// C-08（PRD 第5段 F2 第2条、F7）：安静时段内到达的即时通知，结束时简报即将发送（MergeWindow 内）的并入简报；周末没有简报时照常推送；
// 白天到达的、仍在安静时段内的都不并入；缺省接收即时通知。
func TestMergeIntoBriefing(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	p := Defaults()
	if !p.InstantNotify {
		t.Fatal("缺省接收即时通知")
	}
	sched := Schedule{p}
	night := time.Date(2026, 10, 20, 23, 0, 0, 0, sh) // 周二夜间
	morning := time.Date(2026, 10, 21, 7, 0, 0, 0, sh)
	if next := sched.NextBoundary(morning); !MergeIntoBriefing(p, night, morning, next) {
		t.Fatalf("周三 07:00 结束、07:30 简报：应并入（%v）", next)
	}
	if MergeIntoBriefing(p, night, time.Date(2026, 10, 21, 6, 59, 0, 0, sh), sched.NextBoundary(morning)) {
		t.Fatal("仍在安静时段内")
	}
	if MergeIntoBriefing(p, time.Date(2026, 10, 21, 10, 0, 0, 0, sh), time.Date(2026, 10, 21, 10, 5, 0, 0, sh), sched.NextBoundary(morning)) {
		t.Fatal("白天到达的不并入")
	}
	// 周五夜间到达、周六 07:00 结束：下一期在周一，照常推送
	sat := time.Date(2026, 10, 24, 7, 0, 0, 0, sh)
	if MergeIntoBriefing(p, time.Date(2026, 10, 23, 23, 0, 0, 0, sh), sat, sched.NextBoundary(sat)) {
		t.Fatal("周末没有即将发送的简报")
	}
	// 结束得晚（发送时间已过）：下一期在明天，照常推送
	late := time.Date(2026, 10, 21, 9, 0, 0, 0, sh)
	if MergeIntoBriefing(p, night, late, sched.NextBoundary(late)) || MergeIntoBriefing(p, night, morning, time.Time{}) {
		t.Fatal("没有即将发送的简报")
	}
}
