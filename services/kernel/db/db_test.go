package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// Go 内核表定义与契约一致。
func TestKernelSQLMatchesContract(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "storage", "kernel.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != db.KernelSQL() {
		t.Fatal("services/kernel/db/kernel.sql 与 contracts/storage/kernel.sql 不一致")
	}
}

func tenant(id string) context.Context {
	return reqctx.With(context.Background(), reqctx.Context{Tenant: id, Cell: "01", Principal: reqctx.Principal{ID: "usr_test", Kind: "user"}})
}

// 行级安全：租户只能看到自己的行；无请求上下文时拒绝访问。
func TestRowLevelSecurity(t *testing.T) {
	d := testenv.NewDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, d.OwnerDSN, "", nil); err != nil {
		t.Fatal(err)
	}
	oc, err := pgx.Connect(ctx, d.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	_, err = oc.Exec(ctx, `
		CREATE TABLE notes (id text PRIMARY KEY, tenant_id text NOT NULL DEFAULT current_setting('app.tenant_id'), body text);
		ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
		CREATE POLICY tenant_isolation ON notes USING (tenant_id = current_setting('app.tenant_id', true))
			WITH CHECK (tenant_id = current_setting('app.tenant_id', true));`)
	oc.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	app, err := db.Open(ctx, d.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	write := func(c context.Context, id string) error {
		return app.Tx(c, func(tx pgx.Tx) error {
			_, err := tx.Exec(c, `INSERT INTO notes (id, body) VALUES ($1, 'x')`, id)
			return err
		})
	}
	count := func(c context.Context) (n int) {
		_ = app.Tx(c, func(tx pgx.Tx) error { return tx.QueryRow(c, `SELECT count(*) FROM notes`).Scan(&n) })
		return
	}
	if err := write(tenant("tnt_a"), "n1"); err != nil {
		t.Fatal(err)
	}
	if err := write(tenant("tnt_b"), "n2"); err != nil {
		t.Fatal(err)
	}
	if a, b := count(tenant("tnt_a")), count(tenant("tnt_b")); a != 1 || b != 1 {
		t.Fatalf("每个租户应只看到自己的 1 行，得到 a=%d b=%d", a, b)
	}
	// 写入其他租户的行被 WITH CHECK 拒绝
	err = app.Tx(tenant("tnt_a"), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO notes (id, tenant_id, body) VALUES ('n3', 'tnt_b', 'x')`)
		return err
	})
	if err == nil {
		t.Fatal("跨租户写入应被拒绝")
	}
	if err := app.Tx(context.Background(), func(pgx.Tx) error { return nil }); err != reqctx.ErrMissing {
		t.Fatalf("无请求上下文应拒绝，得到 %v", err)
	}
}
