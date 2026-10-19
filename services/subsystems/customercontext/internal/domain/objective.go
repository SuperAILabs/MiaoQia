package domain

import (
	"reflect"
	"strings"
)

// 目标状态（PRD 第2段第6节）：未激活 → 首次激活（第7段）→ 生效 → 停用 → 重新启用 → 未激活。
const (
	StateInactive = "inactive"
	StateActive   = "active"
	StateDisabled = "disabled"
)

// 不可启用原因。
const (
	NotReleased      = "not_released"
	NoPolicyTemplate = "no_policy_template"
	RoleNotSelected  = "role_not_selected"
)

// TemplateView 是目标目录中的一项。
type TemplateView struct {
	Objective         ObjectiveDef
	Policy            *PolicyTemplate
	DefaultForRoles   []string
	Enableable        bool
	UnavailableReason string
}

// Templates 按价值链角色列出完整目标目录（PRD 第2段 F2）：所选角色的默认与可选目标的并集可启用；
// 未到开放阶段、没有策略模板或不属于所选角色的目标照常列出并给出原因。
func Templates(c *Catalog, roles []string) []TemplateView {
	out := make([]TemplateView, 0, len(c.Objectives))
	for _, o := range c.Objectives {
		v := TemplateView{Objective: o, DefaultForRoles: []string{}}
		listed := false
		for _, r := range roles {
			role := c.Roles[r]
			if contains(role.Default, o.ID) {
				v.DefaultForRoles = append(v.DefaultForRoles, r)
				listed = true
			}
			if contains(role.Optional, o.ID) {
				listed = true
			}
		}
		if len(o.PolicyTemplates) > 0 {
			if t, ok := c.PolicyTemplates[o.PolicyTemplates[0]]; ok {
				v.Policy = &t
			}
		}
		switch n := StageNumber(o.Stage); {
		case n < 0 || n > CurrentStage:
			v.UnavailableReason = NotReleased
		case v.Policy == nil:
			v.UnavailableReason = NoPolicyTemplate
		case !listed:
			v.UnavailableReason = RoleNotSelected
		default:
			v.Enableable = true
		}
		out = append(out, v)
	}
	return out
}

// Param 是目标的一个参数取值。Source 为 profile（随档案）、default（模板默认值）或 objective（本目标值）。
type Param struct {
	Source string   `json:"source"`
	Values []string `json:"values,omitempty"`
	Number *int64   `json:"number,omitempty"`
}

// 参数来源。
const (
	FromProfile   = "profile"
	FromDefault   = "default"
	FromObjective = "objective"
)

// ResolveParams 校验并补全目标参数（PRD 第2段 F5、第13节第2项）：未给出的参数随档案（有 default_from 时）或取模板默认值；
// 本目标值的品类与区域须是当前档案取值或其下级，整数须满足模板 min/max。keep 中与输入相同的参数视为未修改，
// 不再按当前档案重新校验（改为本目标值后不再随档案变化）。
func ResolveParams(c *Catalog, tpl PolicyTemplate, in map[string]Param, profile Profile, keep map[string]Param) (map[string]Param, error) {
	defs := map[string]ParamDef{}
	for _, d := range tpl.Params {
		defs[d.Name] = d
	}
	for name := range in {
		if _, ok := defs[name]; !ok {
			return nil, invalid("unknown_param", "模板 %s 没有参数 %s", tpl.ID, name)
		}
	}
	out := map[string]Param{}
	for _, d := range tpl.Params {
		p, given := in[d.Name]
		if !given {
			if d.DefaultFrom != "" {
				p = Param{Source: FromProfile}
			} else {
				p = Param{Source: FromDefault}
			}
		}
		if old, ok := keep[d.Name]; ok && reflect.DeepEqual(old, p) {
			out[d.Name] = p
			continue
		}
		if err := checkParam(c, d, p, profile); err != nil {
			return nil, err
		}
		out[d.Name] = p
	}
	return out, nil
}

func checkParam(c *Catalog, d ParamDef, p Param, profile Profile) error {
	switch p.Source {
	case FromProfile:
		if d.DefaultFrom == "" || p.Values != nil || p.Number != nil {
			return invalid("invalid_param", "参数 %s 不能随档案", d.Name)
		}
	case FromDefault:
		if !d.HasDefault || p.Values != nil || p.Number != nil {
			return invalid("invalid_param", "参数 %s 没有模板默认值", d.Name)
		}
	case FromObjective:
		if !d.Configurable() {
			return invalid("param_not_configurable", "参数 %s（%s）在本阶段只能取模板默认值", d.Name, d.Type)
		}
		if d.Type == "int" {
			if p.Number == nil || p.Values != nil {
				return invalid("invalid_param", "参数 %s 须为整数", d.Name)
			}
			n := float64(*p.Number)
			if (d.Min != nil && n < *d.Min) || (d.Max != nil && n > *d.Max) {
				return invalid("invalid_param", "参数 %s 超出模板范围", d.Name)
			}
			return nil
		}
		if len(p.Values) == 0 || p.Number != nil {
			return invalid("invalid_param", "参数 %s 须为非空列表", d.Name)
		}
		if err := unique(d.Name, p.Values); err != nil {
			return err
		}
		for _, v := range p.Values {
			var ok bool
			if d.Type == "list<category_code>" {
				ok = c.category(profile.Industries, v) && coveredBy(v, profile.Categories, categoryCovers)
			} else {
				ok = ValidRegion(v) && coveredBy(v, profile.Regions, RegionCovers)
			}
			if !ok {
				return invalid("param_not_subset", "参数 %s 的取值 %s 不在档案范围内", d.Name, v)
			}
		}
	default:
		return invalid("invalid_param", "参数 %s 的来源 %s 无效", d.Name, p.Source)
	}
	return nil
}

func coveredBy(v string, set []string, covers func(parent, child string) bool) bool {
	for _, s := range set {
		if covers(s, v) {
			return true
		}
	}
	return false
}

// categoryCovers：品类代码按“-”分级，上级包含全部下级（category.any_match 的语义）。
func categoryCovers(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, parent+"-")
}

// RegionCovers：GB/T 2260 区划代码上级包含全部下级（全国 000000 含全部，省 XX0000、市 XXXX00），
// 与策略求值 region.within（kernel/policy）的语义一致。两个参数都须是 ValidRegion 通过的代码。
func RegionCovers(parent, child string) bool {
	switch {
	case parent == child, parent == NationalRegion:
		return true
	case child == NationalRegion:
		return false
	case strings.HasSuffix(parent, "0000"):
		return parent[:2] == child[:2]
	case strings.HasSuffix(parent, "00"):
		return parent[:4] == child[:4]
	}
	return false
}

// Transition 返回目标状态变化及事件 change；目标状态与当前相同时 change 为空（无变化）。
// 生效只由策略激活设置（第7段）；重新启用只能从已停用开始，重新启用后须重新激活。
func Transition(current, target string) (string, error) {
	switch {
	case current == target:
		return "", nil
	case target == StateDisabled:
		return "disabled", nil
	case target == StateInactive && current == StateDisabled:
		return "reenabled", nil
	}
	return "", invalid("invalid_state_transition", "目标不能从 %s 变为 %s", current, target)
}
