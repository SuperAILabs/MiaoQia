// Package scheduler 是 scheduler 进程角色的周期任务运行器（docs/03-architecture/07-deployment-topology.md 第 2.3 节；验收线 R-01）。
//
// 子系统在 runtime.Module.Jobs 中声明自己的周期任务，任务调用本子系统已有的、可重复执行的维护入口（例如 identity.Maintenance）。
// 运行器为每个任务起一个循环：启动后立即执行一次，之后按 Every 周期执行；上一轮未结束时跳过本轮，不叠加；单轮受 Timeout 约束。
// 主备部署时同一任务同一时刻只在一个实例上执行：执行前在任务所属子系统的数据库上取会话级咨询锁（pg_try_advisory_lock），取不到即
// 跳过本轮。任务本身仍须可重复、可并发执行，锁只是减少重复工作，不是正确性的前提。单轮失败只记录日志与计数，不终止进程。
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/db"
)

// Job 是一个周期任务。Run 返回本轮处理的数量（只用于日志）。Run 收到的 context 不带请求上下文，任务自行以服务主体调用。
type Job struct {
	Name    string        // 任务名，形如 <子系统>.<任务>，全进程唯一
	Every   time.Duration // 周期，至少 1 秒
	Timeout time.Duration // 单轮上限；零值为 Every
	Run     func(ctx context.Context) (int, error)
}

// Bound 是绑定到所属子系统数据库的任务；DB 为 nil 时不加锁（子系统没有数据库）。
type Bound struct {
	Job
	DB *db.DB
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z][a-z0-9-]*$`)

// Validate 检查任务声明：名称格式与唯一、周期与上限、Run 非空。
func Validate(jobs []Bound) error {
	seen := map[string]bool{}
	for _, j := range jobs {
		switch {
		case !namePattern.MatchString(j.Name):
			return fmt.Errorf("scheduler：任务名 %q 无效（应为 <子系统>.<任务>）", j.Name)
		case seen[j.Name]:
			return fmt.Errorf("scheduler：任务 %s 重复", j.Name)
		case j.Every < time.Second:
			return fmt.Errorf("scheduler：任务 %s 的周期至少 1 秒", j.Name)
		case j.Timeout < 0 || j.Timeout > j.Every:
			return fmt.Errorf("scheduler：任务 %s 的单轮上限不能超过周期", j.Name)
		case j.Run == nil:
			return fmt.Errorf("scheduler：任务 %s 没有 Run", j.Name)
		}
		seen[j.Name] = true
	}
	return nil
}

// Outcome 是一轮执行的结果。
type Outcome string

const (
	Ran     Outcome = "ran"
	Skipped Outcome = "skipped" // 其他实例持有锁
	Failed  Outcome = "failed"
)

// Runner 运行一组任务。OnRound 在每轮结束后调用（测试与指标用，可为 nil）。
type Runner struct {
	Jobs    []Bound
	Log     *slog.Logger
	OnRound func(job string, outcome Outcome, n int, err error)
}

func (r *Runner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Run 运行全部任务直到 ctx 取消。任务声明无效时立即返回错误。
func (r *Runner) Run(ctx context.Context) error {
	if err := Validate(r.Jobs); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, j := range r.Jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(ctx, j)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func (r *Runner) loop(ctx context.Context, j Bound) {
	ticker := time.NewTicker(j.Every)
	defer ticker.Stop()
	for {
		// select 在退出与节拍同时就绪时随机选择：先检查退出，取消后不再开始新的一轮
		if ctx.Err() != nil {
			return
		}
		// 同一任务在本实例内串行：本轮结束前不会开始下一轮，期间到达的节拍由 ticker 合并丢弃
		r.Once(ctx, j)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once 执行任务一轮（取锁 → 执行 → 释放），返回结果。导出供测试与运维手工触发使用。
func (r *Runner) Once(ctx context.Context, j Bound) Outcome {
	timeout := j.Timeout
	if timeout == 0 {
		timeout = j.Every
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	outcome, n, err := r.once(rctx, j)
	if outcome == Failed && ctx.Err() == nil {
		r.logger().WarnContext(ctx, "定时任务失败", "job", j.Name, "error", err)
	} else if outcome == Ran && n > 0 {
		r.logger().InfoContext(ctx, "定时任务", "job", j.Name, "processed", n)
	}
	if r.OnRound != nil {
		r.OnRound(j.Name, outcome, n, err)
	}
	return outcome
}

func (r *Runner) once(ctx context.Context, j Bound) (Outcome, int, error) {
	if j.DB == nil {
		n, err := j.Run(ctx)
		if err != nil {
			return Failed, n, err
		}
		return Ran, n, nil
	}
	conn, err := j.DB.Pool.Acquire(ctx)
	if err != nil {
		return Failed, 0, err
	}
	defer conn.Release()
	key := "scheduler/" + j.Name
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&locked); err != nil {
		return Failed, 0, err
	}
	if !locked {
		return Skipped, 0, nil
	}
	defer func() {
		// 释放锁不受本轮超时影响；释放失败时销毁连接，会话结束即释放会话级锁
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			_ = conn.Conn().Close(uctx)
		}
	}()
	n, err := j.Run(ctx)
	if err != nil {
		return Failed, n, err
	}
	return Ran, n, nil
}
