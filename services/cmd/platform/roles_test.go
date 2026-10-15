package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
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
