package gateway

import (
	"slices"

	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

// route 返回可用于该能力与分级的端点，按主选在前排序。只返回合规端点：没有时由调用方返回明确错误，不降级到不合规模型。
// 决策能力另按 task（任务@版本）过滤，只返回声明了该任务版本的端点；目录校验保证这些端点的输出语义一致。
func (c *Catalog) route(capability api.Capability, class api.Classification, task string) ([]Endpoint, error) {
	switch class {
	case api.Public, api.Internal:
	case api.Sensitive:
		// 敏感数据只能经租户显式授权的指定供应商处理；授权登记尚未接入，一律拒绝。
		return nil, &api.Error{Code: api.CodeSensitiveNotAllowed}
	default:
		return nil, &api.Error{Code: api.CodeInvalidRequest, Detail: "未知数据分级"}
	}
	var out []Endpoint
	for _, e := range c.Endpoints {
		if !e.Filed || !e.has(capability) || !slices.Contains(c.AllowedRegions, e.Region) {
			continue
		}
		if capability.Decision() {
			if _, ok := e.decisionTask(task); !ok {
				continue
			}
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		detail := string(capability)
		if capability.Decision() {
			detail += " " + task
		}
		return nil, &api.Error{Code: api.CodeNoEligibleModel, Detail: detail}
	}
	slices.SortStableFunc(out, func(a, b Endpoint) int {
		if a.Tier != b.Tier {
			return a.Tier - b.Tier
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out, nil
}
