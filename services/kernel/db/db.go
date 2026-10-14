// Package db 是子系统的数据访问：每个子系统独立的连接池与数据库账号，事务内按请求上下文设置
// 行级安全会话变量（app.tenant_id、app.principal）。子系统之间不共享连接与事务。
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

//go:embed kernel.sql
var kernelFS embed.FS

// KernelSQL 是内核表的定义（与 contracts/storage/kernel.sql 一致，测试校验）。
func KernelSQL() string {
	b, _ := kernelFS.ReadFile("kernel.sql")
	return string(b)
}

// DB 是一个子系统的数据库句柄。
type DB struct {
	Pool *pgxpool.Pool
}

// Open 以应用账号连接子系统数据库。
func Open(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

// Close 关闭连接池。
func (d *DB) Close() { d.Pool.Close() }

// Tx 在事务中执行 fn。事务开始时把请求上下文写入会话变量（事务级），租户域表的 RLS 策略据此过滤：
//
//	USING (tenant_id = current_setting('app.tenant_id', true))
//
// 没有请求上下文时拒绝执行，避免以“无租户”身份误读租户数据。
func (d *DB) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return reqctx.ErrMissing
	}
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`SELECT set_config('app.tenant_id', $1, true), set_config('app.cell', $2, true), set_config('app.principal', $3, true)`,
			rc.Tenant, rc.Cell, rc.Principal.ID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Migrate 以 owner 账号执行内核表与子系统迁移（goose，迁移文件在 migrations 中）。
// appRole 为应用账号名，迁移中可用 ${MIAOQIA_DB_APP_ROLE} 引用（需 -- +goose ENVSUB ON），例如收回追加式表的修改权限。
func Migrate(ctx context.Context, ownerDSN, appRole string, migrations fs.FS) error {
	cfg, err := pgx.ParseConfig(ownerDSN)
	if err != nil {
		return err
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer sqlDB.Close()
	if _, err := sqlDB.ExecContext(ctx, KernelSQL()); err != nil {
		return fmt.Errorf("内核表：%w", err)
	}
	if migrations == nil {
		return nil
	}
	if appRole != "" {
		if err := os.Setenv("MIAOQIA_DB_APP_ROLE", appRole); err != nil {
			return err
		}
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		if errors.Is(err, goose.ErrNoMigrations) {
			return nil
		}
		return err
	}
	_, err = p.Up(ctx)
	return err
}
