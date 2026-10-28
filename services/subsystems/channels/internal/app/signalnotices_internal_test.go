package app

import (
	"slices"
	"testing"
)

// C-08：即时通知的投递事件字段与统计原因按白名单；简报条目只标收件人本人的完整条目。
func TestSignalNoticeWhitelists(t *testing.T) {
	d := signalNoticeEventData("dlv_1", "u1", "sig_1", "obj_1", 1)
	var keys []string
	for k := range d {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"attempt", "channel", "kind", "notice_id", "objective_id", "recipient", "signal_id"}) {
		t.Fatalf("%v", keys)
	}
	for _, c := range []struct{ kind, state, reason, want string }{
		{NoticeSignalImmediate, "cancelled", "merged_into_briefing", "merged_into_briefing"},
		{NoticeSignalImmediate, "cancelled", "throttled", "throttled"},
		{NoticeSignalImmediate, "cancelled", "task_closed", "other"}, // 任务提醒的原因不属于即时通知
		{NoticeSignalImmediate, "in_app", "throttled", "other"},
		{NoticeSignalImmediate, "delivered", "anything", ""},
		{"task_due", "cancelled", "throttled", "other"}, // 即时通知的原因不属于任务提醒
		{"task_due", "cancelled", "reassigned", "reassigned"},
	} {
		if got := noticeKindStatsReason(c.kind, c.state, c.reason); got != c.want {
			t.Fatalf("%+v：%q", c, got)
		}
	}
	secs := []composedSection{{section: section{Kind: "objective", Items: []item{
		{Kind: "signal", SignalItem: &SignalItem{SignalID: "sig_a"}},
		{Kind: "signal", SignalItem: &SignalItem{SignalID: "sig_b"}},
		{Kind: "reference", SignalItem: &SignalItem{SignalID: "sig_c"}},
	}}}}
	if got := listedSignals(secs); !slices.Equal(got, []string{"sig_a", "sig_b"}) {
		t.Fatalf("%v", got)
	}
	applyNoticeMarks(secs, &noticeMarks{notified: map[string]bool{"sig_a": true, "sig_c": true}, overnight: map[string]bool{"sig_a": true, "sig_b": true, "sig_c": true}})
	a, b, c := secs[0].Items[0].SignalItem, secs[0].Items[1].SignalItem, secs[0].Items[2].SignalItem
	if !a.Notified || a.ArrivedOvernight || b.Notified || !b.ArrivedOvernight || c.Notified || c.ArrivedOvernight {
		t.Fatalf("%+v %+v %+v", a, b, c)
	}
}
