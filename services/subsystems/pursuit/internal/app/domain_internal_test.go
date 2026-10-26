package app

import (
	"strings"
	"testing"
	"time"
)

// PRD 第6段用例 28：只填日期的截止保存日期与时区，不补成某个时刻；Instant 为所填时区的次日 00:00；无效时区与格式 422 invalid_time。
func TestLocalTimeInstant(t *testing.T) {
	day := &LocalTime{Value: "2026-10-25", Precision: "day", Timezone: "Asia/Shanghai"}
	at, err := day.Instant()
	if err != nil || !at.Equal(time.Date(2026, 10, 25, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %v", at, err)
	}
	minute := &LocalTime{Value: "2026-10-25T09:30", Precision: "minute", Timezone: "America/New_York"}
	if at, err := minute.Instant(); err != nil || !at.Equal(time.Date(2026, 10, 25, 13, 30, 0, 0, time.UTC)) {
		t.Fatalf("%v %v", at, err)
	}
	for _, bad := range []*LocalTime{nil, {Value: "2026-10-25", Precision: "minute", Timezone: "Asia/Shanghai"},
		{Value: "2026-10-25T09:30", Precision: "day", Timezone: "Asia/Shanghai"}, {Value: "2026-10-25", Precision: "day", Timezone: "Mars/Base"},
		{Value: "2026-10-25", Precision: "day", Timezone: "Local"}, {Value: "2026-10-25", Precision: "hour", Timezone: "UTC"}} {
		if _, err := bad.Instant(); err == nil || !strings.Contains(err.Error(), "invalid_time") {
			t.Fatalf("%+v：%v", bad, err)
		}
	}
}

// PRD 第6段用例 29：分钟精度提前 24 小时与到期各一次；只填日期在前一日与当日 09:00（所填时区）；逾期次日 09:00 一次；计划时已过的不计划。
func TestReminderPlan(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	kinds := func(rs []plannedReminder) (out []string) {
		for _, r := range rs {
			out = append(out, r.kind+"@"+r.at.Format(time.RFC3339))
		}
		return
	}
	got := kinds(reminderPlan(&LocalTime{Value: "2026-10-25", Precision: "day", Timezone: "Asia/Shanghai"}, now))
	want := []string{"day_before_0900@2026-10-24T01:00:00Z", "due_day_0900@2026-10-25T01:00:00Z", "overdue_next_day_0900@2026-10-26T01:00:00Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%v", got)
	}
	got = kinds(reminderPlan(&LocalTime{Value: "2026-10-25T09:30", Precision: "minute", Timezone: "Asia/Shanghai"}, now))
	want = []string{"before_24h@2026-10-24T01:30:00Z", "at_due@2026-10-25T01:30:00Z", "overdue_next_day_0900@2026-10-26T01:00:00Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%v", got)
	}
	// 截止在 24 小时内：提前提醒已过，不计划
	got = kinds(reminderPlan(&LocalTime{Value: "2026-10-20T12:00", Precision: "minute", Timezone: "UTC"}, now))
	if len(got) != 2 || !strings.HasPrefix(got[0], "at_due@") {
		t.Fatalf("%v", got)
	}
	if reminderPlan(nil, now) != nil || reminderPlan(&LocalTime{Value: "x", Precision: "day", Timezone: "UTC"}, now) != nil {
		t.Fatal("无截止或无效截止不计划提醒")
	}
}

// PRD 第6段用例 41：信号生成早于获知时间标“首次提示”；获知时间只到日且与信号同日（按所填时区）不判定；用户给出的值优先；
// 手动跟进与缺少获知时间不判定。
func TestContribution(t *testing.T) {
	created := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) // 北京时间 10-19 20:00
	p := &Pursuit{SourceKind: "signal", SignalCreatedAt: &created}
	cases := []struct {
		in   OutcomeInput
		want string
	}{
		{OutcomeInput{KnownAt: &LocalTime{Value: "2026-11-03", Precision: "day", Timezone: "Asia/Shanghai"}}, "first_alert"},
		{OutcomeInput{KnownAt: &LocalTime{Value: "2026-10-19", Precision: "day", Timezone: "Asia/Shanghai"}}, "undetermined"},
		{OutcomeInput{KnownAt: &LocalTime{Value: "2026-10-19T21:00", Precision: "minute", Timezone: "Asia/Shanghai"}}, "first_alert"},
		{OutcomeInput{KnownAt: &LocalTime{Value: "2026-10-19T19:00", Precision: "minute", Timezone: "Asia/Shanghai"}}, "undetermined"},
		{OutcomeInput{}, "undetermined"},
		{OutcomeInput{KnownAt: &LocalTime{Value: "2026-11-03", Precision: "day", Timezone: "Asia/Shanghai"}, Contribution: "self_found"}, "self_found"},
	}
	for _, c := range cases {
		if got := contribution(p, &c.in); got != c.want {
			t.Fatalf("%+v：%s", c.in.KnownAt, got)
		}
	}
	manual := &Pursuit{SourceKind: "manual"}
	if got := contribution(manual, &cases[0].in); got != "undetermined" {
		t.Fatal(got)
	}
}

// 越权与反例：客户端持有的翻页位置经 HMAC 签名，篡改、换用途、截断与过长在任何读取之前拒绝；密钥过短时不签发（失败关闭）。
func TestCursorSealRejectsTamper(t *testing.T) {
	s := &Service{Key: []byte(strings.Repeat("k", MinSigningKey))}
	v, err := s.seal(purposeListCursor, listCursor{Tenant: "tnt_a", Subject: "usr_a", View: "all", ID: "pur_1"})
	if err != nil {
		t.Fatal(err)
	}
	var got listCursor
	if !s.unseal(purposeListCursor, v, &got) || got.Tenant != "tnt_a" || got.ID != "pur_1" {
		t.Fatalf("%+v", got)
	}
	tampered := []byte(v)
	tampered[3] ^= 1
	for _, bad := range []string{string(tampered), v[:len(v)-2], strings.Repeat("A", 2000), ""} {
		if s.unseal(purposeListCursor, bad, &got) {
			t.Fatalf("篡改的游标被接受：%q", bad)
		}
	}
	if s.unseal(purposeTasksCursor, v, &got) {
		t.Fatal("跨用途的游标被接受")
	}
	if _, err := (&Service{Key: []byte("short")}).seal(purposeListCursor, got); err == nil {
		t.Fatal("密钥过短时仍签发")
	}
}

func TestGrantTracksCurrentRoleVersion(t *testing.T) {
	// A 在第1版成为负责人；改派给 B（第2版）再改回 A（第3版）：A 的授予版本为 3，identity 只应用到第1版时不放行
	p := &Pursuit{Owner: "A", OwnerValid: true, Collaborators: []string{}, Participation: 1, Applied: 1}
	p.Grants = regrant(nil, func(string) string { return "" }, "A", nil, 1, map[string]int64{"A": 7})
	if !p.granted("A") {
		t.Fatal("第1版已应用")
	}
	p.Grants, p.Owner, p.Participation = regrant(p.Grants, p.role, "B", nil, 2, map[string]int64{"B": 3}), "B", 2
	p.Grants, p.Owner, p.Participation = regrant(p.Grants, p.role, "A", nil, 3, map[string]int64{"A": 9}), "A", 3
	if g := p.Grants["A"]; g.PV != 3 || g.Member != 9 || p.granted("A") {
		t.Fatalf("A→B→A：%+v", g)
	}
	p.Applied = 3
	if !p.granted("A") {
		t.Fatal("第3版已应用")
	}
	// 角色不变保留原授予；负责人改为协作者是新的授予
	// 角色改变时成员版本取此次重新判断的值，不沿用授予前的值
	next := regrant(p.Grants, p.role, "C", []string{"A"}, 4, map[string]int64{"C": 1, "A": 11})
	if g := next["A"]; g.Role != "collaborator" || g.PV != 4 || g.Member != 11 {
		t.Fatalf("%+v", g)
	}
	p.Collaborators = []string{"C"}
	if kept := regrant(p.Grants, p.role, "A", []string{"C"}, 5, nil); kept["A"].PV != 3 || kept["A"].Member != 9 || kept["C"].PV != 5 || kept["C"].Member != 0 {
		t.Fatalf("%+v", kept)
	}
}
