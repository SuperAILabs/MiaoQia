package scheduler_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/scheduler"
)

func ok(context.Context) (int, error) { return 0, nil }

func TestValidateRejectsBadDeclarations(t *testing.T) {
	good := scheduler.Job{Name: "identity.expire-approvals", Every: time.Minute, Run: ok}
	if err := scheduler.Validate([]scheduler.Bound{{Job: good}}); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]scheduler.Bound{
		"无子系统前缀": {{Job: scheduler.Job{Name: "expire", Every: time.Minute, Run: ok}}},
		"大写":     {{Job: scheduler.Job{Name: "Identity.expire", Every: time.Minute, Run: ok}}},
		"空名":     {{Job: scheduler.Job{Every: time.Minute, Run: ok}}},
		"重复":     {{Job: good}, {Job: good}},
		"周期不足1秒": {{Job: scheduler.Job{Name: "a.b", Every: 500 * time.Millisecond, Run: ok}}},
		"上限超过周期": {{Job: scheduler.Job{Name: "a.b", Every: time.Second, Timeout: 2 * time.Second, Run: ok}}},
		"负上限":    {{Job: scheduler.Job{Name: "a.b", Every: time.Second, Timeout: -1, Run: ok}}},
		"没有 Run": {{Job: scheduler.Job{Name: "a.b", Every: time.Second}}},
	}
	for name, jobs := range cases {
		if err := scheduler.Validate(jobs); err == nil {
			t.Errorf("%s：应拒绝", name)
		}
	}
	r := &scheduler.Runner{Jobs: cases["重复"]}
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("声明无效时 Run 应立即返回错误")
	}
}

func TestOnceWithoutDatabase(t *testing.T) {
	var rounds []scheduler.Outcome
	r := &scheduler.Runner{OnRound: func(_ string, o scheduler.Outcome, _ int, _ error) { rounds = append(rounds, o) }}
	if o := r.Once(context.Background(), scheduler.Bound{Job: scheduler.Job{Name: "a.ok", Every: time.Second,
		Run: func(context.Context) (int, error) { return 3, nil }}}); o != scheduler.Ran {
		t.Fatalf("成功轮次应为 ran：%s", o)
	}
	boom := errors.New("boom")
	var got error
	r.OnRound = func(_ string, o scheduler.Outcome, _ int, err error) { rounds = append(rounds, o); got = err }
	if o := r.Once(context.Background(), scheduler.Bound{Job: scheduler.Job{Name: "a.fail", Every: time.Second,
		Run: func(context.Context) (int, error) { return 0, boom }}}); o != scheduler.Failed || !errors.Is(got, boom) {
		t.Fatalf("失败轮次应为 failed 并带原错误：%s %v", o, got)
	}
	if len(rounds) != 2 {
		t.Fatalf("每轮应回调一次：%v", rounds)
	}
}

func TestOnceTimeoutCancelsRun(t *testing.T) {
	r := &scheduler.Runner{}
	start := time.Now()
	o := r.Once(context.Background(), scheduler.Bound{Job: scheduler.Job{Name: "a.slow", Every: time.Second, Timeout: time.Second,
		Run: func(ctx context.Context) (int, error) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(10 * time.Second):
				return 1, nil
			}
		}}})
	if o != scheduler.Failed || time.Since(start) > 5*time.Second {
		t.Fatalf("超过单轮上限应取消并记为失败：%s %s", o, time.Since(start))
	}
}

func TestRunStartsImmediatelyAndNeverOverlaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var running, overlapped, calls atomic.Int32
	first := make(chan time.Time, 1)
	r := &scheduler.Runner{Jobs: []scheduler.Bound{{Job: scheduler.Job{Name: "a.tick", Every: time.Second,
		Run: func(context.Context) (int, error) {
			if running.Add(1) > 1 {
				overlapped.Add(1)
			}
			defer running.Add(-1)
			if calls.Add(1) == 1 {
				first <- time.Now()
			}
			// 每轮耗时超过一个周期：期间到达的节拍不得叠加出并发轮次
			time.Sleep(1500 * time.Millisecond)
			return 0, nil
		}}}}}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case at := <-first:
		if at.Sub(start) > 500*time.Millisecond {
			t.Fatalf("启动后应立即执行一次：%s", at.Sub(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有执行")
	}
	time.Sleep(3500 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后 Run 应返回 context.Canceled：%v", err)
	}
	if overlapped.Load() != 0 {
		t.Fatal("同一任务的轮次不得重叠")
	}
	if n := calls.Load(); n < 2 || n > 4 {
		t.Fatalf("约 5 秒内应执行 2–4 轮：%d", n)
	}
}

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d := testenv.NewDatabase(t)
	h, err := db.Open(context.Background(), d.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// 主备两实例（各自的连接池）同时执行同一任务：持锁期间另一实例跳过，释放后可再取；不同任务互不阻塞。
func TestOnceAdvisoryLockAcrossInstances(t *testing.T) {
	primary := openDB(t)
	standby, err := db.Open(context.Background(), primary.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer standby.Close()

	entered, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	job := scheduler.Job{Name: "identity.expire-approvals", Every: time.Minute, Run: func(context.Context) (int, error) {
		if runs.Add(1) == 1 {
			close(entered)
			<-release
		}
		return 1, nil
	}}
	r := &scheduler.Runner{}
	ctx := context.Background()
	var wg sync.WaitGroup
	var first scheduler.Outcome
	wg.Add(1)
	go func() { defer wg.Done(); first = r.Once(ctx, scheduler.Bound{Job: job, DB: primary}) }()
	<-entered

	if o := r.Once(ctx, scheduler.Bound{Job: job, DB: standby}); o != scheduler.Skipped {
		t.Fatalf("另一实例持锁时应跳过：%s", o)
	}
	if o := r.Once(ctx, scheduler.Bound{Job: job, DB: primary}); o != scheduler.Skipped {
		t.Fatalf("同一实例的另一连接也应跳过（会话级锁）：%s", o)
	}
	other := scheduler.Job{Name: "identity.expire-invitations", Every: time.Minute, Run: ok}
	if o := r.Once(ctx, scheduler.Bound{Job: other, DB: standby}); o != scheduler.Ran {
		t.Fatalf("不同任务不应互相阻塞：%s", o)
	}
	close(release)
	wg.Wait()
	if first != scheduler.Ran {
		t.Fatalf("持锁实例应执行：%s", first)
	}
	if o := r.Once(ctx, scheduler.Bound{Job: job, DB: standby}); o != scheduler.Ran {
		t.Fatalf("释放后另一实例应能取锁执行：%s", o)
	}
	if runs.Load() != 2 {
		t.Fatalf("应恰好执行两轮：%d", runs.Load())
	}
}

// 任务失败或超时后锁也须释放，否则主备都会永久跳过。
func TestOnceReleasesLockAfterFailureAndTimeout(t *testing.T) {
	h := openDB(t)
	r := &scheduler.Runner{}
	ctx := context.Background()
	failing := scheduler.Job{Name: "a.fail", Every: time.Second, Run: func(context.Context) (int, error) { return 0, errors.New("boom") }}
	if o := r.Once(ctx, scheduler.Bound{Job: failing, DB: h}); o != scheduler.Failed {
		t.Fatalf("应失败：%s", o)
	}
	slow := scheduler.Job{Name: "a.fail", Every: time.Second, Run: func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	if o := r.Once(ctx, scheduler.Bound{Job: slow, DB: h}); o != scheduler.Failed {
		t.Fatalf("超时应失败：%s", o)
	}
	succeed := scheduler.Job{Name: "a.fail", Every: time.Second, Run: ok}
	if o := r.Once(ctx, scheduler.Bound{Job: succeed, DB: h}); o != scheduler.Ran {
		t.Fatalf("失败与超时后锁应已释放：%s", o)
	}
	var held int
	if err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("不应残留咨询锁：%d", held)
	}
}
