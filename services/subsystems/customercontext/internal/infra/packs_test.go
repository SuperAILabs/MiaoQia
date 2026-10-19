package infra

import (
	"path/filepath"
	"testing"

	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// 读取仓库中的 APP-TENDER 与建材管材行业包：目录、模板参数与资质模板齐全，A02 对 MFR 可启用。
func TestLoadCatalogFromRepositoryPacks(t *testing.T) {
	c, err := LoadCatalog(filepath.Join("..", "..", "..", "..", "..", "packs"))
	if err != nil {
		t.Fatal(err)
	}
	a02, ok := c.Objective("APP-TENDER.A02")
	if !ok || a02.Stage != "M2" || len(a02.PolicyTemplates) != 1 {
		t.Fatalf("A02：%+v", a02)
	}
	tpl := c.PolicyTemplates[a02.PolicyTemplates[0]]
	names := map[string]domain.ParamDef{}
	for _, p := range tpl.Params {
		names[p.Name] = p
	}
	if names["categories"].DefaultFrom != "profile.offering_categories" || names["large_amount_cny"].DefaultNumber == nil || *names["large_amount_cny"].DefaultNumber != 5000000 {
		t.Fatalf("A02 参数：%+v", tpl.Params)
	}
	for _, role := range []string{"ROLE-MFR", "ROLE-DEALER", "ROLE-BIDDER", "ROLE-PRIME"} {
		if _, ok := c.Roles[role]; !ok {
			t.Errorf("缺少角色模板 %s", role)
		}
	}
	bm, ok := c.Industries["IND-BUILDING-MATERIALS"]
	if !ok || !bm.Categories["CAT-PIPE-PE-GAS"] || !bm.Qualifications["Q-BM-ISO9001"].HasValidity || len(bm.Qualifications["Q-BM-MUNICIPAL-GC"].Grades) == 0 {
		t.Fatalf("建材管材行业包：%+v", bm)
	}
	enableable := map[string]bool{}
	for _, v := range domain.Templates(c, []string{"ROLE-MFR"}) {
		enableable[v.Objective.ID] = v.Enableable
	}
	for id, want := range map[string]bool{"APP-TENDER.A01": true, "APP-TENDER.A02": true, "APP-TENDER.B03": true, "APP-TENDER.C01": false, "APP-TENDER.A04": false} {
		if enableable[id] != want {
			t.Errorf("MFR %s 可启用应为 %v", id, want)
		}
	}
}

func TestLoadCatalogRequiresPacks(t *testing.T) {
	if _, err := LoadCatalog(t.TempDir()); err == nil {
		t.Fatal("空目录应报错")
	}
}
