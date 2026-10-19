package domain

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// RootUnit 是租户根组织单元（与 api.RootUnit 相同；domain 层不依赖 api 包）。
const RootUnit = "root"

// Date 是业务日历日 YYYY-MM-DD（Asia/Shanghai），按字典序比较。
type Date string

var dateLayout = "2006-01-02"

// Valid 判断是否为合法日期。
func (d Date) Valid() bool {
	t, err := time.Parse(dateLayout, string(d))
	return err == nil && t.Format(dateLayout) == string(d)
}

// AddDays 返回 d 之后 n 天。
func (d Date) AddDays(n int) Date {
	t, _ := time.Parse(dateLayout, string(d))
	return Date(t.AddDate(0, 0, n).Format(dateLayout))
}

// Shanghai 是资质有效期与快照判定使用的业务时区。
var Shanghai = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// Today 返回 t 在业务时区的日历日。
func Today(t time.Time) Date { return Date(t.In(Shanghai).Format(dateLayout)) }

// Profile 是一个档案版本的内容（ProfileVersion）。M2 只支持企业主体。
type Profile struct {
	SubjectType    string          `json:"subject_type"`
	Entities       []Entity        `json:"entities"`
	Roles          []string        `json:"value_chain_roles"`
	Industries     []string        `json:"industry_packs"`
	Categories     []string        `json:"offering_categories"`
	Brands         []string        `json:"brands"`
	Regions        []string        `json:"service_regions"`
	Qualifications []Qualification `json:"qualifications"`
}

// Entity 是我方主体。OrgUnitID 为空表示未指定所属单元。
type Entity struct {
	OrgID                  string `json:"org_id"`
	OrgUnitID              string `json:"org_unit_id,omitempty"`
	QualificationsComplete bool   `json:"qualifications_complete"`
}

// Qualification 是客户录入的一条资质来源观察。
type Qualification struct {
	OrgID         string `json:"org_id"`
	Code          string `json:"code"`
	Grade         string `json:"grade,omitempty"`
	CertificateNo string `json:"certificate_no"`
	ValidFrom     Date   `json:"valid_from,omitempty"`
	ValidTo       Date   `json:"valid_to,omitempty"`
}

// Empty 是从未保存时的档案。
func Empty() Profile {
	return Profile{SubjectType: "organization", Entities: []Entity{}, Roles: []string{}, Industries: []string{}, Categories: []string{},
		Brands: []string{}, Regions: []string{}, Qualifications: []Qualification{}}
}

// Invalid 是档案或目标配置不满足业务规则；Code 为稳定错误码。
type Invalid struct {
	Code, Detail string
}

func (e *Invalid) Error() string { return e.Code + "：" + e.Detail }

func invalid(code, format string, args ...any) error {
	return &Invalid{Code: code, Detail: fmt.Sprintf(format, args...)}
}

var (
	orgPattern    = regexp.MustCompile(`^org_[0-9A-HJKMNP-TV-Z]{26}$`)
	regionPattern = regexp.MustCompile(`^[0-9]{6}$`)
)

// ValidRegion 判断 GB/T 2260 行政区划代码的形式：六位数字，省级两位不为 00，市级为 00 时县级也为 00。
// 代码是否真实存在需要区划数据集，随区划代码表交付后校验（实现中发现的设计缺口）。
func ValidRegion(code string) bool {
	return regionPattern.MatchString(code) && code[:2] != "00" && (code[2:4] != "00" || code[4:] == "00")
}

// Validate 按已安装扩展包校验档案。行业包只能选择已安装的包（未交付的包“暂未开放”）；
// 品类与资质类型必须属于所选行业包；资质必须属于已关联的我方主体。
func (p Profile) Validate(c *Catalog) error {
	if p.SubjectType != "organization" {
		return invalid("unsupported_subject_type", "M2 档案只支持企业主体")
	}
	entities := map[string]bool{}
	for _, e := range p.Entities {
		if !orgPattern.MatchString(e.OrgID) {
			return invalid("invalid_entity", "%s 不是组织标识", e.OrgID)
		}
		if entities[e.OrgID] {
			return invalid("duplicate_entity", "我方主体 %s 重复", e.OrgID)
		}
		entities[e.OrgID] = true
		if e.OrgUnitID != "" && e.OrgUnitID != RootUnit {
			return invalid("org_unit_not_found", "组织单元 %s 不存在", e.OrgUnitID)
		}
	}
	if err := unique("value_chain_roles", p.Roles); err != nil {
		return err
	}
	for _, r := range p.Roles {
		if _, ok := c.Roles[r]; !ok {
			return invalid("unknown_role", "价值链角色 %s 未在应用领域包中定义", r)
		}
	}
	if err := unique("industry_packs", p.Industries); err != nil {
		return err
	}
	for _, id := range p.Industries {
		if _, ok := c.Industries[id]; !ok {
			return invalid("industry_not_available", "行业包 %s 暂未开放", id)
		}
	}
	if err := unique("offering_categories", p.Categories); err != nil {
		return err
	}
	for _, code := range p.Categories {
		if !c.category(p.Industries, code) {
			return invalid("unknown_category", "品类 %s 不属于所选行业包", code)
		}
	}
	if err := unique("brands", p.Brands); err != nil {
		return err
	}
	for _, b := range p.Brands {
		if strings.TrimSpace(b) != b || b == "" {
			return invalid("invalid_brand", "品牌 %q 首尾不能有空白", b)
		}
	}
	if err := unique("service_regions", p.Regions); err != nil {
		return err
	}
	for _, r := range p.Regions {
		if !ValidRegion(r) {
			return invalid("invalid_region", "行政区划代码 %s 无效", r)
		}
	}
	seen := map[[3]string]bool{}
	for _, q := range p.Qualifications {
		if !entities[q.OrgID] {
			return invalid("qualification_entity_unknown", "资质所属主体 %s 未关联", q.OrgID)
		}
		tpl, ok := c.qualification(p.Industries, q.Code)
		if !ok {
			return invalid("unknown_qualification", "资质类型 %s 不属于所选行业包的资质模板", q.Code)
		}
		if strings.TrimSpace(q.CertificateNo) != q.CertificateNo || q.CertificateNo == "" {
			return invalid("invalid_certificate_no", "证书编号首尾不能有空白")
		}
		if q.Grade != "" && len(tpl.Grades) > 0 && !contains(tpl.Grades, q.Grade) {
			return invalid("invalid_grade", "资质 %s 没有等级 %s", q.Code, q.Grade)
		}
		if tpl.HasValidity && q.ValidTo == "" {
			return invalid("validity_required", "资质 %s 须填写有效期", q.Code)
		}
		if (q.ValidFrom != "" && !q.ValidFrom.Valid()) || (q.ValidTo != "" && !q.ValidTo.Valid()) {
			return invalid("invalid_validity", "资质 %s 的日期无效", q.Code)
		}
		if q.ValidFrom != "" && q.ValidTo != "" && q.ValidFrom > q.ValidTo {
			return invalid("invalid_validity", "资质 %s 的起始日晚于有效期", q.Code)
		}
		key := [3]string{q.OrgID, q.Code, q.CertificateNo}
		if seen[key] {
			return invalid("duplicate_qualification", "主体 %s 的资质 %s 证书 %s 重复", q.OrgID, q.Code, q.CertificateNo)
		}
		seen[key] = true
	}
	return nil
}

func unique(field string, values []string) error {
	seen := map[string]bool{}
	for _, v := range values {
		if seen[v] {
			return invalid("duplicate_value", "%s 中 %s 重复", field, v)
		}
		seen[v] = true
	}
	return nil
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// Changed 返回与上一版本相比变化的档案部分（事件 changed 字段，只作提示）。
func Changed(prev, next Profile) []string {
	out := []string{}
	add := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, name)
		}
	}
	add("entities", prev.Entities, next.Entities)
	add("value_chain_roles", prev.Roles, next.Roles)
	add("industry_packs", prev.Industries, next.Industries)
	add("offering_categories", prev.Categories, next.Categories)
	add("brands", prev.Brands, next.Brands)
	add("service_regions", prev.Regions, next.Regions)
	add("qualifications", prev.Qualifications, next.Qualifications)
	return out
}
