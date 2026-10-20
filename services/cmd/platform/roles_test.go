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
	t.Setenv("MIAOQIA_ENV", "development")
	t.Setenv("MIAOQIA_IDENTITY_OPENFGA_URL", fga)
	t.Setenv("MIAOQIA_IDENTITY_URL", "http://127.0.0.1:1/internal")
	t.Setenv("MIAOQIA_CUSTOMER_CONTEXT_URL", "http://127.0.0.1:1/internal")
	variants := []map[string]string{
		{},
		{"MIAOQIA_BINDING_IDENTITY": "remote"},
		{"MIAOQIA_BINDING_CUSTOMER_CONTEXT": "remote"},
		{"MIAOQIA_BINDING_IDENTITY": "remote", "MIAOQIA_BINDING_CUSTOMER_CONTEXT": "remote"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for role := range mf.Roles {
		for _, v := range variants {
			t.Run(fmt.Sprintf("%s %v", role, v), func(t *testing.T) {
				for _, k := range []string{"MIAOQIA_BINDING_IDENTITY", "MIAOQIA_BINDING_CUSTOMER_CONTEXT"} {
					t.Setenv(k, v[k])
				}
				loaded, _, closeAll, err := assemble(context.Background(), log, role, nil)
				defer closeAll()
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range loaded {
					if (m.Name == "identity" && v["MIAOQIA_BINDING_IDENTITY"] == "remote") ||
						(m.Name == "customer-context" && v["MIAOQIA_BINDING_CUSTOMER_CONTEXT"] == "remote") {
						t.Fatalf("远程绑定的 %s 仍在本地加载", m.Name)
					}
				}
			})
		}
	}
}
