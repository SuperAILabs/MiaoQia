package bff

import (
	"context"
	"errors"

	commerce "github.com/superailabs/miaoqia/services/subsystems/commerce/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
)

// CommerceCoverage 是 api.Entitlements 的生产适配器：经 commerce 的覆盖判断（GetEntitlements 的数据覆盖部分）判断对象所在信息领域与
// 区划是否在请求上下文中租户的覆盖包内（以用户主体调用）。对象缺少信息领域时不调用 commerce，按判断失败处理（503，不当作覆盖内）。
type CommerceCoverage struct{ Coverage commerce.Coverage }

var errNoInfoDomain = errors.New("gateway：对象缺少信息领域，无法判断覆盖")

func (c CommerceCoverage) Covered(ctx context.Context, object api.Object) (bool, error) {
	if c.Coverage == nil {
		return false, api.ErrNotWired
	}
	if object.InfoDomain == "" {
		return false, errNoInfoDomain
	}
	d, err := c.Coverage.CheckCoverage(ctx, object.InfoDomain, object.Region)
	if err != nil {
		return false, err
	}
	return d.Covered, nil
}
