package channels

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
)

type scriptedBriefings struct {
	api.Briefings
	results []*api.ComposeDueResult
	err     error
	calls   int
}

func (s *scriptedBriefings) ComposeDue(context.Context, time.Time, int) (*api.ComposeDueResult, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.calls > len(s.results) {
		panic("多余的 ComposeDue 调用")
	}
	return s.results[s.calls-1], nil
}

// 简报组装任务：More 为真时继续；一批没有任何进展时停止（失败的成员留到下一轮）；有失败时本轮记为失败。
func TestComposeDueLoop(t *testing.T) {
	at := time.Date(2026, 10, 27, 0, 30, 0, 0, time.UTC)
	b := &scriptedBriefings{results: []*api.ComposeDueResult{
		{Composed: 50, More: true}, {Composed: 20, Existing: 3, Late: 1, Skipped: 2, More: false},
	}}
	if n, err := composeDue(context.Background(), b, at); n != 70 || err != nil || b.calls != 2 {
		t.Fatalf("应组装到 More 为假：%d %v %d", n, err, b.calls)
	}
	b = &scriptedBriefings{results: []*api.ComposeDueResult{
		{Composed: 10, Failed: 2, More: true}, {Failed: 2, More: true},
	}}
	n, err := composeDue(context.Background(), b, at)
	if n != 10 || err == nil || !strings.Contains(err.Error(), "4 名") || b.calls != 2 {
		t.Fatalf("没有进展时应停止并报告失败：%d %v %d", n, err, b.calls)
	}
	boom := errors.New("boom")
	b = &scriptedBriefings{err: boom}
	if _, err := composeDue(context.Background(), b, at); !errors.Is(err, boom) {
		t.Fatalf("应返回调用错误：%v", err)
	}
}
