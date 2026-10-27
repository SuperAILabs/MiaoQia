package api

import (
	"context"
	"errors"
	"fmt"
)

// 以下是 pursuit 的出站端口（C-10）：
//
//   - CandidateOwners 读取信号上由目标的分派规则给出的候选负责人（PRD 第6段 F1 第2条；customer-context 第11节：customer-context 拥有
//     分派规则，detection 生成信号时按规则算出候选写入信号）。生产组装（C-14）经 detection 的 SignalCandidates 绑定读取（适配器在
//     pursuit 包的 candidates.go）；不服务创建面板的角色缺少该绑定时为“未接入”实现（NotWired）。未接入或信号生成时规则不可用时创建面板的
//     默认负责人退回调用者本人，并说明分派规则不可用（rules_unavailable），不当作“没有候选”。测试可注入内存实现。候选只是建议：pursuit
//     在给出默认值前以候选本人重新判断成员状态与目标访问，候选不因被建议而获得访问。

// ErrNotWired 表示出站端口没有接入。
var ErrNotWired = errors.New("pursuit：出站端口未接入")

// ErrRulesUnavailable 表示信号生成时目标的分派规则不可用（C-14 之前的信号，或 customer-context 未给出分派配置）：与未接入同样说明为
// rules_unavailable（errors.Is(err, ErrNotWired) 为真），不当作“没有候选”。
var ErrRulesUnavailable = fmt.Errorf("pursuit：信号生成时分派规则不可用：%w", ErrNotWired)

// CandidateOwners 返回信号的候选负责人（没有候选时为空串）；以发起请求的用户上下文调用。返回 ErrNotWired 表示分派规则未接入，
// 其他错误按不可用处理（503，失败关闭）。
type CandidateOwners interface {
	CandidateOwner(ctx context.Context, objective, signal string) (string, error)
}

// NotWired 是缺省的出站端口：所有调用返回 ErrNotWired。
type NotWired struct{}

// CandidateOwner 返回 ErrNotWired。
func (NotWired) CandidateOwner(context.Context, string, string) (string, error) {
	return "", ErrNotWired
}
