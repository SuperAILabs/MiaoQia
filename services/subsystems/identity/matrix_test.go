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

// 资源为 tenant 的动作中对全体成员开放的关系（identity.md 第 7.1 节“范围”）；其余 tenant 动作只认绑定在租户上的角色。
var memberWide = map[string]bool{"can_view_profile": true, "can_search_public": true}

// fixture 是一个独立 store 中的租户 T 与其根单元 U；角色按 binding 绑定在租户或单元上（租户管理员总在租户上）。
// 每类资源两个实例：…1 上全部用户都有参与关系（◐ 的“本人参与”），…2 上没有。
func fixture(t *testing.T, binding string) (check func(user, relation, object string) bool, grant func(...[3]string), fga *infra.OpenFGA) {
	t.Helper()
	url := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 中必须设置 MIAOQIA_TEST_OPENFGA，矩阵逐格校验不能跳过")
		}
		t.Skip("未设置 MIAOQIA_TEST_OPENFGA（例如 http://localhost:8081）")
	}
	ctx := context.Background()
	fga, err := infra.ConnectStore(ctx, url, fmt.Sprintf("matrix-%s-%d", binding, time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	grant = func(tuples ...[3]string) {
		t.Helper()
		if err := fga.GrantAll(ctx, tuples); err != nil {
			t.Fatal(err)
		}
	}
	check = func(user, relation, object string) bool {
		t.Helper()
		ok, err := fga.Check(ctx, authz.Request{Subject: user, Relation: relation, Object: object})
		if err != nil {
			t.Fatalf("Check %s %s %s：%v", user, relation, object, err)
		}
		return ok
	}
	tuples := [][3]string{{"tenant:t", "parent", "org_unit:u"}}
	for _, c := range roleColumns {
		if c.relation == "" {
			continue
		}
		scope := "tenant:t"
		if binding == "org_unit" && c.relation != "admin" {
			scope = "org_unit:u"
		}
		tuples = append(tuples, [3]string{c.user, "member", "tenant:t"}, [3]string{c.user, c.relation, scope})
	}
	for _, obj := range []string{"objective:o1", "objective:o2", "research_case:r1", "research_case:r2", "pursuit:p1", "pursuit:p2",
		"knowledge_asset:k1", "knowledge_asset:k2"} {
		tuples = append(tuples, [3]string{"org_unit:u", "unit", obj})
	}
	tuples = append(tuples, [3]string{"org_unit:u", "restricted_unit", "knowledge_asset:kr1"}, [3]string{"org_unit:u", "restricted_unit", "knowledge_asset:kr2"})
	// 外部协作者也写入全部成员式参与关系：它们必须因不是租户成员而不生效
	for _, c := range roleColumns {
		tuples = append(tuples,
			[3]string{c.user, "participant", "objective:o1"},
			[3]string{c.user, "shared_with", "research_case:r1"},
			[3]string{c.user, "collaborator", "pursuit:p1"},
			[3]string{c.user, "viewer", "knowledge_asset:k1"},
			[3]string{c.user, "viewer", "knowledge_asset:kr1"})
	}
	grant(tuples...)
	if err := fga.GrantUntil(ctx, "user:ex", "external_collaborator", "pursuit:p1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return check, grant, fga
}

// 授权模型与权限矩阵逐格一致（真实 OpenFGA，每次使用独立的 store），角色绑定在租户上与绑定在单元上各一遍。
func TestPermissionMatrix(t *testing.T) {
	for _, binding := range []string{"tenant", "org_unit"} {
		t.Run("绑定在"+binding, func(t *testing.T) {
			check, _, _ := fixture(t, binding)
			instances := map[string][2]string{
				"tenant":                     {"tenant:t", "tenant:t"},
				"org_unit":                   {"org_unit:u", "org_unit:u"},
				"objective":                  {"objective:o1", "objective:o2"},
				"research_case":              {"research_case:r1", "research_case:r2"},
				"pursuit":                    {"pursuit:p1", "pursuit:p2"},
				"knowledge_asset":            {"knowledge_asset:k1", "knowledge_asset:k2"},
				"knowledge_asset:restricted": {"knowledge_asset:kr1", "knowledge_asset:kr2"},
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
					// 绑定在单元上的角色不作用于租户整体的动作（对全体成员开放的除外）
					if binding == "org_unit" && r.resource == "tenant" && c.relation != "admin" && !memberWide[r.relation] {
						want = [2]bool{}
					}
					got := [2]bool{check(c.user, r.relation, inst[0]), check(c.user, r.relation, inst[1])}
					if got != want {
						t.Errorf("「%s」%s：期望在参与/未参与的资源上为 %v/%v，模型为 %v/%v", r.action, c.title, want[0], want[1], got[0], got[1])
					}
				}
			}
		})
	}
}

// 范围与成员资格的负向用例。
func TestPermissionBoundaries(t *testing.T) {
	check, grant, fga := fixture(t, "org_unit")

	// 兄弟单元与下级单元：绑定在兄弟单元的角色不作用于本单元；绑定在上级的角色作用于下级
	grant([3]string{"tenant:t", "parent", "org_unit:v"}, [3]string{"org_unit:v", "parent", "org_unit:v1"},
		[3]string{"user:bd_v", "member", "tenant:t"}, [3]string{"user:bd_v", "business_developer", "org_unit:v"},
		[3]string{"org_unit:u", "unit", "objective:ou"}, [3]string{"org_unit:v1", "unit", "objective:ov1"},
		[3]string{"user:bd_v", "participant", "objective:ou"}, [3]string{"user:bd_v", "participant", "objective:ov1"})
	if check("user:bd_v", "can_view", "objective:ou") {
		t.Error("单元 v 的业务开拓者不应看到兄弟单元 u 的目标（即使被加为参与人）")
	}
	if !check("user:bd_v", "can_draft_policy", "objective:ov1") {
		t.Error("单元 v 的业务开拓者应能为下级单元 v1 中参与的目标起草策略")
	}
	if check("user:bd_v", "can_start_research", "org_unit:u") || !check("user:bd_v", "can_start_research", "org_unit:v1") {
		t.Error("发起研究应只在角色绑定的单元及其下级生效")
	}

	// 跨租户：参与关系不能跨租户生效
	grant([3]string{"tenant:other", "parent", "org_unit:other"}, [3]string{"org_unit:other", "unit", "objective:other"},
		[3]string{"user:bd_v", "participant", "objective:other"})
	if check("user:bd_v", "can_view", "objective:other") {
		t.Error("另一租户的资源不能仅凭参与关系访问")
	}

	// 停用成员：没有成员资格时，残留的角色与参与关系全部失效
	grant([3]string{"user:gone", "research_analyst", "tenant:t"}, [3]string{"user:gone", "decision_maker", "org_unit:u"},
		[3]string{"user:gone", "owner", "pursuit:p2"}, [3]string{"user:gone", "owner", "research_case:r2"},
		[3]string{"user:gone", "participant", "objective:o2"})
	for _, c := range [][2]string{{"can_view_profile", "tenant:t"}, {"can_start_research", "org_unit:u"}, {"can_view", "objective:o2"},
		{"can_view", "pursuit:p2"}, {"can_collaborate", "pursuit:p2"}, {"can_view", "research_case:r2"}, {"can_view", "knowledge_asset:k2"}} {
		if check("user:gone", c[0], c[1]) {
			t.Errorf("停用成员不应有 %s %s", c[0], c[1])
		}
	}

	// 服务账号：可以绑定角色
	grant([3]string{"service_account:sa", "member", "tenant:t"}, [3]string{"service_account:sa", "research_analyst", "org_unit:u"})
	if !check("service_account:sa", "can_start_research", "org_unit:u") || !check("service_account:sa", "can_view", "research_case:r2") {
		t.Error("绑定研究分析者的服务账号应能发起研究、查看单元内的研究")
	}
	if check("service_account:sa", "can_view_usage", "tenant:t") {
		t.Error("服务账号不应有其角色之外的权限")
	}

	// 外部协作者：到期即失效；不能经共享看到研究
	if err := fga.GrantUntil(context.Background(), "user:ex_old", "external_collaborator", "pursuit:p2", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if check("user:ex_old", "can_view_limited", "pursuit:p2") || check("user:ex_old", "can_collaborate", "pursuit:p2") {
		t.Error("到期的外部协作关系应失效")
	}
	if !check("user:ex", "can_view_limited", "pursuit:p1") || check("user:ex", "can_view", "pursuit:p1") {
		t.Error("外部协作者应只有推进的受限视图")
	}
	if check("user:ex", "can_view", "research_case:r1") {
		t.Error("研究不能共享给外部协作者")
	}
}
