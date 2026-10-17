package gateway

import (
	"slices"

	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

// route 返回可用于该能力与分级的端点，按主选在前排序。只返回合规端点：没有时由调用方返回明确错误，不降级到不合规模型。
func (c *Catalog) route(capability api.Capability, class api.Classification) ([]Endpoint, error) {
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
		if e.Filed && e.has(capability) && slices.Contains(c.AllowedRegions, e.Region) {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, &api.Error{Code: api.CodeNoEligibleModel, Detail: string(capability)}
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
