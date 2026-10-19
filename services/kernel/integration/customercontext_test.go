package integration_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	ctxapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api/apitest"
	ctxremote "github.com/superailabs/miaoqia/services/subsystems/customercontext/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// PRD 第2段服务端切片：真实 PostgreSQL（行级安全、应用账号）+ 真实 identity 目录与 OpenFGA 授权 + 外部网关令牌校验 +
// 生成的 HTTP 服务端 + mTLS 内部端口与远程绑定。用例编号对应 PRD 第12节。
func TestCustomerContextProfileObjectivesAndSnapshot(t *testing.T) {
	fga := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if fga == "" || os.Getenv("MIAOQIA_TEST_PG") == "" {
		// CI（GitHub Actions 设置 CI=true）提供真实 PostgreSQL 与 OpenFGA（.github/workflows/services.yml），缺少时失败而不是静默跳过。
		if os.Getenv("CI") != "" {
			t.Fatal("CI 中必须设置 MIAOQIA_TEST_PG 与 MIAOQIA_TEST_OPENFGA")
		}
		t.Skip("MIAOQIA_TEST_PG 或 MIAOQIA_TEST_OPENFGA 未设置")
	}
	t.Setenv("MIAOQIA_ENV", "production")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// identity 目录（独立数据库）：两个租户与各角色成员，直接写入目录表作为夹具。
	iam := testenv.NewDatabase(t)
	idModule := identity.Module()
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", idModule.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2, t3 := ids.New("tnt"), ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	user := func(role string) string { return role + "-" + suffix }
	admin, expert, dm, bd, ra := user("admin"), user("expert"), user("dm"), user("bd"), user("ra")
	admin2, admin3 := user("admin2"), user("admin3")
	owner, err := pgx.Connect(ctx, iam.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO identity_tenants VALUES ($1,'甲','cell-01','active',1), ($2,'乙','cell-01','active',1), ($3,'丙','cell-01','active',1)`, []any{t1, t2, t3}},
		{`INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['domain_expert'],1), ($1,$4,'active',ARRAY['decision_maker'],1),
 ($1,$5,'active',ARRAY['business_developer'],1), ($1,$6,'active',ARRAY['research_analyst'],1), ($7,$8,'active',ARRAY['admin','domain_expert','decision_maker'],1),
 ($9,$10,'active',ARRAY['admin','domain_expert','decision_maker'],1)`,
			[]any{t1, admin, expert, dm, bd, ra, t2, admin2, t3, admin3}},
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
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := idModule.Start(ctx, &runtime.Env{DB: iamApp, Bindings: bindings, Log: quiet, Config: func(string) string { return fga }}); err != nil {
		t.Fatal(err)
	}

	// customer-context（独立数据库）。迁移重复执行无副作用。
	database := testenv.NewDatabase(t)
	module := customercontext.Module()
	for range 2 {
		if err := db.Migrate(ctx, database.OwnerDSN, database.Name+"_app", module.Migrations); err != nil {
			t.Fatal(err)
		}
	}
	appDB, err := db.Open(ctx, database.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer appDB.Close()
	packs, _ := filepath.Abs(filepath.Join("..", "..", "..", "packs"))
	// customer-context 的运行日志被捕获，用于核对快照超出规模上界的告警。
	logs := &syncBuffer{}
	env := &runtime.Env{DB: appDB, Bindings: bindings, Log: slog.New(slog.NewJSONHandler(logs, nil)), Config: func(k string) string {
		if k == "PACKS_DIR" {
			return packs
		}
		return ""
	}}
	if err := module.Start(ctx, env); err != nil {
		t.Fatal(err)
	}
	public, internal := http.NewServeMux(), http.NewServeMux()
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

	do := func(t *testing.T, subject, tenant, method, path, body, key string) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+issuer.Token(t, subject, tenant, "cell-01", false))
		r.Header.Set("Content-Type", "application/json")
		if key == "" {
			key = fmt.Sprintf("%x", sha256.Sum256([]byte(subject+method+path+body)))[:40]
		}
		r.Header.Set("Idempotency-Key", key)
		// 外部请求中的内部上下文头不得被采信。
		r.Header.Set(reqctx.Header, reqctx.Encode(reqctx.Context{Tenant: t2, Cell: "cell-01", Principal: reqctx.Principal{ID: admin2, Kind: "user"}}))
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	expect := func(t *testing.T, subject, tenant, method, path, body string, status int, key ...string) []byte {
		t.Helper()
		k := ""
		if len(key) > 0 {
			k = key[0]
		}
		code, raw := do(t, subject, tenant, method, path, body, k)
		if code != status {
			t.Fatalf("%s %s %s：得到 %d，期望 %d：%s", subject, method, path, code, status, raw)
		}
		return raw
	}
	problem := func(t *testing.T, raw []byte, want string) {
		t.Helper()
		var p cc.Problem
		if err := json.Unmarshal(raw, &p); err != nil || p.Code.Or("") != want {
			t.Fatalf("期望错误码 %s，得到 %s", want, raw)
		}
	}
	decode := func(t *testing.T, raw []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}
	readerCtx := func(tenant, principal, kind string) context.Context {
		return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: principal, Kind: kind}})
	}
	local, ok := runtime.Lookup[ctxapi.ContextReader](bindings, ctxapi.ContextBinding)
	if !ok {
		t.Fatal("缺少本地上下文读取绑定")
	}
	const org1, org2 = "org_01JA8ZXXXXXXXXXXXXXXXXXXX1", "org_01JA8ZXXXXXXXXXXXXXXXXXXX2"
	content := func(entities, quals, categories string) string {
		return fmt.Sprintf(`{"subject_type":"organization","entities":%s,"value_chain_roles":["ROLE-MFR"],"industry_packs":["IND-BUILDING-MATERIALS"],
"offering_categories":%s,"brands":["示例品牌"],"service_regions":["330000"],"qualifications":%s}`, entities, categories, quals)
	}
	put := func(expected int, c string) string {
		return fmt.Sprintf(`{"expected_version":%d,"content":%s}`, expected, c)
	}
	const profilePath = "/v1/customer-context/profile"

	t.Run("从未保存的档案为版本0", func(t *testing.T) {
		var p cc.Profile
		decode(t, expect(t, bd, t1, "GET", profilePath, "", 200), &p)
		if p.Version != 0 || len(p.Content.Entities) != 0 {
			t.Fatalf("%+v", p)
		}
		v, err := local.ContextVersion(readerCtx(t1, "svc:detection", "service"))
		if err != nil || v.ContextVersion != 0 {
			t.Fatalf("%+v %v", v, err)
		}
		if _, err := local.ContextSnapshot(readerCtx(t1, "svc:detection", "service"), ctxapi.RootUnit, 0); !isCode(err, "context_not_initialized") {
			t.Fatalf("%v", err)
		}
	})

	unlinked := content(`[]`, `[]`, `["CAT-PIPE-PE"]`)
	t.Run("用例10：无 can_edit_profile 的成员只读", func(t *testing.T) {
		for _, who := range []string{dm, bd, ra} {
			problem(t, expect(t, who, t1, "PUT", profilePath, put(0, unlinked), 403), "forbidden")
		}
	})
	t.Run("档案校验与协议错误", func(t *testing.T) {
		problem(t, expect(t, expert, t1, "PUT", profilePath, put(0, content(`[]`, `[]`, `["CAT-VALVE"]`)), 422), "unknown_category")
		problem(t, expect(t, expert, t1, "PUT", profilePath, put(0, strings.Replace(unlinked, "IND-BUILDING-MATERIALS", "IND-IT", 1)), 422), "industry_not_available")
		problem(t, expect(t, expert, t1, "PUT", profilePath, put(0, content(`[]`, `[{"org_id":"`+org1+`","code":"Q-BM-ISO9001","certificate_no":"1","valid_to":"2030-01-01"}]`, `[]`)), 422), "qualification_entity_unknown")
		problem(t, expect(t, expert, t1, "PUT", profilePath, `{"expected_version":0,"content":{"subject_type":"organization","tenant":"x"}}`, 400), "invalid_request")
		problem(t, expect(t, expert, t1, "PUT", profilePath, put(0, unlinked), 400, "short"), "invalid_request")
	})

	var v1 cc.Profile
	t.Run("用例2：未关联本企业时快照不含我方主体", func(t *testing.T) {
		decode(t, expect(t, expert, t1, "PUT", profilePath, put(0, unlinked), 200, "profile-save-0001"), &v1)
		if v1.Version != 1 || v1.ContextVersion != 1 {
			t.Fatalf("%+v", v1)
		}
		s, err := local.ContextSnapshot(readerCtx(t1, "svc:detection", "service"), ctxapi.RootUnit, 0)
		if err != nil {
			t.Fatal(err)
		}
		if s.PolicyView.OrgIds != nil || s.PolicyView.Entities.Set || s.TenantID != t1 || s.ContextVersion != 1 || s.UnitChain[0] != ctxapi.RootUnit {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("幂等：同键重放返回首次结果，同键不同请求冲突", func(t *testing.T) {
		var again cc.Profile
		decode(t, expect(t, expert, t1, "PUT", profilePath, put(0, unlinked), 200, "profile-save-0001"), &again)
		if again.Version != 1 {
			t.Fatalf("%+v", again)
		}
		problem(t, expect(t, expert, t1, "PUT", profilePath, put(0, content(`[]`, `[]`, `["CAT-PIPE"]`)), 409, "profile-save-0001"), "idempotency_conflict")
	})

	t.Run("用例5：并发编辑后提交者收到版本冲突", func(t *testing.T) {
		var wg sync.WaitGroup
		codes := make(chan int, 2)
		for i, c := range []string{`["CAT-PIPE-PE"]`, `["CAT-PIPE-PVC"]`} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, _ := do(t, expert, t1, "PUT", profilePath, put(1, content(`[]`, `[]`, c)), fmt.Sprintf("concurrent-edit-%04d", i))
				codes <- code
			}()
		}
		wg.Wait()
		close(codes)
		got := map[int]int{}
		for c := range codes {
			got[c]++
		}
		if got[200] != 1 || got[409] != 1 {
			t.Fatalf("%v", got)
		}
	})

	var current cc.Profile
	decode(t, expect(t, bd, t1, "GET", profilePath, "", 200), &current)
	quals := `[{"org_id":"` + org1 + `","code":"Q-BM-ISO9001","certificate_no":"Q1","valid_to":"2099-12-31"},
{"org_id":"` + org2 + `","code":"Q-BM-SAFETY-PRODUCTION","certificate_no":"S1","valid_to":"2020-01-01"}]`
	linked := content(`[{"org_id":"`+org1+`","qualifications_complete":true},{"org_id":"`+org2+`","org_unit_id":"root","qualifications_complete":false}]`, quals, `["CAT-PIPE-PE"]`)
	var v3 cc.Profile
	t.Run("用例4、6、20：关联两个主体与资质，保存返回后立即读到新版本", func(t *testing.T) {
		decode(t, expect(t, admin, t1, "PUT", profilePath, put(int(current.Version), linked), 200), &v3)
		v, err := local.ContextVersion(readerCtx(t1, "svc:detection", "service"))
		if err != nil || v.ContextVersion != v3.ContextVersion || v.ProfileVersion != v3.Version {
			t.Fatalf("%+v %v（期望 %d）", v, err, v3.ContextVersion)
		}
		s, err := local.ContextSnapshot(readerCtx(t1, "svc:detection", "service"), ctxapi.RootUnit, 0)
		if err != nil {
			t.Fatal(err)
		}
		e, _ := s.PolicyView.Entities.Get()
		if len(s.PolicyView.OrgIds) != 2 || e[org1].Qualifications["Q-BM-ISO9001"] != "valid" || !e[org1].QualificationsComplete ||
			e[org2].Qualifications["Q-BM-SAFETY-PRODUCTION"] != "expired" || e[org2].QualificationsComplete || len(e[org2].QualificationsPending) != 0 {
			t.Fatalf("%+v", s.PolicyView)
		}
		if _, ok := e[org1].Qualifications["Q-BM-SAFETY-PRODUCTION"]; ok {
			t.Fatal("未录入的资质不得出现")
		}
		old, err := local.ContextSnapshot(readerCtx(t1, "svc:detection", "service"), ctxapi.RootUnit, 1)
		if err != nil || old.PolicyView.OrgIds != nil || old.ContextVersion != 1 {
			t.Fatalf("旧版本快照应不变：%+v %v", old, err)
		}
	})

	t.Run("资质有效期跨日后读取版本时形成新版本", func(t *testing.T) {
		// 模拟“快照形成于有效期内、此后跨过到期日”：以 owner 账号把当前快照中的过期资质改回有效，并把检查日期提前。
		o, err := pgx.Connect(ctx, database.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer o.Close(ctx)
		if _, err := o.Exec(ctx, `UPDATE ctx_snapshots SET policy_view = jsonb_set(policy_view, '{entities,`+org2+`,qualifications,Q-BM-SAFETY-PRODUCTION}', '"valid"') WHERE tenant_id = $1 AND context_version = $2`, t1, v3.ContextVersion); err != nil {
			t.Fatal(err)
		}
		if _, err := o.Exec(ctx, `UPDATE ctx_contexts SET refresh_after = current_date - 1 WHERE tenant_id = $1`, t1); err != nil {
			t.Fatal(err)
		}
		v, err := local.ContextVersion(readerCtx(t1, "svc:detection", "service"))
		if err != nil || v.ContextVersion != v3.ContextVersion+1 || v.ProfileVersion != v3.Version {
			t.Fatalf("%+v %v", v, err)
		}
		s, _ := local.ContextSnapshot(readerCtx(t1, "svc:detection", "service"), ctxapi.RootUnit, v.ContextVersion)
		if e, _ := s.PolicyView.Entities.Get(); e[org2].Qualifications["Q-BM-SAFETY-PRODUCTION"] != "expired" {
			t.Fatalf("%+v", s.PolicyView)
		}
		again, _ := local.ContextVersion(readerCtx(t1, "svc:detection", "service"))
		if again.ContextVersion != v.ContextVersion {
			t.Fatal("同一天重复读取不应再次递增")
		}
		var count int
		if err := o.QueryRow(ctx, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' = 'customer_context.profile.updated.v1' AND envelope->'data'->>'cause' = 'qualification_validity'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("刷新事件 %d %v", count, err)
		}
	})

	t.Run("档案与快照不可变：应用账号不能修改或删除", func(t *testing.T) {
		for _, stmt := range []string{`UPDATE ctx_snapshots SET as_of = as_of`, `DELETE FROM ctx_profile_versions`, `DELETE FROM ctx_objectives`} {
			err := appDB.Tx(readerCtx(t1, admin, "user"), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt); return err })
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("%s：%v", stmt, err)
			}
		}
	})

	const objectives = "/v1/customer-context/objectives"
	var objective cc.Objective
	t.Run("目标目录按角色列出", func(t *testing.T) {
		var list cc.ObjectiveTemplateList
		decode(t, expect(t, bd, t1, "GET", "/v1/customer-context/objective-templates", "", 200), &list)
		found := map[string]cc.ObjectiveTemplate{}
		for _, tpl := range list.Templates {
			found[tpl.TemplateID] = tpl
		}
		if !found["APP-TENDER.A02"].Enableable || found["APP-TENDER.C01"].Enableable || found["APP-TENDER.C01"].UnavailableReason.Or("") != "not_released" {
			t.Fatalf("%+v", list)
		}
	})
	enable := `{"template_id":"APP-TENDER.A02","org_unit_id":"root","params":{"categories":{"source":"objective","values":["CAT-PIPE-PE-GAS"]}}}`
	t.Run("用例10、11：非决策者不能启用；启用后为未激活", func(t *testing.T) {
		for _, who := range []string{expert, admin, bd, ra} {
			problem(t, expect(t, who, t1, "POST", objectives, enable, 403), "forbidden")
		}
		problem(t, expect(t, dm, t1, "POST", objectives, strings.Replace(enable, "CAT-PIPE-PE-GAS", "CAT-PIPE-PVC", 1), 422), "param_not_subset")
		problem(t, expect(t, dm, t1, "POST", objectives, strings.Replace(enable, `"root"`, `"unt_01JA8ZXXXXXXXXXXXXXXXXXXX9"`, 1), 404), "org_unit_not_found")
		problem(t, expect(t, dm, t1, "POST", objectives, strings.Replace(enable, `"root"`, `"unit-gas"`, 1), 400), "invalid_request")
		problem(t, expect(t, dm, t1, "POST", objectives, strings.Replace(enable, "A02", "C01", 1), 422), "objective_not_enableable")
		decode(t, expect(t, dm, t1, "POST", objectives, enable, 200, "enable-a02-000001"), &objective)
		if objective.State != "inactive" || objective.Version != 1 || string(objective.Owner) != dm || objective.Params["regions"].Source != "profile" ||
			objective.Params["large_amount_cny"].Source != "default" || objective.PolicyTemplateID != "TPL-TENDER-A02-SUPPLY" {
			t.Fatalf("%+v", objective)
		}
		var again cc.Objective
		decode(t, expect(t, dm, t1, "POST", objectives, enable, 200, "enable-a02-000001"), &again)
		if again.ID != objective.ID {
			t.Fatal("同键重放应返回同一目标")
		}
	})
	t.Run("用例13：同一单元重复启用被拒绝并指向已有目标", func(t *testing.T) {
		raw := expect(t, dm, t1, "POST", objectives, enable, 409, "enable-a02-000002")
		var p cc.Problem
		decode(t, raw, &p)
		if p.Code.Or("") != "objective_exists" || p.Detail.Or("") != objective.ID {
			t.Fatalf("%s", raw)
		}
	})
	objPath := objectives + "/" + objective.ID
	t.Run("用例14：单元级角色可查看，非参与业务开拓者看不到", func(t *testing.T) {
		for _, who := range []string{dm, ra, admin} {
			expect(t, who, t1, "GET", objPath, "", 200)
		}
		problem(t, expect(t, bd, t1, "GET", objPath, "", 404), "objective_not_found")
		var list cc.ObjectiveList
		decode(t, expect(t, bd, t1, "GET", objectives, "", 200), &list)
		if len(list.Objectives) != 0 {
			t.Fatal("业务开拓者不应看到目标")
		}
		decode(t, expect(t, ra, t1, "GET", objectives, "", 200), &list)
		if len(list.Objectives) != 1 {
			t.Fatalf("%+v", list)
		}
	})
	t.Run("调整与停用、重新启用", func(t *testing.T) {
		configure := fmt.Sprintf(`{"expected_version":1,"owner":%q,"participants":[%q],"params":{"categories":{"source":"objective","values":["CAT-PIPE-PE-GAS"]},"min_amount_cny":{"source":"objective","number":1000000}}}`, dm, bd)
		problem(t, expect(t, admin, t1, "PUT", objPath, configure, 403), "forbidden")
		problem(t, expect(t, expert, t1, "PUT", objPath, configure, 404), "objective_not_found")
		problem(t, expect(t, bd, t1, "PUT", objPath, configure, 404), "objective_not_found")
		var o cc.Objective
		decode(t, expect(t, dm, t1, "PUT", objPath, configure, 200), &o)
		if o.Version != 2 || len(o.Participants) != 1 || o.Params["min_amount_cny"].Number.Or(0) != 1000000 {
			t.Fatalf("%+v", o)
		}
		problem(t, expect(t, dm, t1, "PUT", objPath, strings.Replace(configure, `"number":1000000`, `"number":5`, 1), 409), "version_conflict")
		// 加为参与人后，业务开拓者仅凭参与关系可查看（◐，经 identity 的 SyncResourceParticipants），列表中也可见
		expect(t, bd, t1, "GET", objPath, "", 200)
		var list cc.ObjectiveList
		decode(t, expect(t, bd, t1, "GET", objectives, "", 200), &list)
		if len(list.Objectives) != 1 {
			t.Fatalf("参与人应在列表中看到目标：%+v", list)
		}
		decode(t, expect(t, dm, t1, "PUT", objPath+"/state", `{"expected_version":2,"state":"disabled"}`, 200), &o)
		if o.State != "disabled" || o.Version != 3 {
			t.Fatalf("%+v", o)
		}
		problem(t, expect(t, dm, t1, "POST", objectives, enable, 409, "enable-a02-000003"), "objective_exists")
		problem(t, expect(t, dm, t1, "PUT", objPath+"/state", `{"expected_version":3,"state":"active"}`, 400), "invalid_request")
		decode(t, expect(t, dm, t1, "PUT", objPath+"/state", `{"expected_version":3,"state":"inactive"}`, 200), &o)
		if o.State != "inactive" || o.Version != 4 {
			t.Fatalf("%+v", o)
		}
	})

	t.Run("用例17：跨租户访问档案、目标与快照全部失败", func(t *testing.T) {
		var p cc.Profile
		decode(t, expect(t, admin2, t2, "GET", profilePath, "", 200), &p)
		if p.Version != 0 {
			t.Fatal("另一租户读到了本租户档案")
		}
		problem(t, expect(t, admin2, t2, "GET", objPath, "", 404), "objective_not_found")
		problem(t, expect(t, admin2, t2, "PUT", objPath+"/state", `{"expected_version":4,"state":"disabled"}`, 404), "objective_not_found")
		var list cc.ObjectiveList
		decode(t, expect(t, admin2, t2, "GET", objectives, "", 200), &list)
		if len(list.Objectives) != 0 {
			t.Fatal("另一租户看到了本租户目标")
		}
		if _, err := local.ContextSnapshot(readerCtx(t2, "svc:detection", "service"), ctxapi.RootUnit, 1); !isCode(err, "context_not_initialized") {
			t.Fatalf("%v", err)
		}
		// 本租户成员令牌声明另一租户：网关按 identity 目录拒绝。
		code, _ := do(t, admin, t2, "GET", profilePath, "", "")
		if code != 403 {
			t.Fatalf("得到 %d", code)
		}
		// 行级安全：即使绕过应用层，按另一租户上下文也读不到本租户的行。
		var n int
		err := appDB.Tx(readerCtx(t2, admin2, "user"), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM ctx_objectives) + (SELECT count(*) FROM ctx_snapshots) + (SELECT count(*) FROM ctx_profile_versions) + (SELECT count(*) FROM ctx_commands)`).Scan(&n)
		})
		if err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
	})

	// 另一租户也保存档案，供契约用例核对跨租户隔离。
	expect(t, admin2, t2, "PUT", profilePath, put(0, content(`[]`, `[]`, `["CAT-PIPE"]`)), 200)
	t.Run("上下文读取接口：本地与远程绑定同一契约", func(t *testing.T) {
		reader := readerCtx(t1, "svc:detection", "service")
		cur, err := local.ContextVersion(reader)
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]*ctxapi.Snapshot{}
		for _, v := range []int64{1, cur.ContextVersion} {
			if want[v], err = local.ContextSnapshot(reader, ctxapi.RootUnit, v); err != nil {
				t.Fatal(err)
			}
		}
		fx := apitest.Fixture{Reader: reader, Stranger: readerCtx(t1, "svc:channels", "service"), Other: readerCtx(t2, "svc:research", "service"),
			Current: cur.ContextVersion, Want: want}
		t.Run("本地绑定", func(t *testing.T) { apitest.RunContextReader(t, local, fx) })
		rem, err := ctxremote.NewContextReader(intern.URL + "/internal")
		if err != nil {
			t.Fatal(err)
		}
		t.Run("远程绑定", func(t *testing.T) { apitest.RunContextReader(t, rem, fx) })
		t.Run("用户主体按 can_view_profile 读取", func(t *testing.T) {
			if _, err := rem.ContextVersion(readerCtx(t1, bd, "user")); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("内部端口要求请求上下文", func(t *testing.T) {
			r, _ := http.NewRequest("GET", intern.URL+"/internal/v1/customer-context/context-version", nil)
			cert, _, _ := certs.Peer(t, "platform-api", false)
			client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: certs.Pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}}}
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatalf("得到 %d", resp.StatusCode)
			}
		})
	})

	t.Run("全国档案：000000 可保存，目标可选任一省市作为子集", func(t *testing.T) {
		national := strings.Replace(content(`[]`, `[]`, `["CAT-PIPE"]`), `["330000"]`, `["000000"]`, 1)
		var p cc.Profile
		decode(t, expect(t, admin3, t3, "PUT", profilePath, put(0, national), 200), &p)
		s, err := local.ContextSnapshot(readerCtx(t3, "svc:detection", "service"), ctxapi.RootUnit, 0)
		if err != nil || len(s.PolicyView.ServiceRegions) != 1 || s.PolicyView.ServiceRegions[0] != "000000" {
			t.Fatalf("%+v %v", s, err)
		}
		problem(t, expect(t, admin3, t3, "PUT", profilePath, put(1, strings.Replace(national, "000000", "000100", 1)), 422), "invalid_region")
		regions := func(v ...string) string {
			b, _ := json.Marshal(v)
			return `{"template_id":"APP-TENDER.A02","org_unit_id":"root","params":{"regions":{"source":"objective","values":` + string(b) + `}}}`
		}
		problem(t, expect(t, admin3, t3, "POST", objectives, regions("000100"), 422), "param_not_subset")
		var o cc.Objective
		decode(t, expect(t, admin3, t3, "POST", objectives, regions("330000", "330100", "110105"), 200), &o)
		if got := o.Params["regions"]; got.Source != "objective" || len(got.Values) != 3 {
			t.Fatalf("%+v", o.Params)
		}
	})

	t.Run("我方主体超过上界：快照省略主体、X-SELF 为未知，每个上下文版本告警一次", func(t *testing.T) {
		var entities []string
		for i := range 11 {
			entities = append(entities, fmt.Sprintf(`{"org_id":"org_01JA8ZXXXXXXXXXXXXXXXXXX%02d","qualifications_complete":false}`, i))
		}
		var p cc.Profile
		decode(t, expect(t, admin3, t3, "PUT", profilePath, put(1, content("["+strings.Join(entities, ",")+"]", `[]`, `["CAT-PIPE-PE"]`)), 200), &p)
		reader := readerCtx(t3, "svc:detection", "service")
		rem, err := ctxremote.NewContextReader(intern.URL + "/internal")
		if err != nil {
			t.Fatal(err)
		}
		var s *ctxapi.Snapshot
		for _, r := range []ctxapi.ContextReader{local, rem, local} { // 重复读取不重复告警
			if _, err := r.ContextVersion(reader); err != nil {
				t.Fatal(err)
			}
			if s, err = r.ContextSnapshot(reader, ctxapi.RootUnit, 0); err != nil {
				t.Fatal(err)
			}
		}
		if s.ContextVersion != p.ContextVersion || s.PolicyView.OrgIds != nil || s.PolicyView.Entities.Set {
			t.Fatalf("超过上界应省略主体而不截断：%+v", s.PolicyView)
		}
		// 未知传播：快照原样作为 A02 策略的 profile 变量，任一我方主体中标时 X-SELF 为未知、不排除。
		pack, err := policy.LoadPack(filepath.Join(packs, "application_domains", "tendering"))
		if err != nil {
			t.Fatal(err)
		}
		tpl := pack.Templates["TPL-TENDER-A02-SUPPLY"]
		compiled, issues := policy.Compile(tpl, pack.Features, policy.DefaultLimits())
		if issues != nil {
			t.Fatal(issues)
		}
		in, err := tpl.ExampleInput(tpl.Examples[0])
		if err != nil {
			t.Fatal(err)
		}
		raw, err := s.PolicyView.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		in.Profile = map[string]any{}
		decode(t, raw, &in.Profile)
		in.Fact["winner"] = map[string]any{"org_id": "org_01JA8ZXXXXXXXXXXXXXXXXXX00"}
		in.Lists = map[string]any{"customers": []any{}, "blocked": []any{}, "competitors": []any{}}
		r, err := compiled.Evaluate(in)
		if err != nil {
			t.Fatal(err)
		}
		self := policy.RuleTrace{}
		for _, tr := range r.Trace {
			if tr.ID == "X-SELF" {
				self = tr
			}
		}
		if self.ID == "" || self.Result != policy.Unknown {
			t.Fatalf("X-SELF 应为未知：%+v", self)
		}
		if r.Disposition == policy.Excluded {
			t.Fatalf("主体被省略时不应排除：%s %s", r.Disposition, r.Reason)
		}
		var warnings []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["level"] == "WARN" && strings.Contains(fmt.Sprint(entry["msg"]), "超出规模上界") {
				warnings = append(warnings, entry)
			}
		}
		if len(warnings) != 1 || warnings[0]["tenant"] != t3 || warnings[0]["context_version"] != float64(p.ContextVersion) ||
			warnings[0]["entities"] != float64(11) || warnings[0]["subsystem"] != "customer-context" {
			t.Fatalf("应恰好告警一次：%v", warnings)
		}
	})

	t.Run("事件载荷符合契约，审计同事务写入", func(t *testing.T) {
		rows, err := appDB.Pool.Query(ctx, `SELECT topic, envelope FROM kernel_outbox ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		counts := map[string]int{}
		for rows.Next() {
			var topic string
			var raw []byte
			if err := rows.Scan(&topic, &raw); err != nil {
				t.Fatal(err)
			}
			var envelope outbox.Envelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			schema, err := events.Schema(envelope.Type)
			if err != nil {
				t.Fatal(err)
			}
			if err := events.Validate(schema, envelope.Data); err != nil {
				t.Fatalf("%s：%v", envelope.Type, err)
			}
			if strings.HasPrefix(envelope.Type, "customer_context.") && (topic != "cell-cell-01.customer_context" || envelope.Zone != reqctx.ZoneTenant || envelope.Tenant == "") {
				t.Fatalf("%s 路由错误：%s %s", envelope.Type, topic, envelope.Zone)
			}
			counts[envelope.Type]++
		}
		// 档案：t1 保存 3 次（含并发胜出的一次）+ 刷新 1 次，t2 保存 1 次，t3 保存 2 次；
		// 目标：t1 启用、调整、停用、重新启用，t3 启用 1 次；每次启用同事务创建首版策略草案（第7段）。
		if counts["customer_context.profile.updated.v1"] != 7 || counts["customer_context.objective.changed.v1"] != 5 ||
			counts["customer_context.policy.drafted.v1"] != 2 || counts["audit.entry.recorded.v1"] != 14 {
			t.Fatalf("%v", counts)
		}
	})
}

func isCode(err error, code string) bool {
	var e *ctxapi.Error
	return err != nil && strings.Contains(err.Error(), code) && (asError(err, &e) && e.Code == code)
}

func asError(err error, target **ctxapi.Error) bool {
	for err != nil {
		if e, ok := err.(*ctxapi.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// syncBuffer 是并发安全的日志缓冲：HTTP 处理协程写入，测试协程读取。
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
