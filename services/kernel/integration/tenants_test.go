package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// identity.Tenants（R-01）：scheduler 角色按 cell 分页枚举 active 租户；其他主体一律被拒，停用租户与其他 cell 不返回。
func TestActiveTenantsForScheduler(t *testing.T) {
	fga := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if fga == "" {
		t.Skip("MIAOQIA_TEST_OPENFGA not set")
	}
	ctx := context.Background()
	database := testenv.NewDatabase(t)
	module := identity.Module()
	if err := db.Migrate(ctx, database.OwnerDSN, database.Name+"_app", module.Migrations); err != nil {
		t.Fatal(err)
	}
	app, err := db.Open(ctx, database.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	bindings := runtime.NewBindings()
	env := &runtime.Env{DB: app, Bindings: bindings, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Cell: "cell-01", Config: func(string) string { return fga }}
	if err := module.Start(ctx, env); err != nil {
		t.Fatal(err)
	}
	tenants, ok := runtime.Lookup[api.Tenants](bindings, api.TenantsBinding)
	if !ok {
		t.Fatal("identity 应提供 identity.Tenants")
	}

	owner, err := pgx.Connect(ctx, database.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	id := func(c byte) string { return "tnt_" + string(slices.Repeat([]byte{c}, 26)) }
	for _, row := range []struct {
		id, state string
	}{{id('3'), "active"}, {id('1'), "active"}, {id('2'), "suspended"}, {id('4'), "active"}, {id('5'), "active"}} {
		if _, err := owner.Exec(ctx, `INSERT INTO identity_tenants (id, name, cell, state, version) VALUES ($1, 'T', 'cell-01', $2, 1)`, row.id, row.state); err != nil {
			t.Fatal(err)
		}
	}

	scheduler := reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: api.SchedulerPrincipal, Kind: "service"}})
	var all []string
	after := ""
	for page := 0; ; page++ {
		got, err := tenants.ActiveTenants(scheduler, "cell-01", after, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, got...)
		if len(got) < 2 {
			break
		}
		after = got[len(got)-1]
		if page > 5 {
			t.Fatal("分页不收敛")
		}
	}
	if want := []string{id('1'), id('3'), id('4'), id('5')}; !slices.Equal(all, want) {
		t.Fatalf("应按 ID 升序返回全部 active 租户、不含停用的：%v", all)
	}
	if got, err := tenants.ActiveTenants(scheduler, "cell-02", "", 10); err != nil || len(got) != 0 {
		t.Fatalf("其他 cell 不应返回：%v %v", got, err)
	}

	code := func(err error) string {
		var e *api.Error
		if errors.As(err, &e) {
			return e.Code
		}
		return "?"
	}
	t.Run("越权：用户主体、其他服务主体、带租户上下文的 scheduler、无上下文均被拒", func(t *testing.T) {
		for name, c := range map[string]context.Context{
			"用户主体":           reqctx.With(ctx, reqctx.Context{Tenant: id('1'), Cell: "cell-01", Principal: reqctx.Principal{ID: "u1", Kind: "user"}}),
			"其他服务主体":         reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: "svc:channels", Kind: "service"}}),
			"服务账号冒用名称":       reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: api.SchedulerPrincipal, Kind: "service_account"}}),
			"带租户的 scheduler": reqctx.With(ctx, reqctx.Context{Tenant: id('1'), Cell: "cell-01", Principal: reqctx.Principal{ID: api.SchedulerPrincipal, Kind: "service"}}),
		} {
			if got, err := tenants.ActiveTenants(c, "cell-01", "", 10); code(err) != "forbidden" || got != nil {
				t.Errorf("%s：应 403 forbidden 且不返回任何租户：%v %v", name, got, err)
			}
		}
		// 没有请求上下文时调用管线先拒绝，不进入实现
		if got, err := tenants.ActiveTenants(ctx, "cell-01", "", 10); !errors.Is(err, reqctx.ErrMissing) || got != nil {
			t.Errorf("无请求上下文：应拒绝：%v %v", got, err)
		}
	})
	t.Run("反例：无效 cell、游标与页大小", func(t *testing.T) {
		for name, args := range map[string]struct {
			cell, after string
			limit       int
		}{
			"空 cell":  {"", "", 10},
			"非法 cell": {"cell_01'--", "", 10},
			"非法游标":    {"cell-01", "x' OR '1'='1", 10},
			"页大小为零":   {"cell-01", "", 0},
			"页大小超过上限": {"cell-01", "", api.MaxTenantsPage + 1},
		} {
			if _, err := tenants.ActiveTenants(scheduler, args.cell, args.after, args.limit); code(err) != "invalid_request" {
				t.Errorf("%s：应 400 invalid_request：%v", name, err)
			}
		}
	})
}
