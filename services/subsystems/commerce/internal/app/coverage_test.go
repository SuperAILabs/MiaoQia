package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/api"
)

func TestParseCoverage(t *testing.T) {
	plan, err := ParseCoverage(" INF-PROC=330000, 310100 ,110105; INF-ORG=* ")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan["INF-PROC"], []string{"33", "3101", "110105"}) || !slices.Equal(plan["INF-ORG"], []string{"*"}) || len(plan) != 2 {
		t.Fatalf("%v", plan)
	}
	if plan, err := ParseCoverage("  "); plan != nil || err != nil {
		t.Fatalf("未配置应为 nil：%v %v", plan, err)
	}
	for _, bad := range []string{
		"INF-PROC",                   // 缺少区划
		"INF-PROC=",                  // 空区划
		"INF-PROC=33",                // 不是六位代码
		"INF-PROC=000000",            // 全零
		"INF-PROC=001100",            // 省级代码不能以 00 开头
		"INF-PROC=*,330000",          // * 与具体区划混用
		"inf-proc=*",                 // 信息领域写法
		"INF-PROC=*;INF-PROC=330000", // 重复
		"INF-PROC=330000;;INF-ORG=*", // 空条目
		"INF-PROC=330000;tenant=t1",  // 不接受其他键
	} {
		if _, err := ParseCoverage(bad); err == nil {
			t.Errorf("%q 应拒绝", bad)
		}
	}
}

func TestCoverageDecide(t *testing.T) {
	plan, err := ParseCoverage("INF-PROC=330000,440305;INF-ORG=*")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		domain, region string
		covered        bool
		missing        string
	}{
		{"INF-PROC", "330106", true, ""},             // 省级包含县级
		{"INF-PROC", "330000", true, ""},             // 省本身
		{"INF-PROC", "440305", true, ""},             // 县级精确
		{"INF-PROC", "440306", false, "region"},      // 同市其他县不在县级覆盖内
		{"INF-PROC", "340106", false, "region"},      // 其他省
		{"INF-PROC", "", false, "region"},            // 区域未知不推定覆盖
		{"INF-ORG", "", true, ""},                    // 全部区域
		{"INF-CRED", "330106", false, "info_domain"}, // 信息领域不在包内
	} {
		d := plan.Decide(c.domain, c.region)
		if d.Covered != c.covered || (c.missing == "" && len(d.Missing) != 0) || (c.missing != "" && !slices.Equal(d.Missing, []string{c.missing})) {
			t.Errorf("%s/%s：%+v", c.domain, c.region, d)
		}
	}
}

func TestCheckCoverageRequiresTenantUserAndConfiguredPlan(t *testing.T) {
	plan, _ := ParseCoverage("INF-PROC=*")
	svc := &Service{Coverage: plan}
	user := reqctx.Context{Principal: reqctx.Principal{ID: "usr_1", Kind: "user"}, Tenant: "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1", Cell: "cell-01"}
	ctx := reqctx.With(context.Background(), user)
	if d, err := svc.CheckCoverage(ctx, "INF-PROC", "330106"); err != nil || !d.Covered {
		t.Fatalf("%+v %v", d, err)
	}
	code := func(err error) string {
		var e *api.Error
		if errors.As(err, &e) {
			return e.Code
		}
		return err.Error()
	}
	// 越权与反例：缺少上下文、无租户、服务主体、非法参数与未配置覆盖包均不给出判断
	if _, err := svc.CheckCoverage(context.Background(), "INF-PROC", ""); !errors.Is(err, reqctx.ErrMissing) {
		t.Fatalf("缺少上下文：%v", err)
	}
	noTenant := user
	noTenant.Tenant, noTenant.Cell = "", ""
	if _, err := svc.CheckCoverage(reqctx.With(context.Background(), noTenant), "INF-PROC", ""); code(err) != "invalid_tenant_context" {
		t.Fatalf("无租户：%v", err)
	}
	service := user
	service.Principal = reqctx.Principal{ID: "svc:gateway", Kind: "service"}
	if _, err := svc.CheckCoverage(reqctx.With(context.Background(), service), "INF-PROC", ""); code(err) != "forbidden" {
		t.Fatalf("服务主体：%v", err)
	}
	for _, args := range [][2]string{{"INF-proc", ""}, {"INF-PROC", "33"}, {"", ""}, {"INF-PROC", "3301061"}} {
		if _, err := svc.CheckCoverage(ctx, args[0], args[1]); code(err) != "invalid_request" {
			t.Fatalf("%v：%v", args, err)
		}
	}
	if _, err := (&Service{}).CheckCoverage(ctx, "INF-PROC", "330106"); code(err) != "coverage_unavailable" {
		t.Fatalf("未配置：%v", err)
	}
}
