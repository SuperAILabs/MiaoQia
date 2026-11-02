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
//   - Organizations 读取组织在实体图（entity-graph）中的当前版本（C-13，PRD 第6段 F10 第1、3条）：持续跟踪按名单导入对象时核对组织
//     是否存在，结束一期时记录期末每个对象的实体来源版本。entity-graph 尚未实现（验收线 D-12），生产缺省为未接入（NotWired）：导入只校验
//     标识格式并说明 not_wired，各期的对象记为 not_wired；未接入、暂时不可用与“不存在”三者不混同。测试可注入内存实现。

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

// Organizations 返回给定组织标识在实体图中的当前版本（实体版本，契约 entity.entity.*.v1 的 entity_version）；不存在的组织不在结果中。
// 返回 ErrNotWired 表示实体图未接入，其他错误按暂时不可用处理。以调用者的请求上下文调用（组织为公共数据，不按租户过滤）。
type Organizations interface {
	OrganizationVersions(ctx context.Context, ids []string) (map[string]int64, error)
}

// NotWired 是缺省的出站端口：所有调用返回 ErrNotWired。
type NotWired struct{}

// CandidateOwner 返回 ErrNotWired。
func (NotWired) CandidateOwner(context.Context, string, string) (string, error) {
	return "", ErrNotWired
}

// OrganizationVersions 返回 ErrNotWired。
func (NotWired) OrganizationVersions(context.Context, []string) (map[string]int64, error) {
	return nil, ErrNotWired
}
