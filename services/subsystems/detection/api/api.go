// Package api 是 detection 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/detection.md。
package api

import (
	"context"
	"time"

	detectionapi "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// Name 是子系统名。
const Name = "detection"

// Error 是可在传输边界暴露的错误（稳定 code）；基础设施错误不直接暴露。Current 是冲突与 signal_updated 时信号的当前值
// （契约 SignalState 的 JSON），其余情况为空。
type Error struct {
	Status  int
	Code    string
	Detail  string
	Current []byte
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Code + "：" + e.Detail
	}
	return e.Code
}

// ExplainerBinding 是信号固定求值输入读取接口在绑定表中的名称。
const ExplainerBinding = "detection.SignalExplainer"

// Explanation 是契约生成类型（contracts/apis/detection/openapi.yaml 的 Explanation）。
type Explanation = detectionapi.Explanation

// SignalExplainer 以请求上下文中的用户主体读取信号的命中理由与固定的求值输入（gateway 对象页的信号上下文与“信号判断时”视角，
// PRD 第4段 F1、F7）。授权与 explainSignal 相同：信号所属目标的 can_view_signals，按 customer-context 的当前口径判断；不属于本租户、
// 不存在与无权查看同为 404 signal_not_found 的 *Error（调用方据此只显示公共部分，不提示信号是否存在）；不可用为 503。只读、幂等。
type SignalExplainer interface {
	Explain(ctx context.Context, signalID string) (*Explanation, error)
}

// LocalExplainer 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func LocalExplainer(impl SignalExplainer) SignalExplainer { return localExplainer{impl} }

type localExplainer struct{ impl SignalExplainer }

func (l localExplainer) Explain(ctx context.Context, signalID string) (*Explanation, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ExplainSignal", Idempotent: true},
		func(ctx context.Context) (*Explanation, error) { return l.impl.Explain(ctx, signalID) })
}

// BriefingBinding 是简报取数、信号卡片与渠道内处理接口在绑定表中的名称（PRD 第5段 F1、F4、F8）。
const BriefingBinding = "detection.Briefing"

// 以下是契约生成类型（contracts/apis/detection/openapi.yaml）。
type (
	BriefingSignalPage = detectionapi.BriefingSignalPage
	BriefingSignal     = detectionapi.BriefingSignal
	SignalRow          = detectionapi.SignalRow
	SignalCard         = detectionapi.SignalCard
	SignalState        = detectionapi.SignalState
	SignalCardList     = detectionapi.SignalCardList
	TriageRequest      = detectionapi.TriageRequest
	TriageResult       = detectionapi.TriageResult
	UndoRequest        = detectionapi.UndoRequest
	UndoResult         = detectionapi.UndoResult
)

// Briefing 供 channels 以收件人（或经有效绑定核验的点击者）的用户主体调用；授权与对应的公开接口相同（目标的 can_view_signals、
// 处理与反馈另需 can_feedback_signals，按 customer-context 的当前口径判断），channels 不另作判断。
//
//   - BriefingSignals：一个目标在覆盖窗口 [from, to) 内的简报条目（契约 listBriefingSignals）；目标不可见为 404 objective_not_found、
//     无权为 403 forbidden；只读、幂等。
//   - SignalCards：按标识读取调用者可见的信号的当前卡片（契约 listSignalCards）；不可见、不存在与其他租户的信号不出现。
//   - Triage：处理一条信号（契约 triageSignal，channel 为 wecom）；同一幂等键重放同一结果，结果固定在 operation_id 上。
//     冲突与 signal_updated 的 *Error 带 Current（当前 SignalState 的 JSON）。
//   - Undo：撤销本人的处理（契约 undoSignalTriage）。
//
// Triage 与 Undo 不在调用管线内自动重试：每次尝试带调用方自己的幂等键，暂时失败由调用方以新的幂等键、同一操作标识重试（PRD 第5段 F5 第3–4条）。
type Briefing interface {
	BriefingSignals(ctx context.Context, objective string, from, to time.Time) (*BriefingSignalPage, error)
	SignalCards(ctx context.Context, ids []string) (*SignalCardList, error)
	Triage(ctx context.Context, signal, key string, req *TriageRequest) (*TriageResult, error)
	Undo(ctx context.Context, key string, req *UndoRequest) (*UndoResult, error)
}

// LocalBriefing 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func LocalBriefing(impl Briefing) Briefing { return localBriefing{impl} }

type localBriefing struct{ impl Briefing }

func (l localBriefing) BriefingSignals(ctx context.Context, objective string, from, to time.Time) (*BriefingSignalPage, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ListBriefingSignals", Idempotent: true},
		func(ctx context.Context) (*BriefingSignalPage, error) {
			return l.impl.BriefingSignals(ctx, objective, from, to)
		})
}

func (l localBriefing) SignalCards(ctx context.Context, ids []string) (*SignalCardList, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ListSignalCards", Idempotent: true},
		func(ctx context.Context) (*SignalCardList, error) { return l.impl.SignalCards(ctx, ids) })
}

func (l localBriefing) Triage(ctx context.Context, signal, key string, req *TriageRequest) (*TriageResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "TriageSignal"},
		func(ctx context.Context) (*TriageResult, error) { return l.impl.Triage(ctx, signal, key, req) })
}

func (l localBriefing) Undo(ctx context.Context, key string, req *UndoRequest) (*UndoResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "UndoSignalTriage"},
		func(ctx context.Context) (*UndoResult, error) { return l.impl.Undo(ctx, key, req) })
}

// CandidatesBinding 是信号候选负责人读取接口在绑定表中的名称（PRD 第6段 F1 第2条，C-14）。
const CandidatesBinding = "detection.SignalCandidates"

// SignalCandidate 是契约生成类型（contracts/apis/detection/openapi.yaml 的 SignalCandidate）。
type SignalCandidate = detectionapi.SignalCandidate

// SignalCandidates 供 pursuit 的创建面板以调用者的用户主体读取信号生成时按目标分派规则算出的候选负责人（契约 getSignalCandidateOwner）。
// 授权：不可见、不存在与其他租户的信号为 404 signal_not_found，能查看而没有 can_assign 为 403 forbidden；不可用为 503。只读、幂等。
// 返回的候选只是生成时的建议：调用方以候选本人再检查成员状态与目标访问。
type SignalCandidates interface {
	SignalCandidate(ctx context.Context, signalID string) (*SignalCandidate, error)
}

// LocalCandidates 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func LocalCandidates(impl SignalCandidates) SignalCandidates { return localCandidates{impl} }

type localCandidates struct{ impl SignalCandidates }

func (l localCandidates) SignalCandidate(ctx context.Context, signalID string) (*SignalCandidate, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetSignalCandidateOwner", Idempotent: true},
		func(ctx context.Context) (*SignalCandidate, error) { return l.impl.SignalCandidate(ctx, signalID) })
}
