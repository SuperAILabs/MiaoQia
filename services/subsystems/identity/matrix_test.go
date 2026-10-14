package identity_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/subsystems/identity/internal/infra"
)

// 矩阵的列（与 identity.md 第 7.2 节表头一致）与测试中代表该角色的用户。
var roleColumns = []struct{ title, relation, user string }{
	{"决策者", "decision_maker", "user:dm"},
	{"业务开拓者", "business_developer", "user:bd"},
	{"项目推进者", "pursuit_lead", "user:pl"},
	{"研究分析者", "research_analyst", "user:ra"},
	{"业务专家", "domain_expert", "user:de"},
	{"租户管理员", "admin", "user:ta"},
	{"外部协作者", "", "user:ex"},
}

type row struct {
	action, resource, relation string
	cells                      []string
}

// matrix 读取 docs/03-architecture/subsystems/identity.md 第 7.2 节的权限矩阵。
func matrix(t *testing.T) []row {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "03-architecture", "subsystems", "identity.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	i := strings.Index(text, "| 动作 | 资源 | 授权关系 |")
	if i < 0 {
		t.Fatal("identity.md 中找不到权限矩阵")
	}
	lines := strings.Split(text[i:], "\n")
	header := strings.Split(strings.Trim(lines[0], "| "), " | ")
	for k, c := range roleColumns {
		if header[3+k] != c.title {
			t.Fatalf("矩阵第 %d 列应为 %s，实际为 %s", 4+k, c.title, header[3+k])
		}
	}
	var rows []row
	for _, l := range lines[2:] {
		if !strings.HasPrefix(l, "| ") {
			break
		}
		f := strings.Split(strings.Trim(l, "| "), " | ")
		if len(f) != 3+len(roleColumns) {
			t.Fatalf("矩阵行列数不符：%s", l)
		}
		rows = append(rows, row{action: f[0], resource: f[1], relation: f[2], cells: f[3:]})
	}
	return rows
}

// 授权模型与权限矩阵逐格一致（真实 OpenFGA，每次使用独立的 store）。
func TestPermissionMatrix(t *testing.T) {
	url := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if url == "" {
		t.Skip("未设置 MIAOQIA_TEST_OPENFGA（例如 http://localhost:8081）")
	}
	ctx := context.Background()
	fga, err := infra.ConnectStore(ctx, url, fmt.Sprintf("matrix-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	// 租户 T 下的组织单元 U；角色绑定在租户上，经继承作用于 U 中的资源。
	// 每类资源两个实例：…1 上全部用户都有参与关系（◐ 的“本人参与”），…2 上没有。
	tuples := [][3]string{{"tenant:t", "parent", "org_unit:u"}}
	for _, c := range roleColumns {
		if c.relation != "" {
			tuples = append(tuples, [3]string{c.user, c.relation, "tenant:t"})
		}
	}
	for _, obj := range []string{"objective:o1", "objective:o2", "research_case:r1", "research_case:r2", "pursuit:p1", "pursuit:p2",
		"knowledge_asset:k1", "knowledge_asset:k2"} {
		tuples = append(tuples, [3]string{"org_unit:u", "unit", obj})
	}
	tuples = append(tuples, [3]string{"org_unit:u", "restricted_unit", "knowledge_asset:kr1"}, [3]string{"org_unit:u", "restricted_unit", "knowledge_asset:kr2"})
	for _, c := range roleColumns {
		tuples = append(tuples,
			[3]string{c.user, "participant", "objective:o1"},
			[3]string{c.user, "shared_with", "research_case:r1"},
			[3]string{c.user, "collaborator", "pursuit:p1"},
			[3]string{c.user, "viewer", "knowledge_asset:k1"},
			[3]string{c.user, "viewer", "knowledge_asset:kr1"})
	}
	if err := fga.GrantAll(ctx, tuples); err != nil {
		t.Fatal(err)
	}
	instances := map[string][2]string{
		"tenant":                     {"tenant:t", "tenant:t"},
		"objective":                  {"objective:o1", "objective:o2"},
		"research_case":              {"research_case:r1", "research_case:r2"},
		"pursuit":                    {"pursuit:p1", "pursuit:p2"},
		"knowledge_asset":            {"knowledge_asset:k1", "knowledge_asset:k2"},
		"knowledge_asset:restricted": {"knowledge_asset:kr1", "knowledge_asset:kr2"},
	}
	check := func(user, relation, object string) bool {
		ok, err := fga.Check(ctx, authz.Request{Subject: user, Relation: relation, Object: object})
		if err != nil {
			t.Fatalf("Check %s %s %s：%v", user, relation, object, err)
		}
		return ok
	}
	rows := matrix(t)
	if len(rows) < 20 {
		t.Fatalf("矩阵只解析到 %d 行", len(rows))
	}
	for _, r := range rows {
		inst, ok := instances[r.resource]
		if !ok {
			t.Errorf("「%s」的资源 %s 未知", r.action, r.resource)
			continue
		}
		for k, c := range roleColumns {
			var want [2]bool // 参与的实例、未参与的实例
			switch r.cells[k] {
			case "✓":
				want = [2]bool{true, true}
			case "◐":
				want = [2]bool{true, false}
			case "—":
			default:
				t.Errorf("「%s」%s 的取值 %q 非法", r.action, c.title, r.cells[k])
				continue
			}
			got := [2]bool{check(c.user, r.relation, inst[0]), check(c.user, r.relation, inst[1])}
			if got != want {
				t.Errorf("「%s」%s：矩阵为 %s，模型在参与/未参与的资源上为 %v/%v", r.action, c.title, r.cells[k], got[0], got[1])
			}
		}
	}

	// 范围：绑定在兄弟单元的角色不作用于本单元；绑定在上级的角色作用于下级
	if err := fga.GrantAll(ctx, [][3]string{
		{"tenant:t", "parent", "org_unit:v"}, {"org_unit:v", "parent", "org_unit:v1"},
		{"user:bd_v", "business_developer", "org_unit:v"}, {"org_unit:u", "unit", "objective:ou"}, {"org_unit:v1", "unit", "objective:ov1"},
		{"user:bd_v", "participant", "objective:ou"}, {"user:bd_v", "participant", "objective:ov1"},
	}); err != nil {
		t.Fatal(err)
	}
	if check("user:bd_v", "can_view", "objective:ou") {
		t.Error("单元 v 的业务开拓者不应看到兄弟单元 u 的目标（即使被加为参与人）")
	}
	if !check("user:bd_v", "can_draft_policy", "objective:ov1") {
		t.Error("单元 v 的业务开拓者应能为下级单元 v1 中参与的目标起草策略")
	}
}
