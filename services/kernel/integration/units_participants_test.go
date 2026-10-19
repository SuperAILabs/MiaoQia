package integration_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	ctxapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	idremote "github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
)

// flakyDirectory 在 fail 置位时让 SyncResourceParticipants 失败（identity 不可用），其余调用照常转发。
type flakyDirectory struct {
	idapi.Directory
	fail atomic.Bool
}

func (f *flakyDirectory) SyncResourceParticipants(ctx context.Context, req *idapi.SyncParticipantsRequest) (*idapi.SyncParticipantsResult, error) {
	if f.fail.Load() {
		return nil, errors.New("identity 不可用（测试注入）")
	}
	return f.Directory.SyncResourceParticipants(ctx, req)
}

// identity 组织单元、单元角色、业务资源授权、服务主体授权与 SyncResourceParticipants，以及 customer-context 对它们的使用
// （PRD 第2段用例 14；identity.md 7.1、7.2、第5节）。真实 PostgreSQL + OpenFGA + 网关令牌校验 + mTLS 内部端口与远程绑定。
func TestOrgUnitsParticipantsAndServiceAuthorization(t *testing.T) {
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
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	iam := testenv.NewDatabase(t)
	idModule := identity.Module()
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", idModule.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2 := ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	user := func(role string) string { return role + "-" + suffix }
	admin, dm, bd, ra, admin2 := user("admin"), user("dm"), user("bd"), user("ra"), user("admin2")
	dmGas, bdGas, bdWater, raWater := user("dm-gas"), user("bd-gas"), user("bd-water"), user("ra-water")
	owner, err := pgx.Connect(ctx, iam.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	// 租户直接写入目录：根单元由触发器随租户创建
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO identity_tenants VALUES ($1,'甲','cell-01','active',1), ($2,'乙','cell-01','active',1)`, []any{t1, t2}},
		{`INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['decision_maker'],1), ($1,$4,'active',ARRAY['business_developer'],1),
 ($1,$5,'active',ARRAY['research_analyst'],1), ($6,$7,'active',ARRAY['admin','decision_maker'],1)`, []any{t1, admin, dm, bd, ra, t2, admin2}},
	} {
		if _, err := owner.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	owner.Close(ctx)
	iamApp, err := db.Open(ctx, iam.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer iamApp.Close()
	bindings := runtime.NewBindings()
	idEnv := &runtime.Env{DB: iamApp, Bindings: bindings, Log: quiet, Config: func(string) string { return fga }}
	if err := idModule.Start(ctx, idEnv); err != nil {
		t.Fatal(err)
	}

	// customer-context 经可注入故障的目录绑定调用 identity，用于模拟命令提交后同步失败与重试耗尽
	localDir, _ := runtime.Lookup[idapi.Directory](bindings, idapi.DirectoryBinding)
	flaky := &flakyDirectory{Directory: localDir}
	bindings.Provide(idapi.DirectoryBinding, idapi.Directory(flaky))

	database := testenv.NewDatabase(t)
	module := customercontext.Module()
	if err := db.Migrate(ctx, database.OwnerDSN, database.Name+"_app", module.Migrations); err != nil {
		t.Fatal(err)
	}
	appDB, err := db.Open(ctx, database.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer appDB.Close()
	packs, _ := filepath.Abs(filepath.Join("..", "..", "..", "packs"))
	env := &runtime.Env{DB: appDB, Bindings: bindings, Log: quiet, Cell: "cell-01", Config: func(k string) string {
		if k == "PACKS_DIR" {
			return packs
		}
		return ""
	}}
	if err := module.Start(ctx, env); err != nil {
		t.Fatal(err)
	}
	public, internal := http.NewServeMux(), http.NewServeMux()
	idModule.Routes(idEnv, public, internal)
	module.Routes(env, public, internal)
	issuer := testissuer.New(t)
	resolver, _ := runtime.Lookup[idapi.SessionResolver](bindings, idapi.SessionBinding)
	secured, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", public, resolver)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(secured)
	defer server.Close()
	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	intern := httptest.NewUnstartedServer(internal)
	intern.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	intern.StartTLS()
	defer intern.Close()
	remoteClient, err := idremote.NewAuthorizer(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	localAuthz, _ := runtime.Lookup[idapi.Authorizer](bindings, idapi.AuthorizerBinding)
	remoteDir := remoteClient.(idapi.Directory)
	reader, _ := runtime.Lookup[ctxapi.ContextReader](bindings, ctxapi.ContextBinding)

	keys := 0
	do := func(t *testing.T, subject, tenant string, mfa bool, method, path, body string) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+issuer.Token(t, subject, tenant, "cell-01", mfa))
		r.Header.Set("Content-Type", "application/json")
		keys++
		r.Header.Set("Idempotency-Key", fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(subject, method, path, body, keys))))[:40])
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	expect := func(t *testing.T, subject, tenant string, mfa bool, method, path, body string, status int, code ...string) []byte {
		t.Helper()
		got, raw := do(t, subject, tenant, mfa, method, path, body)
		if got != status {
			t.Fatalf("%s %s %s：得到 %d，期望 %d：%s", subject, method, path, got, status, raw)
		}
		if len(code) > 0 {
			var p struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(raw, &p) != nil || p.Code != code[0] {
				t.Fatalf("期望错误码 %s，得到 %s", code[0], raw)
			}
		}
		return raw
	}
	decode := func(t *testing.T, raw []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}
	as := func(tenant, principal, kind string) context.Context {
		return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: principal, Kind: kind}})
	}
	check := func(t *testing.T, a idapi.Authorizer, who, relation, object string) bool {
		t.Helper()
		ok, err := a.Check(as(t1, who, "user"), authz.Request{Relation: relation, Object: object})
		if err != nil {
			t.Fatalf("%s %s %s：%v", who, relation, object, err)
		}
		return ok
	}
	errCode := func(err error) string {
		var e *idapi.Error
		if err == nil {
			return ""
		}
		for cur := err; cur != nil; {
			if x, ok := cur.(*idapi.Error); ok {
				e = x
				break
			}
			u, ok := cur.(interface{ Unwrap() error })
			if !ok {
				break
			}
			cur = u.Unwrap()
		}
		if e == nil {
			return err.Error()
		}
		return e.Code
	}

	const units = "/v1/identity/org-units"
	var gas, east, water identityapi.OrgUnit
	t.Run("组织单元：创建、列表、读取与管理权限", func(t *testing.T) {
		expect(t, admin, t1, false, "POST", units, `{"name":"燃气事业部","parent_id":"root"}`, 403, "mfa_required")
		expect(t, dm, t1, true, "POST", units, `{"name":"燃气事业部","parent_id":"root"}`, 403, "forbidden")
		expect(t, admin, t1, true, "POST", units, `{"name":"燃气事业部","parent_id":"unt_01JA8ZXXXXXXXXXXXXXXXXXXX9"}`, 404, "org_unit_not_found")
		expect(t, admin, t1, true, "POST", units, `{"name":"燃气事业部","parent_id":"root","tenant":"x"}`, 400, "invalid_request")
		decode(t, expect(t, admin, t1, true, "POST", units, `{"name":"燃气事业部","parent_id":"root"}`, 200), &gas)
		decode(t, expect(t, admin, t1, true, "POST", units, `{"name":"华东区","parent_id":"`+string(gas.ID)+`"}`, 200), &east)
		decode(t, expect(t, admin, t1, true, "POST", units, `{"name":"水务事业部","parent_id":"root"}`, 200), &water)
		if !strings.HasPrefix(string(gas.ID), "unt_") || len(east.Chain) != 3 || east.Chain[1] != gas.ID || east.Chain[2] != "root" || gas.Version != 1 {
			t.Fatalf("%+v %+v", gas, east)
		}
		var list identityapi.OrgUnitList
		decode(t, expect(t, bd, t1, false, "GET", units, "", 200), &list)
		if len(list.Units) != 4 || list.Units[0].ID != "root" || list.Units[0].Name != "甲" {
			t.Fatalf("%+v", list)
		}
		// 另一租户看不到、也读不到本租户的单元
		decode(t, expect(t, admin2, t2, false, "GET", units, "", 200), &list)
		if len(list.Units) != 1 {
			t.Fatalf("%+v", list)
		}
		expect(t, admin2, t2, false, "GET", units+"/"+string(gas.ID), "", 404, "org_unit_not_found")
		for name, d := range map[string]idapi.Directory{"本地": localDir, "远程": remoteDir} {
			u, err := d.OrgUnit(as(t1, "svc:detection", "service"), string(east.ID))
			if err != nil || len(u.Chain) != 3 {
				t.Fatalf("%s：%+v %v", name, u, err)
			}
			if _, err := d.OrgUnit(as(t2, "svc:detection", "service"), string(east.ID)); errCode(err) != "org_unit_not_found" {
				t.Fatalf("%s：跨租户读取单元应为 404：%v", name, err)
			}
		}
	})
	t.Run("组织单元：深度上限、环与根单元", func(t *testing.T) {
		parent := east.ID
		for i := 0; i < 5; i++ { // 链长 4..8
			var u identityapi.OrgUnit
			decode(t, expect(t, admin, t1, true, "POST", units, fmt.Sprintf(`{"name":"第%d层","parent_id":"%s"}`, i+4, parent), 200), &u)
			parent = u.ID
		}
		expect(t, admin, t1, true, "POST", units, `{"name":"第9层","parent_id":"`+string(parent)+`"}`, 422, "org_unit_too_deep")
		expect(t, admin, t1, true, "PUT", units+"/"+string(gas.ID), `{"name":"燃气事业部","parent_id":"`+string(east.ID)+`","expected_version":1}`, 409, "org_unit_cycle")
		expect(t, admin, t1, true, "PUT", units+"/"+string(water.ID), `{"name":"水务事业部","parent_id":"`+string(parent)+`","expected_version":1}`, 422, "org_unit_too_deep")
		expect(t, admin, t1, true, "PUT", units+"/root", `{"name":"甲","parent_id":"`+string(gas.ID)+`","expected_version":1}`, 422, "root_unit_fixed")
		var root identityapi.OrgUnit
		decode(t, expect(t, admin, t1, true, "PUT", units+"/root", `{"name":"甲集团","expected_version":1}`, 200), &root)
		if root.Version != 2 || root.Name != "甲集团" {
			t.Fatalf("%+v", root)
		}
		expect(t, admin, t1, true, "PUT", units+"/root", `{"name":"甲集团","expected_version":1}`, 409, "version_conflict")
	})

	t.Run("单元角色：成员命令与会话", func(t *testing.T) {
		put := func(subject, body string) identityapi.Session {
			var s identityapi.Session
			decode(t, expect(t, admin, t1, true, "PUT", "/v1/identity/members/"+subject, body, 200), &s)
			return s
		}
		expect(t, admin, t1, true, "PUT", "/v1/identity/members/"+dmGas, `{"state":"active","roles":[],"expected_version":0,"unit_roles":[{"org_unit_id":"root","roles":["admin"]}]}`, 400, "invalid_request")
		expect(t, admin, t1, true, "PUT", "/v1/identity/members/"+dmGas, `{"state":"active","roles":[],"expected_version":0,"unit_roles":[{"org_unit_id":"unt_01JA8ZXXXXXXXXXXXXXXXXXXX9","roles":["decision_maker"]}]}`, 422, "org_unit_not_found")
		s := put(dmGas, `{"state":"active","roles":[],"expected_version":0,"unit_roles":[{"org_unit_id":"`+string(gas.ID)+`","roles":["decision_maker"]}]}`)
		if s.MemberVersion != 1 || len(s.UnitRoles) != 1 || s.UnitRoles[0].OrgUnitID != gas.ID {
			t.Fatalf("%+v", s)
		}
		put(bdGas, `{"state":"active","roles":[],"expected_version":0,"unit_roles":[{"org_unit_id":"`+string(gas.ID)+`","roles":["business_developer"]}]}`)
		put(bdWater, `{"state":"active","roles":[],"expected_version":0,"unit_roles":[{"org_unit_id":"`+string(water.ID)+`","roles":["business_developer"]}]}`)
		put(raWater, `{"state":"active","roles":[],"expected_version":0,"unit_roles":[{"org_unit_id":"`+string(water.ID)+`","roles":["research_analyst"]}]}`)
		var session identityapi.Session
		decode(t, expect(t, dmGas, t1, false, "GET", "/v1/identity/session", "", 200), &session)
		if len(session.UnitRoles) != 1 || len(session.Roles) != 0 {
			t.Fatalf("%+v", session)
		}
		// 单元上的角色作用于本单元及下级，不作用于兄弟单元与租户整体的动作
		for _, a := range []idapi.Authorizer{localAuthz, remoteClient} {
			if !check(t, a, dmGas, "can_create_objective", "org_unit:"+string(east.ID)) || check(t, a, dmGas, "can_create_objective", "org_unit:root") ||
				check(t, a, dmGas, "can_create_objective", "org_unit:"+string(water.ID)) || check(t, a, dmGas, "can_view_usage", "tenant:"+t1) ||
				!check(t, a, dmGas, "can_view_profile", "tenant:"+t1) || !check(t, a, dm, "can_create_objective", "org_unit:"+string(east.ID)) {
				t.Fatal("单元角色范围错误")
			}
			// 另一租户的单元、未知资源类型均为拒绝
			if check(t, a, dm, "can_create_objective", "org_unit:unt_01JA8ZXXXXXXXXXXXXXXXXXXX9") || check(t, a, admin, "can_view", "knowledge_asset:k1") {
				t.Fatal("不属于本租户的资源应拒绝")
			}
		}
	})

	// 目标：由单元决策者在华东区（燃气事业部下级）启用，参与人为燃气与水务的业务开拓者
	const objectives = "/v1/customer-context/objectives"
	profile := `{"expected_version":0,"content":{"subject_type":"organization","entities":[],"value_chain_roles":["ROLE-MFR"],"industry_packs":["IND-BUILDING-MATERIALS"],
"offering_categories":["CAT-PIPE-PE"],"brands":[],"service_regions":["330000"],"qualifications":[]}}`
	expect(t, admin, t1, false, "PUT", "/v1/customer-context/profile", profile, 200)
	t.Run("档案：我方主体挂接本租户的非根单元，其他租户与不存在的单元拒绝", func(t *testing.T) {
		var other identityapi.OrgUnit
		decode(t, expect(t, admin2, t2, true, "POST", units, `{"name":"乙的事业部","parent_id":"root"}`, 200), &other)
		attach := func(unit string) string {
			body := strings.Replace(profile, `"expected_version":0`, `"expected_version":1`, 1)
			return strings.Replace(body, `"entities":[]`, `"entities":[{"org_id":"org_01JA8ZXXXXXXXXXXXXXXXXXX01","org_unit_id":"`+unit+`","qualifications_complete":false}]`, 1)
		}
		expect(t, admin, t1, false, "PUT", "/v1/customer-context/profile", attach(string(other.ID)), 422, "org_unit_not_found")
		expect(t, admin, t1, false, "PUT", "/v1/customer-context/profile", attach("unt_01JA8ZXXXXXXXXXXXXXXXXXXX9"), 422, "org_unit_not_found")
		raw := expect(t, admin, t1, false, "PUT", "/v1/customer-context/profile", attach(string(east.ID)), 200)
		if !strings.Contains(string(raw), `"org_unit_id":"`+string(east.ID)+`"`) {
			t.Fatalf("档案应保存非根单元：%s", raw)
		}
	})
	var objective cc.Objective
	objPath := ""
	t.Run("用例14：目标在单元上授权，参与人经 SyncResourceParticipants 可查看", func(t *testing.T) {
		enable := `{"template_id":"APP-TENDER.A02","org_unit_id":"` + string(east.ID) + `","participants":["` + bdGas + `","` + bdWater + `"]}`
		expect(t, dmGas, t1, false, "POST", objectives, strings.Replace(enable, string(east.ID), "root", 1), 403, "forbidden")
		expect(t, bdGas, t1, false, "POST", objectives, enable, 403, "forbidden")
		decode(t, expect(t, dmGas, t1, false, "POST", objectives, enable, 200), &objective)
		objPath = objectives + "/" + objective.ID
		if objective.OrgUnitID != string(east.ID) {
			t.Fatalf("%+v", objective)
		}
		// 参与人（同单元链上有范围访问）可查看目标与信号；兄弟单元的参与人被 identity 拒绝（无范围访问）
		expect(t, bdGas, t1, false, "GET", objPath, "", 200)
		expect(t, bdWater, t1, false, "GET", objPath, "", 404, "objective_not_found")
		// 同单元链（含上级继承）的决策者、研究分析者、管理员可查看；非参与业务开拓者、兄弟单元成员看不到
		for _, who := range []string{dmGas, dm, ra, admin} {
			expect(t, who, t1, false, "GET", objPath, "", 200)
		}
		for _, who := range []string{bd, raWater} {
			expect(t, who, t1, false, "GET", objPath, "", 404, "objective_not_found")
		}
		obj := "objective:" + objective.ID
		if !check(t, localAuthz, bdGas, "can_view_signals", obj) || check(t, localAuthz, admin, "can_view_signals", obj) ||
			!check(t, remoteClient, admin, "can_view", obj) || check(t, remoteClient, bdGas, "can_edit", obj) {
			t.Fatal("目标与信号权限与矩阵不一致：参与人可看信号，管理员看目标不自动获得信号权限")
		}
		lists := map[string]int{bdGas: 1, bdWater: 0, bd: 0, raWater: 0, dmGas: 1, ra: 1, admin: 1}
		for who, n := range lists {
			var list cc.ObjectiveList
			decode(t, expect(t, who, t1, false, "GET", objectives, "", 200), &list)
			if len(list.Objectives) != n {
				t.Fatalf("%s 列表 %d 个，期望 %d", who, len(list.Objectives), n)
			}
		}
		// 能查看而无权调整为 403，不能查看为 404
		configure := fmt.Sprintf(`{"expected_version":1,"owner":%q,"participants":[%q],"params":{}}`, dmGas, bdGas)
		expect(t, admin, t1, false, "PUT", objPath, configure, 403, "forbidden")
		expect(t, bd, t1, false, "PUT", objPath, configure, 404, "objective_not_found")
		// 跨租户：另一租户管理员（也是决策者）看不到
		expect(t, admin2, t2, false, "GET", objPath, "", 404, "objective_not_found")
	})

	t.Run("SyncResourceParticipants：版本、重放、冲突与服务授权", func(t *testing.T) {
		svc := as(t1, "svc:customer-context", "service")
		req := func(version int64, subjects ...string) *idapi.SyncParticipantsRequest {
			r := &idapi.SyncParticipantsRequest{Resource: "objective:" + objective.ID, OrgUnitID: east.ID, Version: version,
				Participants: []idapi.ParticipantRelation{{Subject: dmGas, Relation: "owner"}}}
			for _, s := range subjects {
				r.Participants = append(r.Participants, idapi.ParticipantRelation{Subject: s, Relation: "participant"})
			}
			return r
		}
		for name, d := range map[string]idapi.Directory{"本地": localDir, "远程": remoteDir} {
			res, err := d.SyncResourceParticipants(svc, req(1, bdGas, bdWater))
			if err != nil || res.Outcome != "replayed" || res.AppliedVersion != 1 || len(res.Rejected) != 1 ||
				res.Rejected[0].Subject != bdWater || res.Rejected[0].Reason != "no_scope_access" {
				t.Fatalf("%s：同一版本同一请求应返回首次结果：%+v %v", name, res, err)
			}
			if _, err := d.SyncResourceParticipants(svc, req(1, bdGas)); errCode(err) != "participants_version_conflict" {
				t.Fatalf("%s：%v", name, err)
			}
			for _, who := range []string{"svc:detection", "svc:pursuit"} {
				if _, err := d.SyncResourceParticipants(as(t1, who, "service"), req(9, bdGas)); errCode(err) != "forbidden" {
					t.Fatalf("%s：%s 不应能同步目标参与关系：%v", name, who, err)
				}
			}
			if _, err := d.SyncResourceParticipants(as(t1, dm, "user"), req(9, bdGas)); errCode(err) != "forbidden" {
				t.Fatalf("%s：用户主体不能直接同步：%v", name, err)
			}
			bad := req(9, bdGas)
			bad.Participants[1].Relation = "collaborator"
			if _, err := d.SyncResourceParticipants(svc, bad); errCode(err) != "invalid_request" {
				t.Fatalf("%s：目标不接受协作者关系：%v", name, err)
			}
			// 资源登记在租户甲，不能以租户乙的上下文改写
			other := req(9)
			other.OrgUnitID = "root"
			if _, err := d.SyncResourceParticipants(as(t2, "svc:customer-context", "service"), other); errCode(err) != "resource_conflict" {
				t.Fatalf("%s：%v", name, err)
			}
		}
		// 更高版本：加入未登记主体与退出成员；被拒成员列出，其余照常应用，多余关系删除
		res, err := localDir.SyncResourceParticipants(svc, req(5, bdGas, "nobody-"+suffix))
		if err != nil || res.Outcome != "applied" || len(res.Rejected) != 1 || res.Rejected[0].Reason != "not_member" {
			t.Fatalf("%+v %v", res, err)
		}
		res, err = remoteDir.SyncResourceParticipants(svc, req(3))
		if err != nil || res.Outcome != "stale" || res.AppliedVersion != 5 {
			t.Fatalf("低于已应用版本应为 stale：%+v %v", res, err)
		}
		if !check(t, localAuthz, bdGas, "can_view", "objective:"+objective.ID) {
			t.Fatal("stale 不得改变已应用的关系")
		}
	})

	t.Run("成员退出：参与关系失效并清理，重新加入不复活", func(t *testing.T) {
		var s identityapi.Session
		decode(t, expect(t, admin, t1, true, "GET", "/v1/identity/members/"+bdGas, "", 200), &s)
		expect(t, admin, t1, true, "PUT", "/v1/identity/members/"+bdGas, fmt.Sprintf(`{"state":"removed","roles":[],"expected_version":%d}`, s.MemberVersion), 200)
		expect(t, bdGas, t1, false, "GET", objPath, "", 403)
		res, err := localDir.SyncResourceParticipants(as(t1, "svc:customer-context", "service"), &idapi.SyncParticipantsRequest{Resource: "objective:" + objective.ID,
			OrgUnitID: east.ID, Version: 6, Participants: []idapi.ParticipantRelation{{Subject: bdGas, Relation: "participant"}}})
		if err != nil || len(res.Rejected) != 1 || res.Rejected[0].Reason != "member_inactive" {
			t.Fatalf("%+v %v", res, err)
		}
		expect(t, admin, t1, true, "PUT", "/v1/identity/members/"+bdGas, fmt.Sprintf(`{"state":"active","roles":[],"expected_version":%d,"unit_roles":[{"org_unit_id":"%s","roles":["business_developer"]}]}`, s.MemberVersion+1, gas.ID), 200)
		expect(t, bdGas, t1, false, "GET", objPath, "", 404, "objective_not_found")
	})

	t.Run("参与关系同步失败后由 objective.changed 消费者重试", func(t *testing.T) {
		// 模拟命令已提交而提交后的同步丢失：直接改写目标版本与参与人，此时 identity 仍是旧版本
		o, err := pgx.Connect(ctx, database.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer o.Close(ctx)
		if _, err := o.Exec(ctx, `UPDATE ctx_objectives SET participants = ARRAY[$2], version = 7 WHERE id = $1`, objective.ID, bdGas); err != nil {
			t.Fatal(err)
		}
		expect(t, bdGas, t1, false, "GET", objPath, "", 404, "objective_not_found")
		consumers := module.Consumers(env)
		if len(consumers) != 1 || consumers[0].Topics[0] != "cell-cell-01.customer_context" {
			t.Fatalf("%+v", consumers)
		}
		handler := consumers[0].Handlers["customer_context.objective.changed.v1"]
		data, _ := json.Marshal(map[string]any{"objective_id": objective.ID, "version": 1})
		ectx := reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:customer-context", Kind: "service"}, Purpose: "event"})
		if err := appDB.Tx(ectx, func(tx pgx.Tx) error {
			return handler(ectx, tx, &outbox.Envelope{Type: "customer_context.objective.changed.v1", Tenant: t1, Cell: "cell-01", Data: data})
		}); err != nil {
			t.Fatal(err)
		}
		expect(t, bdGas, t1, false, "GET", objPath, "", 200)
	})

	t.Run("移动单元立即改变继承范围", func(t *testing.T) {
		var moved identityapi.OrgUnit
		decode(t, expect(t, admin, t1, true, "PUT", units+"/"+string(east.ID), fmt.Sprintf(`{"name":"华东区","parent_id":%q,"expected_version":1}`, water.ID), 200), &moved)
		if len(moved.Chain) != 3 || moved.Chain[1] != water.ID {
			t.Fatalf("%+v", moved)
		}
		expect(t, raWater, t1, false, "GET", objPath, "", 200)
		expect(t, dmGas, t1, false, "GET", objPath, "", 404, "objective_not_found")
		expect(t, bdGas, t1, false, "GET", objPath, "", 404, "objective_not_found") // 燃气的业务开拓者失去范围访问
		decode(t, expect(t, admin, t1, true, "PUT", units+"/"+string(east.ID), fmt.Sprintf(`{"name":"华东区","parent_id":%q,"expected_version":2}`, gas.ID), 200), &moved)
		expect(t, dmGas, t1, false, "GET", objPath, "", 200)
	})

	t.Run("服务主体读取快照：identity 的 can_read_context，单元快照带单元链", func(t *testing.T) {
		for _, who := range []string{"svc:detection", "svc:learning", "svc:research", "svc:assessment"} {
			if _, err := reader.ContextVersion(as(t1, who, "service")); err != nil {
				t.Fatalf("%s：%v", who, err)
			}
		}
		for _, who := range []string{"svc:channels", "svc:customer-context", "svc:pursuit"} {
			if _, err := reader.ContextVersion(as(t1, who, "service")); err == nil || !strings.Contains(err.Error(), "forbidden") {
				t.Fatalf("%s 不是登记的读取方：%v", who, err)
			}
		}
		ok, err := localAuthz.Check(as(t1, "svc:detection", "service"), authz.Request{Relation: "can_read_context", Object: "tenant:" + t2})
		if err != nil || ok {
			t.Fatalf("服务主体只能对上下文租户判断：%v %v", ok, err)
		}
		s, err := reader.ContextSnapshot(as(t1, "svc:detection", "service"), string(east.ID), 0)
		if err != nil || s.OrgUnitID != string(east.ID) || len(s.UnitChain) != 3 || s.UnitChain[2] != "root" {
			t.Fatalf("%+v %v", s, err)
		}
		again, err := reader.ContextSnapshot(as(t1, "svc:detection", "service"), string(east.ID), s.ContextVersion)
		if err != nil || strings.Join(again.UnitChain, "/") != strings.Join(s.UnitChain, "/") {
			t.Fatalf("%+v %v", again, err)
		}
		if _, err := reader.ContextSnapshot(as(t2, "svc:detection", "service"), string(east.ID), 0); err == nil || !strings.Contains(err.Error(), "org_unit_not_found") {
			t.Fatalf("另一租户的单元：%v", err)
		}
		// 停用的租户：服务主体的决策失败关闭
		o, err := pgx.Connect(ctx, iam.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer o.Close(ctx)
		if _, err := o.Exec(ctx, `UPDATE identity_tenants SET state='suspended' WHERE id=$1`, t2); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ContextVersion(as(t2, "svc:detection", "service")); err == nil {
			t.Fatal("停用租户的服务读取应失败")
		}
	})

	retry := func() error {
		handler := module.Consumers(env)[0].Handlers["customer_context.objective.changed.v1"]
		data, _ := json.Marshal(map[string]any{"objective_id": objective.ID})
		ectx := reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:customer-context", Kind: "service"}, Purpose: "event"})
		return appDB.Tx(ectx, func(tx pgx.Tx) error {
			return handler(ectx, tx, &outbox.Envelope{Type: "customer_context.objective.changed.v1", Tenant: t1, Cell: "cell-01", Data: data})
		})
	}
	configure := func(version int64, owner string, participants []string) string {
		b, _ := json.Marshal(map[string]any{"expected_version": version, "owner": owner, "participants": participants, "params": map[string]any{}})
		return string(b)
	}
	listed := func(t *testing.T, who string) int {
		var list cc.ObjectiveList
		decode(t, expect(t, who, t1, false, "GET", objectives, "", 200), &list)
		return len(list.Objectives)
	}

	t.Run("用例14：移除参与人、改派负责人在同步失败时立即撤权，新主体同步应用后才可见", func(t *testing.T) {
		bdGas2, bdGas3 := user("bd-gas2"), user("bd-gas3")
		for _, who := range []string{bdGas2, bdGas3} {
			expect(t, admin, t1, true, "PUT", "/v1/identity/members/"+who, `{"state":"active","roles":[],"expected_version":0,"unit_roles":[{"org_unit_id":"`+string(gas.ID)+`","roles":["business_developer"]}]}`, 200)
		}
		var o cc.Objective
		decode(t, expect(t, dmGas, t1, false, "GET", objPath, "", 200), &o)
		decode(t, expect(t, dmGas, t1, false, "PUT", objPath, configure(o.Version, bdGas, []string{bdGas2}), 200), &o)
		for _, who := range []string{bdGas, bdGas2} {
			expect(t, who, t1, false, "GET", objPath, "", 200)
		}
		expect(t, bdGas3, t1, false, "GET", objPath, "", 404, "objective_not_found")

		// identity 不可用：改派负责人（bdGas → dmGas）、移除参与人 bdGas2、加入 bdGas3；命令照常提交，提交后的同步失败
		flaky.fail.Store(true)
		defer flaky.fail.Store(false)
		decode(t, expect(t, dmGas, t1, false, "PUT", objPath, configure(o.Version, dmGas, []string{bdGas3}), 200), &o)
		if err := retry(); err == nil {
			t.Fatal("identity 不可用时事件重试应失败")
		}
		obj := "objective:" + objective.ID
		if !check(t, localAuthz, bdGas, "can_view", obj) || !check(t, localAuthz, bdGas2, "can_view", obj) || check(t, localAuthz, bdGas3, "can_view", obj) {
			t.Fatal("同步失败时 identity 中应仍是旧版本的参与关系")
		}
		// 旧负责人、被移除的参与人直接读取已知目标被拒（与不存在相同），列表也不再列出；新参与人同步前不可见
		for _, who := range []string{bdGas, bdGas2, bdGas3} {
			expect(t, who, t1, false, "GET", objPath, "", 404, "objective_not_found")
			if n := listed(t, who); n != 0 {
				t.Fatalf("%s 列表 %d 个，期望 0", who, n)
			}
		}
		// 单元权限不受参与关系同步影响
		expect(t, dmGas, t1, false, "GET", objPath, "", 200)
		expect(t, ra, t1, false, "GET", objPath, "", 200)

		flaky.fail.Store(false)
		if err := retry(); err != nil {
			t.Fatal(err)
		}
		expect(t, bdGas3, t1, false, "GET", objPath, "", 200)
		if n := listed(t, bdGas3); n != 1 {
			t.Fatalf("新参与人同步后列表 %d 个，期望 1", n)
		}
		for _, who := range []string{bdGas, bdGas2} {
			expect(t, who, t1, false, "GET", objPath, "", 404, "objective_not_found")
			if check(t, localAuthz, who, "can_view", obj) {
				t.Fatalf("%s 的旧参与关系应在同步后删除", who)
			}
		}
	})

	t.Run("参与人上限：负责人与参与人合计不同主体至多 50，提交前校验", func(t *testing.T) {
		people := func(n int) []string {
			out := make([]string, n)
			for i := range out {
				out[i] = fmt.Sprintf("cap-%02d-%s", i, suffix)
			}
			return out
		}
		iamOwner, err := pgx.Connect(ctx, iam.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer iamOwner.Close(ctx)
		ctxOwner, err := pgx.Connect(ctx, database.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer ctxOwner.Close(ctx)
		state := func(t *testing.T) (version, events, applied int64) {
			t.Helper()
			if err := ctxOwner.QueryRow(ctx, `SELECT version, (SELECT count(*) FROM kernel_outbox WHERE envelope->'data'->>'objective_id' = $1) FROM ctx_objectives WHERE id = $1`,
				objective.ID).Scan(&version, &events); err != nil {
				t.Fatal(err)
			}
			if err := iamOwner.QueryRow(ctx, `SELECT applied_version FROM identity_resources WHERE object = $1`, "objective:"+objective.ID).Scan(&applied); err != nil {
				t.Fatal(err)
			}
			return
		}
		var o cc.Objective
		decode(t, expect(t, dmGas, t1, false, "GET", objPath, "", 200), &o)
		version, events, _ := state(t)
		// 50 名参与人且不含负责人：51 个关系，提交前拒绝；51 名参与人由契约拒绝。均不入库、不产生事件
		expect(t, dmGas, t1, false, "PUT", objPath, configure(o.Version, dmGas, people(50)), 422, "too_many_participants")
		expect(t, dmGas, t1, false, "PUT", objPath, configure(o.Version, dmGas, people(51)), 400)
		enable := fmt.Sprintf(`{"template_id":"APP-TENDER.A02","org_unit_id":%q,"participants":%s}`, water.ID, func() string { b, _ := json.Marshal(people(50)); return string(b) }())
		expect(t, dm, t1, false, "POST", objectives, enable, 422, "too_many_participants")
		if v, e, _ := state(t); v != version || e != events {
			t.Fatalf("超限命令不得提交：版本 %d→%d，事件 %d→%d", version, v, events, e)
		}
		// 边界：49 名参与人 + 负责人 = 50 个关系，提交并同步应用
		decode(t, expect(t, dmGas, t1, false, "PUT", objPath, configure(o.Version, dmGas, people(49)), 200), &o)
		if _, _, applied := state(t); applied != o.Version {
			t.Fatalf("49 + 负责人应同步应用：已应用 %d，目标版本 %d", applied, o.Version)
		}
		// 负责人同时是参与人只计一次：50 名参与人（含负责人）= 50 个关系
		decode(t, expect(t, dmGas, t1, false, "PUT", objPath, configure(o.Version, dmGas, append(people(49), dmGas)), 200), &o)
		if _, _, applied := state(t); applied != o.Version {
			t.Fatalf("负责人兼参与人应同步应用：已应用 %d，目标版本 %d", applied, o.Version)
		}
	})
}
