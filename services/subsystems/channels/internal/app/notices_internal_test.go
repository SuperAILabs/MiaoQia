package app

import (
	"slices"
	"testing"
)

// C-11：投递事件与统计的字段按通知种类、原因按状态白名单。
func TestNoticeWhitelists(t *testing.T) {
	keys := func(m map[string]any) []string {
		var k []string
		for key := range m {
			k = append(k, key)
		}
		slices.Sort(k)
		return k
	}
	due := noticeEventData("dlv_1", "task_due", "u1", "pur_1", "tsk_1", 3, true, 2)
	if got := keys(due); !slices.Equal(got, []string{"attempt", "channel", "deadline_version", "escalated", "kind", "notice_id", "pursuit_id", "recipient", "task_id"}) {
		t.Fatalf("task_due：%v", got)
	}
	rej := noticeEventData("dlv_1", "authorization_rejected", "u1", "pur_1", "tsk_should_not_leak", 3, true, 1)
	if got := keys(rej); !slices.Equal(got, []string{"attempt", "channel", "kind", "notice_id", "pursuit_id", "recipient"}) {
		t.Fatalf("authorization_rejected：%v", got)
	}
	for _, c := range []struct{ state, reason, want string }{
		{"pending", "anything", ""}, {"delivered", "channel_error", ""}, {"retrying", "channel_error", "channel_error"}, {"retrying", "cutoff", "other"},
		{"in_app", "not_bound", "not_bound"}, {"in_app", "reassigned", "other"}, {"cancelled", "pursuit_not_visible", "pursuit_not_visible"},
		{"cancelled", "负责人张三已离职", "other"}, {"cancelled", "", "other"},
	} {
		if got := noticeStatsReason(c.state, c.reason); got != c.want {
			t.Fatalf("%s/%s：%q", c.state, c.reason, got)
		}
	}
}

// C-17：授权同步失败通知的再检查顺序与字段白名单（只带跟进，不带任务字段）。
func TestSyncFailedNoticeRecheck(t *testing.T) {
	cases := []struct {
		target rejectedTarget
		want   string
	}{
		{rejectedTarget{open: true, failed: true, resolver: true}, ""},
		{rejectedTarget{open: false, failed: true, resolver: true}, "pursuit_closed"},
		{rejectedTarget{open: true, failed: false, resolver: true}, "sync_recovered"},
		{rejectedTarget{open: true, failed: true, resolver: false}, "not_decision_maker"},
	}
	for _, c := range cases {
		if got := c.target.syncFailedReason(); got != c.want {
			t.Fatalf("%+v：%q，期望 %q", c.target, got, c.want)
		}
	}
	d := noticeEventData("dlv_1", NoticeSyncFailed, "u", "pur_1", "tsk_1", 3, true, 1)
	for _, k := range []string{"task_id", "deadline_version", "escalated"} {
		if _, ok := d[k]; ok {
			t.Fatalf("授权同步失败通知不应带 %s：%+v", k, d)
		}
	}
	for _, r := range []string{"sync_recovered", "not_decision_maker", "pursuit_closed"} {
		if !noticeReasons["cancelled"][r] {
			t.Fatalf("%s 应在白名单内", r)
		}
	}
	if handedBack([]byte(`{"handed_back":true,"escalated":true}`)) || !handedBack([]byte(`{"handed_back":true,"escalated":false}`)) {
		t.Fatal("转回只在未转交时成立")
	}
}
