// Package api 是 channels 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/channels.md。
//
// 对外 HTTP 接口（安装、绑定确认、解绑、卡片跳转）见 contracts/apis/channels/openapi.yaml。本包另提供三个本地绑定：
//   - Callbacks：渠道侧入口——安装授权回调、消息回调（验签、去重）与渠道内置浏览器免登。它们不带妙洽令牌，
//     对外路由随网关的未认证入口接入（尚未实现），在此之前只以本地绑定提供；
//   - Messaging：卡片详情链接与投递目标（简报、通知等出站编排随 PRD 第5段实现）；
//   - Maintenance：撤销重试、失效完成与免登会话对账（定时任务角色随所属里程碑接入）；
//   - Briefings：简报组装、投递与卡片动作执行（PRD 第5段；定时任务与工作进程随所属里程碑接入）。
package api

import (
	"context"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/binding"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Name 是子系统名。
const Name = "channels"

// Error 是可在传输边界暴露的错误（稳定 code）；基础设施错误不外泄。Recipient 只用于 account_mismatch。
type Error struct {
	Status    int
	Code      string
	Recipient string
}

func (e *Error) Error() string { return e.Code }

// CallbacksBinding 是渠道侧入口在绑定表中的名称。
const CallbacksBinding = "channels.Callbacks"

// InboundOutcome 是一次消息回调的处理结果。
type InboundOutcome string

const (
	InboundGuideSent InboundOutcome = "guide_sent" // 未绑定账号：发出绑定引导（一次性绑定链接），不回答业务内容
	InboundBound     InboundOutcome = "bound"      // 已绑定账号：交给会话处理（渠道内对话尚未实现）
	InboundIgnored   InboundOutcome = "ignored"    // 渠道未安装或已停用
	InboundDuplicate InboundOutcome = "duplicate"  // 同一事件已处理过（重放）：不改变任何状态
	InboundQueued    InboundOutcome = "queued"     // 卡片按钮：令牌签名与租户核对通过，点击已排队异步执行（1 秒内确认）
	InboundRejected  InboundOutcome = "rejected"   // 卡片按钮：令牌被篡改、伪造或属于其他租户；与不存在相同，只写审计，不触及令牌
)

// InboundResult 是消息回调的处理结果。
type InboundResult struct {
	Outcome InboundOutcome
}

// SSOResult 是渠道内置浏览器免登的结果：Outcome 为 session 时 Session 是 identity 签发的免登会话，
// 为 login 时转到登录页，Reason 说明原因（channel_oauth_unavailable、channel_oauth_invalid、channel_not_installed、
// not_bound、binding_inactive、member_inactive、identity_unavailable、session_rejected）。渠道或 identity 不可用时是
// “不可用”，不会被当作“未绑定”。
type SSOResult struct {
	Outcome string
	Reason  string
	Tenant  string
	Subject string
	Session *identity.ChannelSession
}

// Installation 是安装回调后的安装状态摘要。
type Installation struct {
	ID, Tenant, State, CorpID string
	Version                   int64
}

// Callbacks 是渠道侧入口。调用上下文为不带租户的服务主体（网关的未认证入口），租户只从已登记的企业路由派生：
//   - InstallCallback：渠道官方授权页的回调（state 一次性、10分钟有效）；成功后安装为 pending_verification；
//   - ReceiveInbound：消息回调。先验签（失败计入渠道健康，不改变状态），再按渠道事件标识去重、核对时间窗（重放不改变状态）；
//     未绑定账号只收到绑定引导；已绑定账号经 identity 核对绑定仍属于成员当前的成员资格，来自退出之前的旧绑定以 member_left
//     失效后按未绑定发出引导；identity 不可用时返回 503 且不写入任何状态（含去重记录），由渠道重发；
//   - ChannelSSO：渠道内置浏览器打开妙洽链接时，经渠道网页授权取得当前渠道账号，核对权威绑定后请 identity 签发免登会话。
type Callbacks interface {
	InstallCallback(ctx context.Context, channel, state, code string) (*Installation, error)
	ReceiveInbound(ctx context.Context, channel string, cb *Callback) (*InboundResult, error)
	ChannelSSO(ctx context.Context, channel, oauthCode string) (*SSOResult, error)
}

// MessagingBinding 是卡片链接与投递目标在绑定表中的名称。
const MessagingBinding = "channels.Messaging"

// CardLink 是一条卡片详情链接：Token 只在创建时返回，channels 只保存摘要。
type CardLink struct {
	ID, Token, Recipient, Object string
	ExpiresAt                    time.Time
}

// DeliveryTarget 是给成员投递消息的目标：Channel 为 wecom 时 BindingID 是有效绑定；为 in_app 时 Reason 说明为何改投站内
// （channel_not_enabled 渠道未启用或已停用；not_bound 未绑定或绑定已失效/失效中）。
type DeliveryTarget struct {
	Channel   string
	BindingID string
	Reason    string
}

// Messaging 供出站编排使用（以带租户的服务主体调用）：
//   - CreateCardLink 为收件人与对象生成卡片详情链接（72小时有效）；
//   - DeliveryTarget 返回投递目标：只有渠道已启用、绑定为 bound 且经 identity 核对仍属于成员当前的成员资格（绑定的成员版本高于
//     最近一次退出时的成员版本）时投到渠道，否则改投站内；来自退出之前的旧绑定随即以 member_left 失效。查询失败或 identity
//     不可用时返回错误（503，可重试），既不投到渠道，也不当作“未绑定”。
type Messaging interface {
	CreateCardLink(ctx context.Context, recipient, object string) (*CardLink, error)
	DeliveryTarget(ctx context.Context, subject string) (*DeliveryTarget, error)
}

// MaintenanceBinding 是 channels 定时维护在绑定表中的名称。
const MaintenanceBinding = "channels.Maintenance"

// Maintenance 以带租户的服务主体调用，可重复执行：
//   - RetryRevocations 对失效中的绑定重试同步撤销（每次至多 limit 条，按上次尝试时间轮转，失败的不会挡住其他绑定），
//     并把“撤销已确认且最后一张凭据已过期”的绑定标为已失效；返回本次标为已失效的数量；
//   - ReconcileChannelSessions 分页读取 identity 中仍有有效免登会话的全部绑定，对不是 bound 的（含 channels 中不存在的）
//     撤销其会话；channels 查询失败时中止，不把未知当作失效。返回撤销的绑定数。
type Maintenance interface {
	RetryRevocations(ctx context.Context, limit int) (int, error)
	ReconcileChannelSessions(ctx context.Context) (int, error)
}

// BriefingsBinding 是简报组装、投递与卡片动作执行在绑定表中的名称（定时任务与工作进程随所属里程碑接入，在此之前由测试与运维调用）。
const BriefingsBinding = "channels.Briefings"

// ComposeResult 是一次组装的结果：Outcome 为 composed（新生成）、existing（同一期已生成，返回已有的一份）、late（晚于当日 12:00，
// 不推送，各区窗口不前移）或 skipped（成员不是有效成员，或尚无已到计划发送时间的周期）。
type ComposeResult struct {
	Outcome    string
	BriefingID string
	DeliveryID string
}

// Briefings 以带租户的服务主体调用，均可重复执行：
//   - Compose 为成员组装 at 时最近一个已到计划发送时间的周期（幂等键：租户 + 成员 + 简报来源 + 周期起止）；按成员本人的权限
//     逐区取数（以成员的用户主体调用 detection 与 customer-context），保存快照，需要推送时创建投递并立即尝试一次；
//   - DeliverDue 处理到期的投递（每次至多 limit 条）：每次发送前按收件人当前状态再检查，失败退避重试到当日 18:00；
//   - ProcessActions 执行排队的卡片点击（每次至多 limit 条），按 F4 第6条的顺序检查后调用 detection。
type Briefings interface {
	Compose(ctx context.Context, subject string, at time.Time) (*ComposeResult, error)
	DeliverDue(ctx context.Context, limit int) (int, error)
	ProcessActions(ctx context.Context, limit int) (int, error)
}

func LocalBriefings(impl Briefings) Briefings { return localBriefings{impl} }

type localBriefings struct{ impl Briefings }

func (l localBriefings) Compose(ctx context.Context, subject string, at time.Time) (*ComposeResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ComposeBriefing", Timeout: 60 * time.Second},
		func(ctx context.Context) (*ComposeResult, error) { return l.impl.Compose(ctx, subject, at) })
}

func (l localBriefings) DeliverDue(ctx context.Context, limit int) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "DeliverDue", Timeout: 60 * time.Second},
		func(ctx context.Context) (int, error) { return l.impl.DeliverDue(ctx, limit) })
}

func (l localBriefings) ProcessActions(ctx context.Context, limit int) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ProcessActions", Timeout: 60 * time.Second},
		func(ctx context.Context) (int, error) { return l.impl.ProcessActions(ctx, limit) })
}

// LocalCallbacks 等把本地实现包装为经调用管线的接口。
func LocalCallbacks(impl Callbacks) Callbacks { return localCallbacks{impl} }

type localCallbacks struct{ impl Callbacks }

func (l localCallbacks) InstallCallback(ctx context.Context, channel, state, code string) (*Installation, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "InstallCallback"},
		func(ctx context.Context) (*Installation, error) {
			return l.impl.InstallCallback(ctx, channel, state, code)
		})
}

func (l localCallbacks) ReceiveInbound(ctx context.Context, channel string, cb *Callback) (*InboundResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ReceiveInbound"},
		func(ctx context.Context) (*InboundResult, error) { return l.impl.ReceiveInbound(ctx, channel, cb) })
}

func (l localCallbacks) ChannelSSO(ctx context.Context, channel, oauthCode string) (*SSOResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ChannelSSO", Timeout: 30 * time.Second},
		func(ctx context.Context) (*SSOResult, error) { return l.impl.ChannelSSO(ctx, channel, oauthCode) })
}

func LocalMessaging(impl Messaging) Messaging { return localMessaging{impl} }

type localMessaging struct{ impl Messaging }

func (l localMessaging) CreateCardLink(ctx context.Context, recipient, object string) (*CardLink, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "CreateCardLink"},
		func(ctx context.Context) (*CardLink, error) { return l.impl.CreateCardLink(ctx, recipient, object) })
}

func (l localMessaging) DeliveryTarget(ctx context.Context, subject string) (*DeliveryTarget, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "DeliveryTarget", Idempotent: true},
		func(ctx context.Context) (*DeliveryTarget, error) { return l.impl.DeliveryTarget(ctx, subject) })
}

func LocalMaintenance(impl Maintenance) Maintenance { return localMaintenance{impl} }

type localMaintenance struct{ impl Maintenance }

func (l localMaintenance) RetryRevocations(ctx context.Context, limit int) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RetryRevocations", Timeout: 60 * time.Second},
		func(ctx context.Context) (int, error) { return l.impl.RetryRevocations(ctx, limit) })
}

func (l localMaintenance) ReconcileChannelSessions(ctx context.Context) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ReconcileChannelSessions", Timeout: 60 * time.Second},
		func(ctx context.Context) (int, error) { return l.impl.ReconcileChannelSessions(ctx) })
}
