package domain

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func catalog() *Catalog {
	n0, n5m := int64(0), int64(5000000)
	zero := 0.0
	return &Catalog{
		Roles: map[string]Role{
			"ROLE-MFR":    {ID: "ROLE-MFR", Default: []string{"APP-TENDER.A01", "APP-TENDER.A02", "APP-TENDER.B03", "APP-TENDER.C01"}, Optional: []string{"APP-TENDER.A04"}},
			"ROLE-DEALER": {ID: "ROLE-DEALER", Default: []string{"APP-TENDER.A01", "APP-TENDER.A02", "APP-TENDER.C01"}},
		},
		Objectives: []ObjectiveDef{
			{ID: "APP-TENDER.A01", Version: "0.1.0", Stage: "M2", PolicyTemplates: []string{"TPL-A01"}},
			{ID: "APP-TENDER.A02", Version: "0.1.0", Stage: "M2", PolicyTemplates: []string{"TPL-A02"}},
			{ID: "APP-TENDER.A04", Version: "0.1.0", Stage: "M4"},
			{ID: "APP-TENDER.B03", Version: "0.1.0", Stage: "M2", PolicyTemplates: []string{"TPL-B03"}},
			{ID: "APP-TENDER.C01", Version: "0.1.0", Stage: "M3", PolicyTemplates: []string{"TPL-C01"}},
			{ID: "APP-TENDER.X01", Version: "0.1.0", Stage: "M2"},
		},
		PolicyTemplates: map[string]PolicyTemplate{
			"TPL-A01": {ID: "TPL-A01", Version: "0.1.0"},
			"TPL-A02": {ID: "TPL-A02", Version: "0.3.0", Params: []ParamDef{
				{Name: "categories", Type: "list<category_code>", DefaultFrom: "profile.offering_categories"},
				{Name: "large_amount_cny", Type: "int", HasDefault: true, DefaultNumber: &n5m, Min: &zero},
				{Name: "min_amount_cny", Type: "int", HasDefault: true, DefaultNumber: &n0, Min: &zero},
				{Name: "minor_change_types", Type: "list<enum>", HasDefault: true, DefaultValues: []string{"address"}},
				{Name: "regions", Type: "list<region_code>", DefaultFrom: "profile.service_regions"},
			}},
			"TPL-B03": {ID: "TPL-B03", Version: "0.1.0"},
			"TPL-C01": {ID: "TPL-C01", Version: "0.1.0"},
		},
		Industries: map[string]Industry{"IND-BM": {ID: "IND-BM",
			Categories: map[string]bool{"CAT-PIPE": true, "CAT-PIPE-PE": true, "CAT-PIPE-PE-GAS": true, "CAT-PIPE-PVC": true},
			Qualifications: map[string]QualificationTemplate{
				"Q-ISO9001": {Code: "Q-ISO9001", HasValidity: true},
				"Q-GC":      {Code: "Q-GC", Grades: []string{"一级", "二级"}, HasValidity: true},
				"Q-MEMBER":  {Code: "Q-MEMBER"},
			}}},
	}
}

func org(n int) string { return fmt.Sprintf("org_01JA8ZXXXXXXXXXXXXXXXXXX%02d", n) }

func valid() Profile {
	p := Empty()
	p.Entities = []Entity{{OrgID: org(1)}, {OrgID: org(2), OrgUnitID: RootUnit, QualificationsComplete: true}}
	p.Roles = []string{"ROLE-MFR"}
	p.Industries = []string{"IND-BM"}
	p.Categories = []string{"CAT-PIPE-PE"}
	p.Brands = []string{"示例"}
	p.Regions = []string{"330000"}
	p.Qualifications = []Qualification{
		{OrgID: org(1), Code: "Q-ISO9001", CertificateNo: "A1", ValidTo: "2027-06-30"},
		{OrgID: org(2), Code: "Q-GC", Grade: "一级", CertificateNo: "B1", ValidTo: "2026-10-18"},
	}
	return p
}

func code(err error) string {
	var bad *Invalid
	if errors.As(err, &bad) {
		return bad.Code
	}
	return ""
}

func TestProfileValidate(t *testing.T) {
	c := catalog()
	if err := valid().Validate(c); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, want string
		mutate     func(*Profile)
	}{
		{"个人主体", "unsupported_subject_type", func(p *Profile) { p.SubjectType = "person" }},
		{"主体不是组织", "invalid_entity", func(p *Profile) { p.Entities[0].OrgID = "tnt_01JA8ZXXXXXXXXXXXXXXXXXX01" }},
		{"主体重复", "duplicate_entity", func(p *Profile) { p.Entities[1].OrgID = org(1) }},
		{"未登记单元", "org_unit_not_found", func(p *Profile) { p.Entities[0].OrgUnitID = "unit-gas" }},
		{"未知角色", "unknown_role", func(p *Profile) { p.Roles = []string{"ROLE-INVESTOR"} }},
		{"未开放行业包", "industry_not_available", func(p *Profile) { p.Industries = []string{"IND-IT"} }},
		{"品类不属于所选行业包", "unknown_category", func(p *Profile) { p.Categories = []string{"CAT-VALVE"} }},
		{"未选行业包不能选品类", "unknown_category", func(p *Profile) { p.Industries = []string{}; p.Qualifications = nil }},
		{"区划代码无效", "invalid_region", func(p *Profile) { p.Regions = []string{"330012"[:4] + "AB"} }},
		{"市级为零县级非零", "invalid_region", func(p *Profile) { p.Regions = []string{"330001"} }},
		{"品牌首尾空白", "invalid_brand", func(p *Profile) { p.Brands = []string{" 示例"} }},
		{"品类重复", "duplicate_value", func(p *Profile) { p.Categories = []string{"CAT-PIPE", "CAT-PIPE"} }},
		{"资质属于未关联主体", "qualification_entity_unknown", func(p *Profile) { p.Qualifications[0].OrgID = org(9) }},
		{"资质类型不在模板", "unknown_qualification", func(p *Profile) { p.Qualifications[0].Code = "Q-OTHER" }},
		{"等级不在模板", "invalid_grade", func(p *Profile) { p.Qualifications[1].Grade = "特级" }},
		{"有效期必填", "validity_required", func(p *Profile) { p.Qualifications[0].ValidTo = "" }},
		{"日期无效", "invalid_validity", func(p *Profile) { p.Qualifications[0].ValidTo = "2027-02-30" }},
		{"起始日晚于有效期", "invalid_validity", func(p *Profile) { p.Qualifications[0].ValidFrom = "2028-01-01" }},
		{"同一证书重复", "duplicate_qualification", func(p *Profile) { p.Qualifications = append(p.Qualifications, p.Qualifications[0]) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid()
			tc.mutate(&p)
			if got := code(p.Validate(c)); got != tc.want {
				t.Fatalf("得到 %q，期望 %q", got, tc.want)
			}
		})
	}
	t.Run("无有效期的资质不必填写有效期，不同证书编号是两条资质", func(t *testing.T) {
		p := valid()
		p.Qualifications = append(p.Qualifications, Qualification{OrgID: org(1), Code: "Q-MEMBER", CertificateNo: "M1"},
			Qualification{OrgID: org(1), Code: "Q-ISO9001", CertificateNo: "A2", ValidTo: "2025-01-01"})
		if err := p.Validate(c); err != nil {
			t.Fatal(err)
		}
	})
}

// PRD 第2段用例 2、4 与“求值输入与未知传播”：快照对未知如实表达，不当作满足。
func TestCompose(t *testing.T) {
	t.Run("未关联本企业时不出现我方主体与状态表", func(t *testing.T) {
		p := valid()
		p.Entities, p.Qualifications = []Entity{}, []Qualification{}
		v, warn := Compose(p, "2026-10-19")
		if v.OrgIDs != nil || v.Entities != nil || warn {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("主体超过上界时省略而不截断并告警", func(t *testing.T) {
		p := valid()
		p.Entities = nil
		for i := range MaxSnapshotEntities + 1 {
			p.Entities = append(p.Entities, Entity{OrgID: org(i + 1)})
		}
		v, warn := Compose(p, "2026-10-19")
		if v.OrgIDs != nil || v.Entities != nil || !warn {
			t.Fatalf("%+v %v", v, warn)
		}
	})
	t.Run("按日期判定有效与过期，未录入不出现", func(t *testing.T) {
		v, _ := Compose(valid(), "2026-10-19")
		if !slices.Equal(v.OrgIDs, []string{org(1), org(2)}) {
			t.Fatalf("org_ids %v", v.OrgIDs)
		}
		e1, e2 := v.Entities[org(1)], v.Entities[org(2)]
		if e1.Qualifications["Q-ISO9001"] != QualValid || e1.Complete || len(e1.Pending) != 0 {
			t.Fatalf("%+v", e1)
		}
		if e2.Qualifications["Q-GC"] != QualExpired || !e2.Complete {
			t.Fatalf("%+v", e2)
		}
		if _, ok := e1.Qualifications["Q-GC"]; ok {
			t.Fatal("未录入的资质不得出现在状态表中")
		}
	})
	t.Run("有效期最后一天当日仍有效", func(t *testing.T) {
		v, _ := Compose(valid(), "2026-10-18")
		if v.Entities[org(2)].Qualifications["Q-GC"] != QualValid {
			t.Fatal("到期日当日应有效")
		}
	})
	t.Run("没有资质记录的主体给空表", func(t *testing.T) {
		p := valid()
		p.Qualifications = nil
		v, _ := Compose(p, "2026-10-19")
		if e, ok := v.Entities[org(1)]; !ok || e.Qualifications == nil || len(e.Qualifications) != 0 || e.Pending == nil {
			t.Fatalf("%+v", v.Entities)
		}
	})
	t.Run("同一代码任一记录有效即有效，尚未生效的记录不计入", func(t *testing.T) {
		p := valid()
		p.Qualifications = []Qualification{
			{OrgID: org(1), Code: "Q-ISO9001", CertificateNo: "old", ValidTo: "2025-01-01"},
			{OrgID: org(1), Code: "Q-ISO9001", CertificateNo: "new", ValidTo: "2028-01-01"},
			{OrgID: org(2), Code: "Q-GC", CertificateNo: "future", ValidFrom: "2027-01-01", ValidTo: "2030-01-01"},
		}
		v, _ := Compose(p, "2026-10-19")
		if v.Entities[org(1)].Qualifications["Q-ISO9001"] != QualValid {
			t.Fatal("有效记录应优先")
		}
		if _, ok := v.Entities[org(2)].Qualifications["Q-GC"]; ok {
			t.Fatal("尚未生效的记录不应计入")
		}
	})
	t.Run("集合字段排序，结果与输入顺序无关", func(t *testing.T) {
		p := valid()
		p.Categories = []string{"CAT-PIPE-PVC", "CAT-PIPE-PE"}
		q := valid()
		q.Categories = []string{"CAT-PIPE-PE", "CAT-PIPE-PVC"}
		a, _ := Compose(p, "2026-10-19")
		b, _ := Compose(q, "2026-10-19")
		if !SameView(a, b) {
			t.Fatal("合成结果应与顺序无关")
		}
	})
}

func TestRefreshAfter(t *testing.T) {
	p := valid()
	if got := RefreshAfter(p, "2026-10-17"); got != "2026-10-19" {
		t.Fatalf("得到 %s", got)
	}
	if got := RefreshAfter(p, "2026-10-19"); got != "2027-07-01" {
		t.Fatalf("得到 %s", got)
	}
	p.Qualifications = append(p.Qualifications, Qualification{OrgID: org(1), Code: "Q-ISO9001", CertificateNo: "f", ValidFrom: "2027-03-01", ValidTo: "2029-01-01"})
	if got := RefreshAfter(p, "2026-10-19"); got != "2027-03-01" {
		t.Fatalf("得到 %s", got)
	}
	if got := RefreshAfter(Empty(), "2026-10-19"); got != "" {
		t.Fatalf("得到 %s", got)
	}
}

func TestTemplates(t *testing.T) {
	c := catalog()
	byID := func(views []TemplateView) map[string]TemplateView {
		out := map[string]TemplateView{}
		for _, v := range views {
			out[v.Objective.ID] = v
		}
		return out
	}
	mfr := byID(Templates(c, []string{"ROLE-MFR"}))
	for id, want := range map[string]string{"APP-TENDER.A01": "", "APP-TENDER.A02": "", "APP-TENDER.B03": "",
		"APP-TENDER.C01": NotReleased, "APP-TENDER.A04": NotReleased, "APP-TENDER.X01": NoPolicyTemplate} {
		if v := mfr[id]; v.UnavailableReason != want || v.Enableable != (want == "") {
			t.Errorf("%s：%+v，期望原因 %q", id, v, want)
		}
	}
	if !slices.Equal(mfr["APP-TENDER.A02"].DefaultForRoles, []string{"ROLE-MFR"}) {
		t.Error("A02 应为 MFR 的默认目标")
	}
	dealer := byID(Templates(c, []string{"ROLE-DEALER"}))
	if v := dealer["APP-TENDER.B03"]; v.Enableable || v.UnavailableReason != RoleNotSelected {
		t.Errorf("经销商不应能启用 B03：%+v", v)
	}
	none := byID(Templates(c, nil))
	if v := none["APP-TENDER.A02"]; v.Enableable || v.UnavailableReason != RoleNotSelected {
		t.Errorf("未选角色时不可启用：%+v", v)
	}
	if len(Templates(c, nil)) != len(c.Objectives) {
		t.Error("目录应完整列出全部目标")
	}
}

func TestResolveParams(t *testing.T) {
	c := catalog()
	tpl := c.PolicyTemplates["TPL-A02"]
	p := valid()
	n := func(v int64) *int64 { return &v }
	got, err := ResolveParams(c, tpl, nil, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["categories"].Source != FromProfile || got["min_amount_cny"].Source != FromDefault || got["minor_change_types"].Source != FromDefault {
		t.Fatalf("默认补全：%+v", got)
	}
	ok := []map[string]Param{
		{"categories": {Source: FromObjective, Values: []string{"CAT-PIPE-PE-GAS"}}},
		{"regions": {Source: FromObjective, Values: []string{"330100", "330106"}}},
		{"min_amount_cny": {Source: FromObjective, Number: n(1000000)}},
	}
	for _, in := range ok {
		if _, err := ResolveParams(c, tpl, in, p, nil); err != nil {
			t.Errorf("%+v：%v", in, err)
		}
	}
	bad := []struct {
		in   map[string]Param
		want string
	}{
		{map[string]Param{"unknown": {Source: FromDefault}}, "unknown_param"},
		{map[string]Param{"categories": {Source: FromObjective, Values: []string{"CAT-PIPE-PVC"}}}, "param_not_subset"},
		{map[string]Param{"categories": {Source: FromObjective, Values: []string{"CAT-PIPE-PE-XYZ"}}}, "param_not_subset"},
		{map[string]Param{"regions": {Source: FromObjective, Values: []string{"320000"}}}, "param_not_subset"},
		{map[string]Param{"min_amount_cny": {Source: FromObjective, Number: n(-1)}}, "invalid_param"},
		{map[string]Param{"min_amount_cny": {Source: FromProfile}}, "invalid_param"},
		{map[string]Param{"categories": {Source: FromDefault}}, "invalid_param"},
		{map[string]Param{"minor_change_types": {Source: FromObjective, Values: []string{"name"}}}, "param_not_configurable"},
		{map[string]Param{"categories": {Source: FromObjective}}, "invalid_param"},
		{map[string]Param{"categories": {Source: "inherit"}}, "invalid_param"},
	}
	for _, tc := range bad {
		if _, err := ResolveParams(c, tpl, tc.in, p, nil); code(err) != tc.want {
			t.Errorf("%+v：得到 %v，期望 %s", tc.in, err, tc.want)
		}
	}
	t.Run("未修改的本目标值不随档案缩小而失效", func(t *testing.T) {
		keep := map[string]Param{"categories": {Source: FromObjective, Values: []string{"CAT-PIPE-PE-GAS"}}}
		shrunk := valid()
		shrunk.Categories = []string{"CAT-PIPE-PVC"}
		if _, err := ResolveParams(c, tpl, keep, shrunk, keep); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveParams(c, tpl, keep, shrunk, nil); code(err) != "param_not_subset" {
			t.Fatalf("新设置的本目标值须在档案范围内：%v", err)
		}
	})
}

func TestTransition(t *testing.T) {
	for _, tc := range []struct{ from, to, change, err string }{
		{StateInactive, StateDisabled, "disabled", ""},
		{StateActive, StateDisabled, "disabled", ""},
		{StateDisabled, StateInactive, "reenabled", ""},
		{StateDisabled, StateDisabled, "", ""},
		{StateActive, StateInactive, "", "invalid_state_transition"},
		{StateInactive, StateActive, "", "invalid_state_transition"},
	} {
		change, err := Transition(tc.from, tc.to)
		if change != tc.change || code(err) != tc.err {
			t.Errorf("%s→%s：%q %v", tc.from, tc.to, change, err)
		}
	}
}

func TestRegions(t *testing.T) {
	for _, tc := range []struct {
		parent, child string
		want          bool
	}{{"330000", "330106", true}, {"330100", "330106", true}, {"330100", "330200", false}, {"330106", "330106", true}, {"330106", "330100", false}, {"330000", "320000", false}} {
		if RegionCovers(tc.parent, tc.child) != tc.want {
			t.Errorf("%s ⊇ %s 应为 %v", tc.parent, tc.child, tc.want)
		}
	}
}
