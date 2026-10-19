package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 成员退出后的清理与重新加入、更高版本的参与关系同步并发（identity.md 7.1、第5节 SyncResourceParticipants）。
// 在 OpenFGA 前放一个代理作为屏障：退出后的清理读到旧参与关系、发出删除时暂停；此时重新加入必须等待清理提交
// （成员行排他锁），不能先完成并由 N+1 同步写入新关系、再被旧清理删除。最终新关系有效，同版本重放结果不变且无需修复。
func TestPurgeDoesNotRemoveRejoinedGrants(t *testing.T) {
	fga := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if fga == "" || os.Getenv("MIAOQIA_TEST_PG") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 中必须设置 MIAOQIA_TEST_PG 与 MIAOQIA_TEST_OPENFGA")
		}
		t.Skip("MIAOQIA_TEST_PG 或 MIAOQIA_TEST_OPENFGA 未设置")
	}
	t.Setenv("MIAOQIA_ENV", "production")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// OpenFGA 屏障：armed 置为主体时，第一条删除该主体关系的写入在转发前暂停，直到 release
	target, err := url.Parse(fga)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	var armed atomic.Pointer[string]
	arrived, release := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/write") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if who := armed.Load(); who != nil && bytes.Contains(body, []byte(`"deletes"`)) && bytes.Contains(body, []byte(`"user:`+*who+`"`)) {
				armed.Store(nil)
				close(arrived)
				<-release
			}
		}
		forward.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	iam := testenv.NewDatabase(t)
	module := identity.Module()
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", module.Migrations); err != nil {
		t.Fatal(err)
	}
	tenant := ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	admin, bd := "admin-"+suffix, "bd-"+suffix
	owner, err := pgx.Connect(ctx, iam.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	if _, err := owner.Exec(ctx, `INSERT INTO identity_tenants VALUES ($1,'甲','cell-01','active',1)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['business_developer'],1)`, tenant, admin, bd); err != nil {
		t.Fatal(err)
	}
	app, err := db.Open(ctx, iam.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	bindings := runtime.NewBindings()
	env := &runtime.Env{DB: app, Bindings: bindings, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: func(string) string { return proxy.URL }}
	if err := module.Start(ctx, env); err != nil {
		t.Fatal(err)
	}
	public := http.NewServeMux()
	module.Routes(env, public, http.NewServeMux())
	issuer := testissuer.New(t)
	resolver, _ := runtime.Lookup[idapi.SessionResolver](bindings, idapi.SessionBinding)
	secured, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", public, resolver)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(secured)
	defer server.Close()
	directory, _ := runtime.Lookup[idapi.Directory](bindings, idapi.DirectoryBinding)
	authorizer, _ := runtime.Lookup[idapi.Authorizer](bindings, idapi.AuthorizerBinding)

	keys := atomic.Int64{}
	putMember := func(body string) (int, string) {
		r, _ := http.NewRequest("PUT", server.URL+"/v1/identity/members/"+bd, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+issuer.Token(t, admin, tenant, "cell-01", true))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(body, keys.Add(1)))))[:40])
		resp, err := server.Client().Do(r)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	svc := reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:customer-context", Kind: "service"}})
	resource := "objective:" + ids.New("obj")
	sync := func(version int64) *idapi.SyncParticipantsResult {
		t.Helper()
		res, err := directory.SyncResourceParticipants(svc, &idapi.SyncParticipantsRequest{Resource: resource, OrgUnitID: "root", Version: version,
			Participants: []idapi.ParticipantRelation{{Subject: bd, Relation: "participant"}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	canView := func() bool {
		t.Helper()
		ok, err := authorizer.Check(reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: bd, Kind: "user"}}),
			authz.Request{Relation: "can_view", Object: resource})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if res := sync(1); res.Outcome != "applied" || !canView() {
		t.Fatalf("版本 1 的参与关系应生效：%+v", res)
	}

	// 退出：提交后的清理在删除旧关系时被屏障暂停（仍持有成员行排他锁）
	armed.Store(&bd)
	type answer struct {
		status int
		body   string
	}
	leave := make(chan answer, 1)
	go func() {
		s, b := putMember(`{"state":"removed","roles":[],"expected_version":1}`)
		leave <- answer{s, b}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("退出后的清理没有发出删除")
	}
	// 评审给出的交错：清理暂停期间，主体重新加入并由 N+1 同步写入新关系，之后旧清理才恢复删除。
	// 修复后重新加入在成员行锁上等待清理提交，N+1 同步只能在清理之后发生。
	type rejoined struct {
		answer
		res *idapi.SyncParticipantsResult
		err error
	}
	rejoin := make(chan rejoined, 1)
	go func() {
		var out rejoined
		out.status, out.body = putMember(`{"state":"active","roles":["business_developer"],"expected_version":2}`)
		if out.status == 200 {
			out.res, out.err = directory.SyncResourceParticipants(svc, &idapi.SyncParticipantsRequest{Resource: resource, OrgUnitID: "root", Version: 2,
				Participants: []idapi.ParticipantRelation{{Subject: bd, Relation: "participant"}}})
		}
		rejoin <- out
	}()
	// 其他角色会话的等待事件只对超级用户可见
	monitor, err := pgx.Connect(ctx, os.Getenv("MIAOQIA_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close(ctx)
	waited := false
	var early *rejoined
	for deadline := time.Now().Add(4 * time.Second); !waited && early == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var n int
		if err := monitor.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`, iam.Name).Scan(&n); err != nil {
			close(release)
			t.Fatal(err)
		}
		waited = n > 0
		select {
		case r := <-rejoin:
			early = &r
		default:
		}
	}
	close(release)
	if a := <-leave; a.status != 200 {
		t.Fatalf("退出：%d %s", a.status, a.body)
	}
	r := early
	if r == nil {
		select {
		case got := <-rejoin:
			r = &got
		case <-time.After(10 * time.Second):
			t.Fatal("重新加入没有完成")
		}
	}
	if r.status != 200 || r.err != nil || r.res == nil || r.res.Outcome != "applied" || len(r.res.Rejected) != 0 {
		t.Fatalf("重新加入与版本 2 同步：%d %s %+v %v", r.status, r.body, r.res, r.err)
	}
	// 最终授权：新一代成员经版本 2 写入的关系有效；同版本重放返回首次结果，授权不变（无需修复）
	if !canView() {
		t.Fatal("旧的清理删除了重新加入后经版本 2 写入的参与关系")
	}
	if res := sync(2); res.Outcome != "replayed" || res.AppliedVersion != 2 || len(res.Rejected) != 0 || !canView() {
		t.Fatalf("同版本重放应返回首次结果且授权不变：%+v", res)
	}
	if early != nil || !waited {
		t.Fatal("清理在途时重新加入应在成员行锁上等待，而不是先完成")
	}
}
