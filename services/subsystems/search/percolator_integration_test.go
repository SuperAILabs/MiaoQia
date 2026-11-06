package search

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/search/api"
	"github.com/superailabs/miaoqia/services/subsystems/search/migrations"
)

func percolatorContext(tenant, principal, kind string) context.Context {
	return reqctx.With(context.Background(), reqctx.Context{Tenant: tenant, Cell: "cell-01",
		Principal: reqctx.Principal{ID: principal, Kind: kind}, Purpose: "monitoring"})
}

func TestPercolatorPostgresPersistenceAndRLS(t *testing.T) {
	admin := os.Getenv("MIAOQIA_TEST_PG")
	if admin == "" {
		t.Skip("未设置 MIAOQIA_TEST_PG（例如 postgres://postgres:postgres@localhost:5432/postgres）")
	}
	ctx := context.Background()
	adminConn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer adminConn.Close(ctx)
	name := fmt.Sprintf("sea%d_%04d", time.Now().Unix()%100000, rand.IntN(10000))
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE ROLE %s_owner LOGIN PASSWORD 'pw'`, name),
		fmt.Sprintf(`CREATE ROLE %s_app LOGIN PASSWORD 'pw' NOBYPASSRLS`, name),
		fmt.Sprintf(`CREATE DATABASE %s OWNER %s_owner`, name, name),
	} {
		if _, err := adminConn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	adminURL, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	dsn := func(user string) string {
		u := *adminURL
		u.User = url.UserPassword(user, "pw")
		u.Path = "/" + name
		return u.String()
	}
	owner, err := pgx.Connect(ctx, dsn(name+"_owner"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s_app`, name),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s_app`, name),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s_app`, name),
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			owner.Close(ctx)
			t.Fatal(err)
		}
	}
	owner.Close(ctx)
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			defer c.Close(context.Background())
			_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
			_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s_app`, name))
			_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s_owner`, name))
		}
	})
	dbDSN := dsn(name + "_app")
	if err := db.Migrate(ctx, dsn(name+"_owner"), name+"_app", migrations.FS); err != nil {
		t.Fatal(err)
	}
	handle, err := db.Open(ctx, dbDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	queryA := api.WatchQuery{ID: "obj_01JA8ZXXXXXXXXXXXXXXXXXXX1", Tenant: "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1", Cell: "cell-01", FactTypes: []string{"tendering.bid_result"}}
	queryB := api.WatchQuery{ID: "obj_01JA8ZXXXXXXXXXXXXXXXXXXX2", Tenant: "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX2", Cell: "cell-01", FactTypes: []string{"tendering.bid_result"}}
	p := newPercolator(handle)
	if err := p.Register(percolatorContext(queryA.Tenant, "usr_01JA8ZXXXXXXXXXXXXXXXXXXX1", "user"), queryA); err != nil {
		t.Fatal(err)
	}
	if err := p.Register(percolatorContext(queryB.Tenant, "usr_01JA8ZXXXXXXXXXXXXXXXXXXX2", "user"), queryB); err != nil {
		t.Fatal(err)
	}
	if err := p.Register(percolatorContext(queryA.Tenant, "usr_01JA8ZXXXXXXXXXXXXXXXXXXX1", "user"), queryB); err == nil {
		t.Fatal("ordinary tenant principal registered another tenant query")
	}
	fact := api.Fact{Cell: "cell-01", Units: []api.FactUnit{{FactType: "tendering.bid_result"}}}

	matched, err := p.Percolate(percolatorContext(queryA.Tenant, "usr_01JA8ZXXXXXXXXXXXXXXXXXXX1", "user"), fact)
	if err != nil || len(matched) != 1 || matched[0] != queryA.Tenant {
		t.Fatalf("tenant A visibility matched=%v err=%v", matched, err)
	}
	matched, err = p.Percolate(percolatorContext(queryA.Tenant, "svc:detection", "service"), fact)
	if err != nil || len(matched) != 2 || matched[0] != queryA.Tenant || matched[1] != queryB.Tenant {
		t.Fatalf("service cross-tenant visibility matched=%v err=%v", matched, err)
	}
	if err := p.Remove(percolatorContext(queryA.Tenant, "usr_01JA8ZXXXXXXXXXXXXXXXXXXX1", "user"), queryB.ID); err != nil {
		t.Fatal(err)
	}
	matched, err = p.Percolate(percolatorContext(queryA.Tenant, "svc:detection", "service"), fact)
	if err != nil || len(matched) != 2 {
		t.Fatalf("cross-tenant remove changed another tenant query matched=%v err=%v", matched, err)
	}

	if err := p.Remove(percolatorContext(queryA.Tenant, "usr_01JA8ZXXXXXXXXXXXXXXXXXXX1", "user"), queryA.ID); err != nil {
		t.Fatal(err)
	}
	matched, err = p.Percolate(percolatorContext(queryA.Tenant, "svc:detection", "service"), fact)
	if err != nil || len(matched) != 1 || matched[0] != queryB.Tenant {
		t.Fatalf("removed query still visible matched=%v err=%v", matched, err)
	}

	handle.Close()
	handle, err = db.Open(ctx, dbDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	recovered := newPercolator(handle)
	matched, err = recovered.Percolate(percolatorContext(queryB.Tenant, "usr_01JA8ZXXXXXXXXXXXXXXXXXXX2", "user"), fact)
	if err != nil || len(matched) != 1 || matched[0] != queryB.Tenant {
		t.Fatalf("restart recovery matched=%v err=%v", matched, err)
	}
}
