package pursuit

import (
	"context"
	"errors"
	"fmt"

	det "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// detectionCandidates 是 api.CandidateOwners 的生产适配器（C-14）：以发起请求的用户主体经 detection 的 SignalCandidates（本地或远程
// 绑定）读取信号生成时按目标分派规则算出的候选负责人。detection 只返回生成时记录的值；候选此刻是否仍有效（租户有效成员、能查看该目标的
// 信号）由 GetCreateDefaults 以候选本人再检查，存储的候选只是提示。
//
// 映射：assigned → 候选标识；none → 没有候选（空串）；rules_unavailable（信号生成时分派规则不可用）→ api.ErrRulesUnavailable（创建面板说明
// “分派规则不可用”）；读取失败、信号不属于所请求的目标或无法识别的状态 → 错误（创建面板 503，失败关闭）。
type detectionCandidates struct{ d detection.SignalCandidates }

func (c detectionCandidates) CandidateOwner(ctx context.Context, objective, signal string) (string, error) {
	if c.d == nil {
		return "", errors.New("pursuit：detection 候选读取未组装")
	}
	out, err := c.d.SignalCandidate(ctx, signal)
	if err != nil {
		return "", fmt.Errorf("pursuit：读取候选负责人：%w", err)
	}
	if out.ObjectiveID != objective || out.SignalID != signal {
		return "", errors.New("pursuit：候选负责人的信号不属于所请求的目标")
	}
	switch out.State {
	case det.SignalCandidateStateAssigned:
		owner, ok := out.CandidateOwner.Get()
		if !ok || owner == "" {
			return "", errors.New("pursuit：候选负责人缺少标识")
		}
		return owner, nil
	case det.SignalCandidateStateNone:
		return "", nil
	case det.SignalCandidateStateRulesUnavailable:
		return "", api.ErrRulesUnavailable
	}
	return "", fmt.Errorf("pursuit：无法识别的候选状态 %s", out.State)
}
