package app

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// 真实的快照组装经 A02 模板求值（PRD 第2段用例 2、3、20）：快照的 policy_view 原样作为策略的 profile 变量。
func TestSnapshotDrivesA02Evaluation(t *testing.T) {
	pack, err := policy.LoadPack(filepath.Join("..", "..", "..", "..", "..", "packs", "application_domains", "tendering"))
	if err != nil {
		t.Fatal(err)
	}
	tpl := pack.Templates["TPL-TENDER-A02-SUPPLY"]
	compiled, issues := policy.Compile(tpl, pack.Features, policy.DefaultLimits())
	if issues != nil {
		t.Fatal(issues)
	}
	const me, me2, other = "org_01JA8ZXXXXXXXXXXXXXXXXXXX1", "org_01JA8ZXXXXXXXXXXXXXXXXXXX2", "org_01JA8ZXXXXXXXXXXXXXXXXXXX9"
	profileView := func(p domain.Profile) map[string]any {
		view, _ := domain.Compose(p, "2026-10-19")
		body, err := SnapshotOut(&SnapshotResult{Tenant: "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1", Unit: "root", Version: 1, Chain: []string{"root"}, View: view}).MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var snap map[string]any
		if err := json.Unmarshal(body, &snap); err != nil {
			t.Fatal(err)
		}
		return snap["policy_view"].(map[string]any)
	}
	evaluate := func(p domain.Profile, winner string, customers []any) *policy.Result {
		in, err := tpl.ExampleInput(tpl.Examples[0])
		if err != nil {
			t.Fatal(err)
		}
		in.Profile = profileView(p)
		in.Fact["winner"].(map[string]any)["org_id"] = winner
		in.Lists = map[string]any{"customers": customers, "blocked": []any{}, "competitors": []any{}}
		r, err := compiled.Evaluate(in)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	trace := func(r *policy.Result, id string) policy.RuleTrace {
		for _, tr := range r.Trace {
			if tr.ID == id {
				return tr
			}
		}
		t.Fatalf("轨迹中没有 %s", id)
		return policy.RuleTrace{}
	}
	base := domain.Empty()
	base.Categories, base.Regions = []string{"CAT-PIPE"}, []string{"330000"}
	linked := base
	linked.Entities = []domain.Entity{{OrgID: me}, {OrgID: me2}}

	t.Run("用例3：勾选上级品类即匹配下级品类", func(t *testing.T) {
		r := evaluate(linked, other, []any{other})
		if trace(r, "R-CAT").Result != policy.True || r.Disposition != policy.PursueCandidate {
			t.Fatalf("%s %+v", r.Disposition, r.Trace)
		}
		unrelated := linked
		unrelated.Categories = []string{"CAT-PIPE-PVC"}
		if r := evaluate(unrelated, other, []any{other}); trace(r, "R-CAT").Result != policy.False || r.Disposition != policy.NoSignal {
			t.Fatalf("其他品类不应匹配：%s", r.Disposition)
		}
	})
	t.Run("用例2：未关联本企业时 X-SELF 为未知、不排除，处置由其他规则决定", func(t *testing.T) {
		r := evaluate(base, me, []any{me})
		if x := trace(r, "X-SELF"); x.Result != policy.Unknown {
			t.Fatalf("X-SELF %s", x.Result)
		}
		if r.Disposition == policy.Excluded || r.Disposition != policy.PursueCandidate {
			t.Fatalf("未关联时不应排除：%s", r.Disposition)
		}
	})
	t.Run("区划包含与策略 region.within 一致（含全国 000000）", func(t *testing.T) {
		for _, region := range []string{"000000", "330000", "330100", "330106"} {
			for _, loc := range []string{"330106", "330100", "110105", "000000"} {
				p := linked
				p.Regions = []string{region}
				in, err := tpl.ExampleInput(tpl.Examples[0])
				if err != nil {
					t.Fatal(err)
				}
				in.Profile = profileView(p)
				fact := map[string]any{"location": loc} // 样例输入的映射在调用间共享，复制后修改
				for k, v := range in.Fact {
					if k != "location" {
						fact[k] = v
					}
				}
				in.Fact = fact
				r, err := compiled.Evaluate(in)
				if err != nil {
					t.Fatal(err)
				}
				want := policy.False
				if domain.RegionCovers(region, loc) {
					want = policy.True
				}
				if got := trace(r, "R-REGION").Result; got != want {
					t.Errorf("档案 %s、地点 %s：策略 %s，档案子集判断 %s", region, loc, got, want)
				}
			}
		}
	})
	t.Run("用例20：任一我方主体中标都被 X-SELF 排除", func(t *testing.T) {
		for _, winner := range []string{me, me2} {
			if r := evaluate(linked, winner, []any{winner}); r.Disposition != policy.Excluded || r.Reason != "SELF_IS_WINNER" {
				t.Fatalf("%s：%s %s", winner, r.Disposition, r.Reason)
			}
		}
	})
}
