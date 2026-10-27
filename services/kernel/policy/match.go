package policy

import "strings"

// RegionWithin 判断地点 loc（GB/T 2260 六位代码）是否在区划集合 regions 之内，与表达式函数 region.within 同一语义（000000 为全国，
// 省级 XX0000 与市级 XXXX00 包含其下级）。供求值器之外需要同一口径的匹配使用（例如 detection 按目标的分派规则计算候选负责人）。
func RegionWithin(loc string, regions []string) bool {
	for _, r := range regions {
		if regionCovers(r, loc) {
			return true
		}
	}
	return false
}

// CategoryCovered 判断行业包品类代码 code 本身或其各级上级（按“-”截断）是否在集合 set 中，与表达式函数 category.any_match 对单个
// 品类代码的语义一致。
func CategoryCovered(code string, set []string) bool {
	for s := code; s != ""; {
		for _, c := range set {
			if c == s {
				return true
			}
		}
		i := strings.LastIndex(s, "-")
		if i <= 0 {
			return false
		}
		s = s[:i]
	}
	return false
}
