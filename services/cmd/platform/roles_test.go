package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	chnapi "github.com/superailabs/miaoqia/services/subsystems/channels/api"
	commerceapi "github.com/superailabs/miaoqia/services/subsystems/commerce/api"
	ccapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detectionapi "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	pursuitapi "github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

func TestMigrationRejectsInvalidRoleBeforeDatabase(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "platform"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "platform", "roles.yaml"), []byte("roles:\n  api:\n    subsystems: [identity, identity]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "subsystems.yaml"), []byte("subsystems:\n  identity: {unit: platform, data_scopes: [platform]}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIAOQIA_DEPLOY_DIR", root)
	if err := migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), ""); err == nil {
		t.Fatal("重复子系统未在迁移前拒绝")
	}
}

func TestPublicSearchMigrationDoesNotMigrateTenantModules(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "platform"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "platform", "roles.yaml"), []byte("roles:\n  public-search-api:\n    subsystems: [search]\n    data_scopes: {search: public}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "subsystems.yaml"), []byte("subsystems:\n  search: {unit: platform, data_scopes: [public, tenant]}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIAOQIA_DEPLOY_DIR", root)
	// 当前search没有数据库；若迁移忽略角色筛选，会尝试连接其他模块并失败。
	if err := migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "public-search-api"); err != nil {
		t.Fatal(err)
	}
}

// TestEveryRoleResolvesBindings 按部署清单中的每个角色、在本地与远程绑定的组合下组装并启动子系统（不连接数据库、不服务、不消费），
// 断言被加载子系统依赖的绑定全部可解析：只在单进程中登记的跨子系统绑定会让拆分后的角色启动失败。
func TestEveryRoleResolvesBindings(t *testing.T) {
	fga := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if fga == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 中必须设置 MIAOQIA_TEST_OPENFGA")
		}
		t.Skip("MIAOQIA_TEST_OPENFGA 未设置（本地加载 identity 需要 OpenFGA）")
	}
	deploy, err := filepath.Abs(filepath.Join("..", "..", "..", "deploy"))
	if err != nil {
		t.Fatal(err)
	}
	var mf manifest
	if err := readYAML(filepath.Join(deploy, "platform", "roles.yaml"), &mf); err != nil {
		t.Fatal(err)
	}
	orig := openDB
	// 连接池惰性建立连接：组装与启动不应访问数据库
	openDB = func(ctx context.Context, _ string) (*db.DB, error) {
		pool, err := pgxpool.New(ctx, "postgres://unused@127.0.0.1:1/unused")
		return &db.DB{Pool: pool}, err
	}
	t.Cleanup(func() { openDB = orig })
	t.Setenv("MIAOQIA_DEPLOY_DIR", deploy)
	packs := filepath.Join(filepath.Dir(deploy), "packs")
	t.Setenv("MIAOQIA_CUSTOMER_CONTEXT_PACKS_DIR", packs)
	t.Setenv("MIAOQIA_LEARNING_PACKS_DIR", packs)
	t.Setenv("MIAOQIA_DETECTION_PACKS_DIR", packs)
	t.Setenv("MIAOQIA_CHANNELS_PACKS_DIR", packs)
	t.Setenv("MIAOQIA_PURSUIT_PACKS_DIR", packs)
	t.Setenv("MIAOQIA_ENV", "development")
	t.Setenv("MIAOQIA_IDENTITY_OPENFGA_URL", fga)
	t.Setenv("MIAOQIA_IDENTITY_URL", "http://127.0.0.1:1/internal")
	t.Setenv("MIAOQIA_CUSTOMER_CONTEXT_URL", "http://127.0.0.1:1/internal")
	t.Setenv("MIAOQIA_COMMERCE_URL", "http://127.0.0.1:1/internal")
	t.Setenv("MIAOQIA_DETECTION_URL", "http://127.0.0.1:1/internal")
	t.Setenv("MIAOQIA_PURSUIT_URL", "http://127.0.0.1:1/internal")
	keys := []string{"MIAOQIA_BINDING_IDENTITY", "MIAOQIA_BINDING_CUSTOMER_CONTEXT", "MIAOQIA_BINDING_COMMERCE", "MIAOQIA_BINDING_DETECTION",
		"MIAOQIA_BINDING_PURSUIT"}
	subsystemOf := map[string]string{"MIAOQIA_BINDING_IDENTITY": "identity", "MIAOQIA_BINDING_CUSTOMER_CONTEXT": "customer-context",
		"MIAOQIA_BINDING_COMMERCE": "commerce", "MIAOQIA_BINDING_DETECTION": "detection", "MIAOQIA_BINDING_PURSUIT": "pursuit"}
	variants := []map[string]string{
		{},
		{"MIAOQIA_BINDING_IDENTITY": "remote"},
		{"MIAOQIA_BINDING_CUSTOMER_CONTEXT": "remote"},
		{"MIAOQIA_BINDING_IDENTITY": "remote", "MIAOQIA_BINDING_CUSTOMER_CONTEXT": "remote"},
		{"MIAOQIA_BINDING_COMMERCE": "remote"},
		{"MIAOQIA_BINDING_DETECTION": "remote"},
		{"MIAOQIA_BINDING_PURSUIT": "remote"},
		{"MIAOQIA_BINDING_IDENTITY": "remote", "MIAOQIA_BINDING_CUSTOMER_CONTEXT": "remote", "MIAOQIA_BINDING_COMMERCE": "remote", "MIAOQIA_BINDING_DETECTION": "remote"},
		{"MIAOQIA_BINDING_IDENTITY": "remote", "MIAOQIA_BINDING_CUSTOMER_CONTEXT": "remote", "MIAOQIA_BINDING_COMMERCE": "remote", "MIAOQIA_BINDING_DETECTION": "remote",
			"MIAOQIA_BINDING_PURSUIT": "remote"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for role := range mf.Roles {
		for _, v := range variants {
			t.Run(fmt.Sprintf("%s %v", role, v), func(t *testing.T) {
				for _, k := range keys {
					t.Setenv(k, v[k])
				}
				loaded, envs, closeAll, err := assemble(context.Background(), log, role, nil)
				defer closeAll()
				if err != nil {
					t.Fatal(err)
				}
				for i, m := range loaded {
					for _, k := range keys {
						if m.Name == subsystemOf[k] && v[k] == "remote" {
							t.Fatalf("远程绑定的 %s 仍在本地加载", m.Name)
						}
					}
					// 第3段新增的跨子系统绑定：detection 每批读取生效策略与上下文，用户入口经 ObjectiveAccess 判断
					if m.Name == "detection" {
						b := envs[i].Bindings
						_, access := runtime.Lookup[ccapi.ObjectiveAccess](b, ccapi.AccessBinding)
						_, reader := runtime.Lookup[ccapi.ContextReader](b, ccapi.ContextBinding)
						_, active := runtime.Lookup[ccapi.ActivePolicies](b, ccapi.ActivePoliciesBinding)
						if !access || !reader || !active {
							t.Fatalf("detection 的绑定不完整：access=%v context=%v active=%v", access, reader, active)
						}
					}
					// 第4段新增的跨子系统绑定：gateway 的对象页经 identity 授权、commerce 计量、detection 读取信号固定的求值输入、
					// customer-context 读取档案品类
					if m.Name == "gateway" {
						b := envs[i].Bindings
						_, authz := runtime.Lookup[idapi.Authorizer](b, idapi.AuthorizerBinding)
						_, meter := runtime.Lookup[commerceapi.ViewMetering](b, commerceapi.MeteringBinding)
						_, explainer := runtime.Lookup[detectionapi.SignalExplainer](b, detectionapi.ExplainerBinding)
						_, reader := runtime.Lookup[ccapi.ContextReader](b, ccapi.ContextBinding)
						if !authz || !meter || !explainer || !reader {
							t.Fatalf("gateway 的绑定不完整：authz=%v meter=%v explainer=%v context=%v", authz, meter, explainer, reader)
						}
					}
					// 第5段新增的跨子系统绑定：channels 经 detection 读取简报条目与信号卡片并在渠道内处理、经 customer-context 判断目标访问
					// 与读取生效策略、经 identity 读取审批；并提供 Briefings
					if m.Name == "channels" {
						b := envs[i].Bindings
						_, briefing := runtime.Lookup[detectionapi.Briefing](b, detectionapi.BriefingBinding)
						_, access := runtime.Lookup[ccapi.ObjectiveAccess](b, ccapi.AccessBinding)
						_, active := runtime.Lookup[ccapi.ActivePolicies](b, ccapi.ActivePoliciesBinding)
						_, approvals := runtime.Lookup[idapi.ApprovalReader](b, idapi.ApprovalReaderBinding)
						_, own := runtime.Lookup[chnapi.Briefings](b, chnapi.BriefingsBinding)
						_, pursuits := runtime.Lookup[pursuitapi.Actions](b, pursuitapi.ActionsBinding)
						// C-09：渠道通知（任务提醒卡片与授权被拒的通知）的投递入口
						_, notices := runtime.Lookup[chnapi.Notices](b, chnapi.NoticesBinding)
						if !briefing || !access || !active || !approvals || !own || !pursuits || !notices {
							t.Fatalf("channels 的绑定不完整：detection=%v access=%v active=%v approvals=%v briefings=%v pursuit=%v notices=%v", briefing, access, active,
								approvals, own, pursuits, notices)
						}
					}
					// 第6段新增的跨子系统绑定：pursuit 经 identity 授权、同步参与关系与核验成员，经 customer-context 判断成员的目标访问；
					// 服务对外接口的 api 角色经 detection 核对信号；并提供 Actions 与 Reminders
					if m.Name == "pursuit" {
						b := envs[i].Bindings
						_, authz := runtime.Lookup[idapi.Authorizer](b, idapi.AuthorizerBinding)
						_, directory := runtime.Lookup[idapi.Directory](b, idapi.DirectoryBinding)
						_, members := runtime.Lookup[idapi.SessionResolver](b, idapi.SessionBinding)
						_, scope := runtime.Lookup[ccapi.ObjectiveScope](b, ccapi.ScopeBinding)
						_, briefing := runtime.Lookup[detectionapi.Briefing](b, detectionapi.BriefingBinding)
						_, actions := runtime.Lookup[pursuitapi.Actions](b, pursuitapi.ActionsBinding)
						_, reminders := runtime.Lookup[pursuitapi.Reminders](b, pursuitapi.RemindersBinding)
						if !authz || !directory || !members || !scope || (role == "api" && !briefing) || !actions || !reminders {
							t.Fatalf("pursuit 的绑定不完整：authz=%v directory=%v members=%v scope=%v detection=%v actions=%v reminders=%v", authz, directory,
								members, scope, briefing, actions, reminders)
						}
					}
				}
			})
		}
	}
}
