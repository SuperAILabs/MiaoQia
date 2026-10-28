package domain

import "slices"

// 分派规则（customer-context.md 第11节，PRD 第6段 F1 第2条，C-14）：目标的一份按顺序的规则，detection 生成信号时以第一条命中的规则
// 给出候选负责人。候选只是建议，不授予访问；被分派人须是目标的负责人或参与人（由用例层在目标行锁下核对），写入前以其本人再检查访问。

// MaxAssignmentRules 是一个目标的规则条数上限（契约 maxItems）；每条规则的区域与品类各至多 MaxAssignmentValues 个。
const (
	MaxAssignmentRules  = 50
	MaxAssignmentValues = 50
)

// AssignmentRule 是一条分派规则。Regions 与 Categories 都为空时匹配全部信号。
type AssignmentRule struct {
	Assignee   string   `json:"assignee"`
	Regions    []string `json:"regions,omitempty"`
	Categories []string `json:"categories,omitempty"`
}

// ValidateAssignment 校验规则的形式：条数与取值个数上限、区域是有效的行政区划代码、品类属于已安装的某个行业包、
// 同一规则内取值不重复。被分派人的形式与访问由用例层判断。
func ValidateAssignment(c *Catalog, rules []AssignmentRule) error {
	if len(rules) > MaxAssignmentRules {
		return invalid("too_many_rules", "分派规则最多 %d 条", MaxAssignmentRules)
	}
	for i, r := range rules {
		if r.Regions != nil && (len(r.Regions) == 0 || len(r.Regions) > MaxAssignmentValues) {
			return invalid("invalid_rule", "第 %d 条规则的区域须为 1–%d 个", i+1, MaxAssignmentValues)
		}
		if r.Categories != nil && (len(r.Categories) == 0 || len(r.Categories) > MaxAssignmentValues) {
			return invalid("invalid_rule", "第 %d 条规则的品类须为 1–%d 个", i+1, MaxAssignmentValues)
		}
		if err := unique("regions", r.Regions); err != nil {
			return err
		}
		if err := unique("categories", r.Categories); err != nil {
			return err
		}
		for _, v := range r.Regions {
			if !ValidRegion(v) {
				return invalid("invalid_region", "第 %d 条规则的行政区划代码 %s 无效", i+1, v)
			}
		}
		for _, v := range r.Categories {
			if !c.installedCategory(v) {
				return invalid("unknown_category", "第 %d 条规则的品类 %s 不属于已安装的行业包", i+1, v)
			}
		}
	}
	return nil
}

// Assignees 返回规则中出现的被分派人（按首次出现的顺序，不重复）。
func Assignees(rules []AssignmentRule) []string {
	var out []string
	for _, r := range rules {
		if !slices.Contains(out, r.Assignee) {
			out = append(out, r.Assignee)
		}
	}
	return out
}

// installedCategory 判断品类代码是否属于任一已安装行业包的分类。
func (c *Catalog) installedCategory(code string) bool {
	for _, ind := range c.Industries {
		if ind.Categories[code] {
			return true
		}
	}
	return false
}
