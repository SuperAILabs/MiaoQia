// Package domain 是 customer-context 的业务规则：档案校验、快照合成与目标配置。只依赖标准库（archtest R5）。
package domain

import (
	"strconv"
	"strings"
)

// CurrentStage 是当前交付阶段：覆盖矩阵中 release.stage 不晚于本阶段的目标才可启用（PRD 第2段 F2）。
const CurrentStage = 2

// Catalog 是已安装扩展包中本子系统使用的定义：APP-TENDER 的角色模板、目标实例与策略模板参数，
// 行业包的品类分类与资质模板。由 infra 从 packs/ 读取。
type Catalog struct {
	Roles           map[string]Role
	Objectives      []ObjectiveDef // 按标识排序
	PolicyTemplates map[string]PolicyTemplate
	Industries      map[string]Industry
}

// Role 是价值链角色模板。
type Role struct {
	ID, Name          string
	Default, Optional []string
}

// ObjectiveDef 是目标实例。Stage 为覆盖矩阵中的开放阶段，例如 M2。
type ObjectiveDef struct {
	ID, Version, Name, Say, Stage string
	PolicyTemplates               []string
	RequiresDomains               []string
}

// PolicyTemplate 是策略模板中目标配置用到的部分。
type PolicyTemplate struct {
	ID, Version string
	Params      []ParamDef // 按名称排序
}

// ParamDef 是策略模板参数定义。
type ParamDef struct {
	Name, Type, DefaultFrom string
	HasDefault              bool     // 模板声明了 default（任意类型）
	DefaultNumber           *int64   // 整数默认值
	DefaultValues           []string // 字符串列表默认值
	Min, Max                *float64
}

// Configurable 表示本切片可把该参数改为本目标值：整数与品类、区域列表。
func (p ParamDef) Configurable() bool {
	switch p.Type {
	case "int", "list<category_code>", "list<region_code>":
		return true
	}
	return false
}

// Industry 是行业包：品类代码集合与资质模板。
type Industry struct {
	ID             string
	Categories     map[string]bool
	Qualifications map[string]QualificationTemplate
}

// QualificationTemplate 是资质模板中档案录入用到的部分。
type QualificationTemplate struct {
	Code        string
	Grades      []string
	HasValidity bool
}

// StageNumber 把 M2 解析为 2；无法解析时返回 -1（视为未开放）。
func StageNumber(stage string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(stage, "M"))
	if err != nil || !strings.HasPrefix(stage, "M") {
		return -1
	}
	return n
}

// Objective 返回目标实例定义。
func (c *Catalog) Objective(id string) (ObjectiveDef, bool) {
	for _, o := range c.Objectives {
		if o.ID == id {
			return o, true
		}
	}
	return ObjectiveDef{}, false
}

// qualification 在所选行业包中查找资质模板。
func (c *Catalog) qualification(industries []string, code string) (QualificationTemplate, bool) {
	for _, id := range industries {
		if q, ok := c.Industries[id].Qualifications[code]; ok {
			return q, true
		}
	}
	return QualificationTemplate{}, false
}

// category 判断品类代码是否属于所选行业包的分类。
func (c *Catalog) category(industries []string, code string) bool {
	for _, id := range industries {
		if c.Industries[id].Categories[code] {
			return true
		}
	}
	return false
}
