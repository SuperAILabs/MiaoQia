// Package api 是 detection 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/detection.md。
package api

import (
	"context"

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
