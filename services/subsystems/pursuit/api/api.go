// Package api 是 pursuit 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/pursuit.md。
package api

import (
	"context"
	"time"

	pur "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// Name 是子系统名。
const Name = "pursuit"

// Error 是可在传输边界暴露的错误（稳定 code）；基础设施错误不直接暴露。Current 是冲突时跟进的当前值（契约 PursuitState 的 JSON），
// SignalCurrent 是 signal_updated 时信号的当前版本（契约 SignalVersion 的 JSON）。
type Error struct {
	Status        int
	Code          string
	Detail        string
	Current       []byte
	SignalCurrent []byte
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Code + "：" + e.Detail
	}
	return e.Code
}

// 契约类型（contracts/apis/pursuit/openapi.yaml）。
type (
	CreatePursuitRequest = pur.CreatePursuitRequest
	CompleteTaskRequest  = pur.CompleteTaskRequest
	ReopenTaskRequest    = pur.ReopenTaskRequest
	CommandResult        = pur.CommandResult
	TaskView             = pur.TaskView
	MyTaskList           = pur.MyTaskList
	Pursuit              = pur.Pursuit
	Tracking             = pur.Tracking
)

// ActionsBinding 是渠道内跟进动作在绑定表中的名称（PRD 第5段第26条延后的卡片“跟进”按钮与第6段 F9 的提醒卡片“完成”）。
const ActionsBinding = "pursuit.Actions"

// Actions 供 channels 以点击者（经有效绑定核验的收件人本人）的用户主体调用，授权与对应的公开接口相同（创建为目标上的 can_assign，
// 完成与撤销完成为推进的 can_edit），channels 不另作判断；channel 为 wecom 的请求只经本接口（本地绑定或 mTLS 内部端口）接受。
//
//   - CreatePursuit：卡片上的“跟进”（契约 createPursuit；负责人为点击者本人、没有任务）；
//   - GetTask：提醒卡片发送前与点击后读取任务与调用者的当前权限（契约 getTask）；
//   - CompleteTask / ReopenTask：提醒卡片的“完成”与“撤销”（契约 completeTask、reopenTask）；
//   - ListMyTasks：简报“待我处理”中的任务，以收件人本人读取（契约 listMyTasks；before 为空表示不限，limit 1–100，cursor 为上一页的签名游标）；
//   - GetPursuit：授权被拒的操作者通知发送前以操作者本人再检查能否查看该跟进（契约 getPursuit）。
//
// 命令不在调用管线内自动重试：每次尝试带调用方自己的幂等键，暂时失败由调用方以新的幂等键、同一操作标识重试。
type Actions interface {
	CreatePursuit(ctx context.Context, key string, req *CreatePursuitRequest) (*CommandResult, error)
	GetTask(ctx context.Context, task string) (*TaskView, error)
	CompleteTask(ctx context.Context, task, key string, req *CompleteTaskRequest) (*CommandResult, error)
	ReopenTask(ctx context.Context, task, key string, req *ReopenTaskRequest) (*CommandResult, error)
	ListMyTasks(ctx context.Context, before *time.Time, limit int, cursor string) (*MyTaskList, error)
	GetPursuit(ctx context.Context, pursuit string) (*Pursuit, error)
}

// LocalActions 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func LocalActions(impl Actions) Actions { return localActions{impl} }

type localActions struct{ impl Actions }

func (l localActions) CreatePursuit(ctx context.Context, key string, req *CreatePursuitRequest) (*CommandResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "CreatePursuit", Timeout: 10 * time.Second},
		func(ctx context.Context) (*CommandResult, error) { return l.impl.CreatePursuit(ctx, key, req) })
}

func (l localActions) GetTask(ctx context.Context, task string) (*TaskView, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetTask", Idempotent: true},
		func(ctx context.Context) (*TaskView, error) { return l.impl.GetTask(ctx, task) })
}

func (l localActions) CompleteTask(ctx context.Context, task, key string, req *CompleteTaskRequest) (*CommandResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "CompleteTask"},
		func(ctx context.Context) (*CommandResult, error) { return l.impl.CompleteTask(ctx, task, key, req) })
}

func (l localActions) ReopenTask(ctx context.Context, task, key string, req *ReopenTaskRequest) (*CommandResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ReopenTask"},
		func(ctx context.Context) (*CommandResult, error) { return l.impl.ReopenTask(ctx, task, key, req) })
}

func (l localActions) ListMyTasks(ctx context.Context, before *time.Time, limit int, cursor string) (*MyTaskList, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ListMyTasks", Idempotent: true},
		func(ctx context.Context) (*MyTaskList, error) { return l.impl.ListMyTasks(ctx, before, limit, cursor) })
}

func (l localActions) GetPursuit(ctx context.Context, pursuit string) (*Pursuit, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetPursuit", Idempotent: true},
		func(ctx context.Context) (*Pursuit, error) { return l.impl.GetPursuit(ctx, pursuit) })
}

// TrackingsBinding 是持续跟踪读取在绑定表中的名称（C-11/C-12：授权被拒通知的操作者为持续跟踪时，channels 以操作者本人再检查）。
const TrackingsBinding = "pursuit.Trackings"

// Trackings 供 channels 以收件人本人的用户主体调用，授权与公开接口 getTracking 相同（推进的 can_view，每次按当前权限判断；交易型跟进、
// 他租户、不存在与无权查看同为 404 pursuit_not_found）。
type Trackings interface {
	GetTracking(ctx context.Context, tracking string) (*Tracking, error)
}

// LocalTrackings 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func LocalTrackings(impl Trackings) Trackings { return localTrackings{impl} }

type localTrackings struct{ impl Trackings }

func (l localTrackings) GetTracking(ctx context.Context, tracking string) (*Tracking, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetTracking", Idempotent: true},
		func(ctx context.Context) (*Tracking, error) { return l.impl.GetTracking(ctx, tracking) })
}

// RemindersBinding 是任务提醒的持久定时器与授权同步重试在绑定表中的名称（本地绑定；定时任务角色尚未接入，
// 见 docs/05-delivery/09-a02-end-to-end-acceptance.md 缺口 R-01）。
const RemindersBinding = "pursuit.Reminders"

// Reminders 以带租户的服务主体请求上下文调用：
//   - RunDue 处理到时（fire_at 不晚于 now）的已计划提醒，至多 limit 条：按当前值再检查（跟进进行中、任务待办、截止版本未变、
//     参与版本已同步、任务负责人仍是跟进的负责人或协作者且仍能更新该跟进），通过的发布 pursuit.task.due.v1 并标为已发送（每个任务、
//     截止版本与种类只一次）；负责人无效时转给目标所在单元的决策者（escalated）；参与版本尚未同步的留到同步完成后；同一任务同一截止
//     版本有更晚的已到时提醒时只发最晚的一条。返回已发送的数量。可重复执行，并发执行不重复发送。
//   - RetrySync 重试授权同步待处理的跟进（期望版本高于已应用版本），至多 limit 个，返回本次同步成功的数量。
//   - RunTracking（C-10，持续跟踪 F10）结束到达周期时点（next_run_at 不晚于 now）的跟踪中各期，至多 limit 个跟踪：每个跟踪在自己的事务
//     中加行锁（跳过已被并发执行者锁住的）后按当前值再检查（仍为跟踪中、时点仍已到），记录一期（实际期间为进行中的一期的开始到不晚于
//     now 的最近一个周期时点，期末成员快照与对比基准）并发布 pursuit.tracking.period_closed.v1，再计划下一期。暂停与停止的不处理，
//     恢复后不补。返回本次结束的期数。可重复执行，并发执行不重复。
type Reminders interface {
	RunDue(ctx context.Context, now time.Time, limit int) (int, error)
	RetrySync(ctx context.Context, limit int) (int, error)
	RunTracking(ctx context.Context, now time.Time, limit int) (int, error)
}

// LocalReminders 把本地实现包装为经调用管线的接口。
func LocalReminders(impl Reminders) Reminders { return localReminders{impl} }

type localReminders struct{ impl Reminders }

func (l localReminders) RunDue(ctx context.Context, now time.Time, limit int) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RunDueReminders", Timeout: 60 * time.Second},
		func(ctx context.Context) (int, error) { return l.impl.RunDue(ctx, now, limit) })
}

func (l localReminders) RetrySync(ctx context.Context, limit int) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RetryAuthorizationSync", Timeout: 60 * time.Second},
		func(ctx context.Context) (int, error) { return l.impl.RetrySync(ctx, limit) })
}

func (l localReminders) RunTracking(ctx context.Context, now time.Time, limit int) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RunTrackingPeriods", Timeout: 60 * time.Second},
		func(ctx context.Context) (int, error) { return l.impl.RunTracking(ctx, now, limit) })
}
