package app

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/api"
)

// 覆盖判断（GetEntitlements 的数据覆盖部分，commerce 第4、8节）。产品目录与订阅接入（M2-P8）之前，覆盖包来自部署配置，
// 对全部租户相同；未配置时覆盖判断不可用（503），不当作全部覆盖。
var (
	domainPattern = regexp.MustCompile(`^INF-[A-Z]+$`)
	regionPattern = regexp.MustCompile(`^[0-9]{6}$`)
)

// AllRegions 是覆盖包中“全部区域”的写法。
const AllRegions = "*"

// CoveragePlan 是信息领域 → 覆盖的行政区划前缀（省级两位、地级四位、县级六位；AllRegions 为全部区域）。
type CoveragePlan map[string][]string

// ParseCoverage 读取 MIAOQIA_COMMERCE_COVERAGE：以分号分隔的“信息领域=区划,区划”，区划为 GB/T 2260 六位代码或 *，
// 例如 INF-PROC=330000,310000;INF-ORG=*。省级代码（后四位为 0）包含该省全部区划，地级代码（后两位为 0）包含该市全部区划。
// 空串返回 nil（未配置）。格式错误、重复的信息领域或空的区划列表都拒绝启动，不部分生效。
func ParseCoverage(v string) (CoveragePlan, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	plan := CoveragePlan{}
	for _, entry := range strings.Split(v, ";") {
		domain, regions, ok := strings.Cut(strings.TrimSpace(entry), "=")
		domain = strings.TrimSpace(domain)
		if !ok || !domainPattern.MatchString(domain) {
			return nil, fmt.Errorf("commerce：覆盖包条目 %q 无效（应为 INF-XXX=区划,区划）", entry)
		}
		if _, dup := plan[domain]; dup {
			return nil, fmt.Errorf("commerce：覆盖包中信息领域 %s 重复", domain)
		}
		var prefixes []string
		for _, r := range strings.Split(regions, ",") {
			r = strings.TrimSpace(r)
			switch {
			case r == AllRegions:
				prefixes = append(prefixes, AllRegions)
			case regionPattern.MatchString(r) && r != "000000" && !strings.HasPrefix(r, "00"):
				prefixes = append(prefixes, regionPrefix(r))
			default:
				return nil, fmt.Errorf("commerce：覆盖包中 %s 的区划 %q 无效（应为六位代码或 *）", domain, r)
			}
		}
		if slices.Contains(prefixes, AllRegions) && len(prefixes) > 1 {
			return nil, fmt.Errorf("commerce：覆盖包中 %s 的 * 不能与具体区划同时出现", domain)
		}
		plan[domain] = prefixes
	}
	return plan, nil
}

// regionPrefix 返回区划代码的包含前缀：省级 330000 → 33，地级 330800 → 3308，县级为完整代码。
func regionPrefix(code string) string {
	switch {
	case strings.HasSuffix(code, "0000"):
		return code[:2]
	case strings.HasSuffix(code, "00"):
		return code[:4]
	}
	return code
}

// Decide 判断（信息领域、区划）是否在覆盖包内。区划未知时只有“全部区域”覆盖；信息领域不在包内时只报告信息领域缺失。
func (p CoveragePlan) Decide(infoDomain, region string) *api.CoverageDecision {
	prefixes, ok := p[infoDomain]
	if !ok {
		return &api.CoverageDecision{Missing: []string{api.MissingInfoDomain}}
	}
	for _, prefix := range prefixes {
		if prefix == AllRegions || (region != "" && strings.HasPrefix(region, prefix)) {
			return &api.CoverageDecision{Covered: true, Missing: []string{}}
		}
	}
	return &api.CoverageDecision{Missing: []string{api.MissingRegion}}
}

// CheckCoverage 是 api.Coverage 的本地实现：调用者须为带租户的用户主体；覆盖包未配置为 503 coverage_unavailable。
func (s *Service) CheckCoverage(ctx context.Context, infoDomain, region string) (*api.CoverageDecision, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return nil, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	if rc.Principal.Kind != "user" {
		return nil, failure(403, "forbidden")
	}
	if !domainPattern.MatchString(infoDomain) || (region != "" && !regionPattern.MatchString(region)) {
		return nil, &api.Error{Status: 400, Code: "invalid_request", Detail: "info_domain 或 region 无效"}
	}
	if s.Coverage == nil {
		return nil, failure(503, "coverage_unavailable")
	}
	return s.Coverage.Decide(infoDomain, region), nil
}

// Coverage 是 api.Coverage 的本地实现。
type Coverage struct{ Service *Service }

func (c Coverage) CheckCoverage(ctx context.Context, infoDomain, region string) (*api.CoverageDecision, error) {
	return c.Service.CheckCoverage(ctx, infoDomain, region)
}
