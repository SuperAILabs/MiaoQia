package integration_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/commerce"
	commerceapi "github.com/superailabs/miaoqia/services/subsystems/commerce/api"
	commerceremote "github.com/superailabs/miaoqia/services/subsystems/commerce/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	ccapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	ccremote "github.com/superailabs/miaoqia/services/subsystems/customercontext/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/detection"
	detectionapi "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api/facttest"
	detectionremote "github.com/superailabs/miaoqia/services/subsystems/detection/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	gwapi "github.com/superailabs/miaoqia/services/subsystems/gateway/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api/publictest"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api/datasettest"
)

// PRD 第4段服务端切片（对象页与证据）：真实 PostgreSQL（各子系统自己的库、行级安全、应用账号）+ 真实 identity 目录与 OpenFGA +
// 外部网关令牌校验 + 生成的 HTTP 服务端。gateway 的对象页经 mTLS 内部端口的远程绑定调用 commerce 计量与 detection 的信号固定求值
// 输入（以用户主体转交）；数据平面（对象、分区、证据）与权益覆盖使用标明为夹具的内存实现（publictest，内容均为虚构），事实视图与
// 归属边使用 facttest。用例编号对应 PRD 第4段第12节。信号的产生沿用第3段的设置（A02 激活经 learning 仿真与 identity 审批）。
func TestObjectPageEvidence(t *testing.T) {
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
	now := time.Now

	iam := testenv.NewDatabase(t)
	idModule := identity.ModuleWith(identity.Adapters{Now: now})
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", idModule.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2 := ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	user := func(role string) string { return role + "-" + suffix }
	admin, adminx, expert, dm, dm2, bd, bd2, ra, admin2 := user("admin"), user("adminx"), user("expert"), user("dm"), user("dm2"), user("bd"),
		user("bd2"), user("ra"), user("admin2")
	iamOwner, err := pgx.Connect(ctx, iam.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer iamOwner.Close(ctx)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO identity_tenants VALUES ($1,'甲','cell-01','active',1), ($2,'乙','cell-01','active',1)`, []any{t1, t2}},
		{`INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['admin'],1), ($1,$4,'active',ARRAY['domain_expert'],1),
 ($1,$5,'active',ARRAY['decision_maker'],1), ($1,$6,'active',ARRAY['decision_maker'],1), ($1,$7,'active',ARRAY['business_developer'],1),
 ($1,$8,'active',ARRAY['business_developer'],1), ($1,$9,'active',ARRAY['research_analyst'],1), ($10,$11,'active',ARRAY['admin','domain_expert','decision_maker'],1)`,
			[]any{t1, admin, adminx, expert, dm, dm2, bd, bd2, ra, t2, admin2}},
	} {
		if _, err := iamOwner.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	iamApp, err := db.Open(ctx, iam.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer iamApp.Close()
	bindings := runtime.NewBindings()
	idEnv := &runtime.Env{DB: iamApp, Bindings: bindings, Log: quiet, Cell: "cell-01", Config: func(string) string { return fga }}
	if err := idModule.Start(ctx, idEnv); err != nil {
		t.Fatal(err)
	}
	localDirectory, _ := runtime.Lookup[idapi.Directory](bindings, idapi.DirectoryBinding)
	syncDir := &flakyDirectory{Directory: localDirectory}
	bindings.Provide(idapi.DirectoryBinding, idapi.Directory(syncDir))

	packs, _ := filepath.Abs(filepath.Join("..", "..", "..", "packs"))
	config := func(k string) string {
		if k == "PACKS_DIR" {
			return packs
		}
		return ""
	}
	start := func(m runtime.Module) (*runtime.Env, *pgx.Conn) {
		t.Helper()
		database := testenv.NewDatabase(t)
		if err := db.Migrate(ctx, database.OwnerDSN, database.Name+"_app", m.Migrations); err != nil {
			t.Fatal(err)
		}
		app, err := db.Open(ctx, database.AppDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Close)
		owner, err := pgx.Connect(ctx, database.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { owner.Close(context.Background()) })
		env := &runtime.Env{DB: app, Bindings: bindings, Log: quiet, Cell: "cell-01", Config: config}
		if err := m.Start(ctx, env); err != nil {
			t.Fatal(err)
		}
		return env, owner
	}
	ccModule := customercontext.Module()
	ccEnv, ccOwner := start(ccModule)
	public, internal := http.NewServeMux(), http.NewServeMux()
	idModule.Routes(idEnv, public, internal)
	ccModule.Routes(ccEnv, public, internal)

	// detection 经 mTLS 内部端口读取生效策略（远程绑定），目标访问判断为本地绑定；两者都可注入故障
	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	intern := httptest.NewUnstartedServer(internal)
	intern.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	intern.StartTLS()
	defer intern.Close()
	remoteActive, err := ccremote.NewActivePolicies(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	active := &flakyActive{next: remoteActive}
	bindings.Provide(ccapi.ActivePoliciesBinding, ccapi.ActivePolicies(active))
	localAccess, _ := runtime.Lookup[ccapi.ObjectiveAccess](bindings, ccapi.AccessBinding)
	access := &flakyAccess{ObjectiveAccess: localAccess}
	bindings.Provide(ccapi.AccessBinding, ccapi.ObjectiveAccess(access))

	today := now().UTC().Truncate(24 * time.Hour)
	const org1 = "org_01JA8ZXXXXXXXXXXXXXXXXXXX1" // 我方主体（档案）
	fixture := datasettest.New(today.AddDate(0, 0, -365))
	fixture.Add(datasettest.A02(today, org1)...)
	fixture.SetStatus("INF-CRED", "not_live") // 信用领域未上线：激活版本的启用领域集合不含 INF-CRED
	lrnModule := learning.ModuleWith(learning.Adapters{Dataset: fixture, Domains: fixture, Now: now})
	lrnEnv, lrnOwner := start(lrnModule)
	lrnModule.Routes(lrnEnv, public, internal)

	facts := facttest.New(t1, t2)
	detModule := detection.ModuleWith(detection.Adapters{Facts: facts, Candidates: facts, ProjectLinks: facts, Now: now, LeaseTTL: time.Second})
	detEnv, detOwner := start(detModule)
	detModule.Routes(detEnv, public, internal)

	// commerce（查看计量）与 gateway（对象页 BFF）：gateway 经 mTLS 内部端口的远程绑定调用 commerce 计量与 detection 信号固定求值输入
	// （以用户主体转交），两者都可注入故障；数据平面与权益覆盖为夹具（publictest）
	var dayShift atomic.Int64
	comModule := commerce.ModuleWith(commerce.Adapters{Now: func() time.Time { return now().Add(time.Duration(dayShift.Load())) }})
	comEnv, comOwner := start(comModule)
	comModule.Routes(comEnv, public, internal)
	remoteMeter, err := commerceremote.NewMetering(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	meter := &flakyMeter{next: remoteMeter}
	bindings.Provide(commerceapi.MeteringBinding, commerceapi.ViewMetering(meter))
	remoteExplainer, err := detectionremote.NewExplainer(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	explainer := &flakyExplainer{next: remoteExplainer}
	bindings.Provide(detectionapi.ExplainerBinding, explainer)
	pub := publictest.New()
	gwModule := gateway.ModuleWith(gateway.Adapters{Public: pub, Evidence: publictest.Resolver{Store: pub}, Entitlements: pub, Now: now})
	gwEnv, gwOwner := start(gwModule)
	gwModule.Routes(gwEnv, public, internal)

	issuer := testissuer.New(t)
	resolver, _ := runtime.Lookup[idapi.SessionResolver](bindings, idapi.SessionBinding)
	secured, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", public, resolver)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(secured)
	defer server.Close()

	var keys atomic.Int64
	do := func(t *testing.T, subject, tenant string, mfa bool, method, path, body, key string) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+issuer.Token(t, subject, tenant, "cell-01", mfa))
		r.Header.Set("Content-Type", "application/json")
		if key == "" {
			key = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(subject, method, path, body, keys.Add(1)))))[:40]
		}
		r.Header.Set("Idempotency-Key", key)
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	codeOf := func(raw []byte) string {
		var p struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &p)
		return p.Code
	}
	expectKey := func(t *testing.T, subject, tenant string, mfa bool, method, path, body, key string, status int, code ...string) []byte {
		t.Helper()
		got, raw := do(t, subject, tenant, mfa, method, path, body, key)
		if got != status {
			t.Fatalf("%s %s %s：得到 %d，期望 %d：%s", subject, method, path, got, status, raw)
		}
		if len(code) > 0 && codeOf(raw) != code[0] {
			t.Fatalf("期望错误码 %s，得到 %s", code[0], raw)
		}
		return raw
	}
	expect := func(t *testing.T, subject string, mfa bool, method, path, body string, status int, code ...string) []byte {
		t.Helper()
		return expectKey(t, subject, t1, mfa, method, path, body, "", status, code...)
	}
	decode := func(t *testing.T, raw []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("%v：%s", err, raw)
		}
	}
	obj := func(t *testing.T, raw []byte) map[string]any {
		t.Helper()
		var m map[string]any
		decode(t, raw, &m)
		return m
	}

	// 发件箱泵（同 TestPolicySimulationApproval）：detection 的事件交给 learning 的反馈消费
	type source struct {
		name  string
		owner *pgx.Conn
		last  int64
	}
	sources := []*source{{name: "identity", owner: iamOwner}, {name: "customer-context", owner: ccOwner}, {name: "learning", owner: lrnOwner},
		{name: "detection", owner: detOwner}, {name: "gateway", owner: gwOwner}, {name: "commerce", owner: comOwner}}
	type route struct {
		module   runtime.Module
		env      *runtime.Env
		consumer string
	}
	routes := map[string][]route{
		"identity.approval.requested.v1":        {{ccModule, ccEnv, "customer-context.policy-approvals"}},
		"identity.approval.decided.v1":          {{ccModule, ccEnv, "customer-context.policy-approvals"}},
		"identity.approval.closed.v1":           {{ccModule, ccEnv, "customer-context.policy-approvals"}},
		"customer_context.policy.drafted.v1":    {{lrnModule, lrnEnv, "learning.policy-simulations"}},
		"learning.simulation.completed.v1":      {{ccModule, ccEnv, "customer-context.policy-simulations"}},
		"customer_context.objective.changed.v1": {{ccModule, ccEnv, "customer-context.objective-participants"}},
		"detection.signal.detected.v1":          {{lrnModule, lrnEnv, "learning.signal-feedback"}},
		"detection.signal.triaged.v1":           {{lrnModule, lrnEnv, "learning.signal-feedback"}},
		"gateway.view_meter.deferred.v1":        {{comModule, comEnv, "commerce.view-backfill"}},
	}
	deliver := func(t *testing.T, envelope outbox.Envelope) {
		t.Helper()
		for _, r := range routes[envelope.Type] {
			found := false
			for _, c := range r.module.Consumers(r.env) {
				if c.Name != r.consumer {
					continue
				}
				found = true
				if !slices.Contains(c.Topics, outbox.Topic(envelope.Zone, "cell-01", strings.SplitN(envelope.Type, ".", 2)[0])) {
					t.Fatalf("%s 不订阅 %s 的主题", c.Name, envelope.Type)
				}
				ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Tenant: envelope.Tenant, Cell: envelope.Cell, Purpose: "event"}), c.Subsystem)
				if err := r.env.DB.Tx(ectx, func(tx pgx.Tx) error { return c.Handlers[envelope.Type](ectx, tx, &envelope) }); err != nil {
					t.Fatalf("%s 处理 %s：%v", c.Name, envelope.Type, err)
				}
			}
			if !found {
				t.Fatalf("没有消费者 %s", r.consumer)
			}
		}
	}
	var delivered, pending []outbox.Envelope
	hold := map[string]bool{}
	pump := func(t *testing.T) {
		t.Helper()
		for {
			n := 0
			for _, s := range sources {
				rows, err := s.owner.Query(ctx, `SELECT seq, envelope FROM kernel_outbox WHERE seq > $1 ORDER BY seq`, s.last)
				if err != nil {
					t.Fatal(err)
				}
				var batch []outbox.Envelope
				for rows.Next() {
					var raw []byte
					var e outbox.Envelope
					if err := rows.Scan(&s.last, &raw); err != nil || json.Unmarshal(raw, &e) != nil {
						t.Fatal(err)
					}
					batch = append(batch, e)
				}
				rows.Close()
				for _, e := range batch {
					n++
					schema, err := events.Schema(e.Type)
					if err != nil {
						t.Fatal(err)
					}
					if err := events.Validate(schema, e.Data); err != nil {
						t.Fatalf("%s 的 %s 不符合契约：%v：%s", s.name, e.Type, err, e.Data)
					}
					if hold[e.Type] {
						pending = append(pending, e)
						continue
					}
					delivered = append(delivered, e)
					deliver(t, e)
				}
			}
			if n == 0 {
				return
			}
		}
	}

	// 公共事实：登记夹具的求值单位并构造 domain.fact.upserted.v1（按契约校验），交给 detection 的匹配消费
	fact := func(t *testing.T, bids ...facttest.Bid) (string, outbox.Envelope) {
		t.Helper()
		notice := ids.New("ntc")
		var version int64 = 1
		for i := range bids {
			bids[i].Notice = notice
			if bids[i].Result == "" {
				bids[i].Result = ids.New("bdr")
			}
			if bids[i].Title == "" {
				bids[i].Title = "夹具：某管网工程中标结果"
			}
			if bids[i].Location == "" {
				bids[i].Location = "330106"
			}
			if bids[i].Categories == nil {
				bids[i].Categories = []string{"CAT-PIPE-PE"}
			}
			if bids[i].Winner == "" {
				bids[i].Winner = ids.New("org")
			}
			facts.Put(notice, version, facttest.BidResult(bids[i]))
		}
		data, _ := json.Marshal(map[string]any{"info_domain": "INF-PROC", "fact_id": notice, "changed_fields": []string{"/results"}, "rights_tags": []string{"fixture"},
			"snapshot_ref": ids.New("snp"), "revision_ref": ids.New("exr"), "object_type": "tendering.notice", "fact_version": version, "schema_version": "1.0.0"})
		schema, err := events.Schema("domain.fact.upserted.v1")
		if err != nil {
			t.Fatal(err)
		}
		if err := events.Validate(schema, data); err != nil {
			t.Fatalf("夹具事件不符合契约：%v", err)
		}
		return notice, outbox.Envelope{SpecVersion: "1.0", ID: ids.New("evt"), Source: "miaoqia/domain-intelligence", Type: "domain.fact.upserted.v1",
			Time: now().UTC().Add(-time.Minute).Truncate(time.Millisecond), DataContentType: "application/json", Zone: reqctx.ZonePublic, Data: data}
	}
	match := func(envelope outbox.Envelope) error {
		for _, c := range detModule.Consumers(detEnv) {
			if c.Name != "detection.fact-matching" {
				continue
			}
			if !slices.Contains(c.Topics, outbox.Topic(envelope.Zone, "", "domain")) {
				return errors.New("detection 不订阅公共域的 domain 主题")
			}
			ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Purpose: "event"}), c.Subsystem)
			return detEnv.DB.Tx(ectx, func(tx pgx.Tx) error { return c.Handlers[envelope.Type](ectx, tx, &envelope) })
		}
		return errors.New("没有消费者 detection.fact-matching")
	}
	mustMatch := func(t *testing.T, envelope outbox.Envelope) {
		t.Helper()
		if err := match(envelope); err != nil {
			t.Fatal(err)
		}
		pump(t)
	}
	count := func(t *testing.T, owner *pgx.Conn, sql string, args ...any) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// sig 返回一个求值单位（公告与结果）的信号标识
	sig := func(t *testing.T, notice string) []string {
		t.Helper()
		rows, err := detOwner.Query(ctx, `SELECT id FROM det_signals WHERE subject_id = $1 ORDER BY part_id`, notice)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 档案（我方主体 org1）与 A02 目标：决策者 dm 启用，业务开拓者 bd、管理员 admin 与业务专家 expert 参与
	content := fmt.Sprintf(`{"subject_type":"organization","entities":[{"org_id":"%s","qualifications_complete":true}],"value_chain_roles":["ROLE-MFR"],
"industry_packs":["IND-BUILDING-MATERIALS"],"offering_categories":["CAT-PIPE-PE"],"brands":["示例品牌"],"service_regions":["330000"],"qualifications":[]}`, org1)
	expect(t, expert, false, "PUT", "/v1/customer-context/profile", `{"expected_version":0,"content":`+content+`}`, 200)
	var objective cc.Objective
	decode(t, expect(t, dm, false, "POST", "/v1/customer-context/objectives",
		fmt.Sprintf(`{"template_id":"APP-TENDER.A02","org_unit_id":"root","participants":["%s","%s","%s"]}`, bd, admin, expert), 200), &objective)
	objPath := "/v1/customer-context/objectives/" + objective.ID
	policyPath := objPath + "/policy"
	inbox := "/v1/detection/objectives/" + objective.ID + "/signals"
	pump(t)
	getPolicy := func(t *testing.T) cc.Policy {
		t.Helper()
		var p cc.Policy
		decode(t, expect(t, dm, false, "GET", policyPath, "", 200), &p)
		return p
	}
	approve := func(t *testing.T, approvalID string) {
		t.Helper()
		var a struct {
			Version int64 `json:"version"`
		}
		decode(t, expect(t, dm2, false, "GET", "/v1/identity/approvals/"+approvalID, "", 200), &a)
		expect(t, dm2, true, "POST", "/v1/identity/approvals/"+approvalID+"/decide", fmt.Sprintf(`{"expected_version":%d,"decision":"approve"}`, a.Version), 200)
	}
	activate := func(t *testing.T, note string) {
		t.Helper()
		p := getPolicy(t)
		if !p.Draft.Set {
			form := func() map[string]any {
				a, _ := p.Active.Get()
				raw, _ := json.Marshal(&a.Form)
				var m map[string]any
				decode(t, raw, &m)
				return m
			}()
			body, _ := json.Marshal(map[string]any{"expected_version": p.DraftVersion, "form": form})
			expect(t, dm, false, "PUT", policyPath+"/draft", string(body), 200)
			pump(t)
			p = getPolicy(t)
		}
		d := p.Draft.Or(cc.PolicyDraft{})
		sim := d.Simulation.Or(cc.PolicySimulationRef{})
		if sim.Status != "completed" {
			t.Fatalf("仿真未完成：%+v", d)
		}
		body, _ := json.Marshal(map[string]any{"expected_version": p.DraftVersion, "revision": d.Revision, "simulation_id": sim.SimulationID, "note": note})
		var r cc.PolicyRequest
		decode(t, expect(t, dm, true, "POST", policyPath+"/activation-requests", string(body), 200), &r)
		approve(t, r.ApprovalID)
		pump(t)
	}
	setParticipants := func(t *testing.T, participants ...string) {
		t.Helper()
		var o cc.Objective
		decode(t, expect(t, dm, false, "GET", objPath, "", 200), &o)
		params, _ := json.Marshal(o.Params)
		list, _ := json.Marshal(participants)
		expect(t, dm, false, "PUT", objPath, fmt.Sprintf(`{"expected_version":%d,"owner":"%s","participants":%s,"params":%s}`, o.Version, dm, list, params), 200)
	}

	// ===== PRD 第4段：对象页与证据 =====
	_, _, _, _, _ = inbox, admin, adminx, ra, today
	getJSON := func(t *testing.T, who, tenant, path, key string, status int, code ...string) map[string]any {
		t.Helper()
		return obj(t, expectKey(t, who, tenant, false, "GET", path, "", key, status, code...))
	}
	meters := func(t *testing.T, tenant, target string) int {
		t.Helper()
		return count(t, comOwner, `SELECT count(*) FROM com_view_meters WHERE tenant_id = $1 AND target = $2`, tenant, target)
	}
	usage := func(t *testing.T, tenant, kind string) int {
		t.Helper()
		return count(t, comOwner, `SELECT COALESCE(sum(used), 0)::int FROM com_view_usage WHERE tenant_id = $1 AND kind = $2`, tenant, kind)
	}
	deferredEvents := func(t *testing.T) int {
		t.Helper()
		return count(t, gwOwner, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' = 'gateway.view_meter.deferred.v1'`)
	}
	signalsOf := func(t *testing.T, who, tenant, query string) []string {
		t.Helper()
		p := getJSON(t, who, tenant, "/v1/detection/object-signals?"+query, "", 200)
		var out []string
		for _, r := range p["signals"].([]any) {
			out = append(out, r.(map[string]any)["id"].(string))
		}
		return out
	}

	// 夹具对象（虚构）：采购项目 pp、项目 prj（归属边 prj → pp）、中标方 orgA（求值后并入 orgB）、采购人 buyer
	pp, prj, orgA, orgB, buyer := ids.New("pp"), ids.New("prj"), ids.New("org"), ids.New("org"), ids.New("org")
	long := now().Add(-365 * 24 * time.Hour)
	pub.PutObject(long, gwapi.Object{ID: pp, Type: "procurement_project", Name: "夹具：某市政管网改造工程", Region: "330106", InfoDomain: "INF-PROC"})
	pub.PutObject(long, gwapi.Object{ID: prj, Type: "project", Name: "夹具：某片区综合整治项目", Region: "330106", InfoDomain: "INF-PROC"})
	pub.PutObject(long, gwapi.Object{ID: orgA, Type: "organization", Name: "夹具总包甲", Region: "330106", InfoDomain: "INF-ORG"})
	pub.PutObject(long, gwapi.Object{ID: orgB, Type: "organization", Name: "夹具总包乙（存续）", Region: "330106", InfoDomain: "INF-ORG"})
	pub.PutObject(long, gwapi.Object{ID: buyer, Type: "organization", Name: "夹具水务公司", Region: "330106", InfoDomain: "INF-ORG"})
	for _, o := range []string{pp, prj, orgA, orgB, buyer} {
		pub.Cover(t1, o, true)
	}
	pub.Cover(t2, orgA, true) // 租户 B 只覆盖 orgA（覆盖不足与隔离用例）
	pub.Cover(t2, orgB, true)
	facts.Link(prj, pp)

	// 激活 A02 并产生两条与对象相关的信号：同一采购项目 pp，中标方分别为 orgA 与另一家，采购人 buyer
	activate(t, "首次激活")
	n1, e1 := fact(t, facttest.Bid{Amount: 6_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}, Winner: orgA, WinnerName: "夹具总包甲",
		Project: pp, Buyer: buyer, BuyerName: "夹具水务公司"})
	n2, e2 := fact(t, facttest.Bid{Amount: 7_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}, Project: pp, Buyer: buyer})
	mustMatch(t, e1)
	mustMatch(t, e2)
	s1, s2 := sig(t, n1)[0], sig(t, n2)[0]
	var evaluated, created time.Time
	if err := detOwner.QueryRow(ctx, `SELECT evaluated_at, created_at FROM det_signals WHERE id = $1`, s1).Scan(&evaluated, &created); err != nil {
		t.Fatal(err)
	}
	// 对外时间精度到秒（见 bff.begin）
	evaluated, created = evaluated.Truncate(time.Second), created.Truncate(time.Second)
	if !created.After(evaluated) {
		t.Fatalf("求值时间 %v 应早于生成时间 %v", evaluated, created)
	}
	// 分区内容：公告时间线有一条在求值时间 T 之前记录，一条在 T 与信号生成时间之间记录（历史视角不应出现）
	pub.Add(pp, "notice_timeline", evaluated.Add(-time.Hour), map[string]any{"type": "tender", "published_at": "2026-10-09", "precision": "day"})
	pub.Add(pp, "notice_timeline", evaluated.Add(10*time.Millisecond), map[string]any{"type": "amendment", "changes": []any{"budget"}})
	pub.Add(pp, "summary", long, map[string]any{"field": "budget", "state": "value", "value": "20000000"})
	pub.Merge(evaluated.Add(20*time.Millisecond), orgA, orgB, "统一社会信用代码一致")
	pub.Add(orgA, "registration", long, map[string]any{"field": "name", "state": "value", "value": "夹具总包甲"})
	pub.SetStatus(orgB, "credit", "not_connected", "domain_not_live")
	for i := range 205 {
		cat := "CAT-PIPE-PE"
		if i%2 == 1 {
			cat = "CAT-OTHER"
		}
		pub.Add(orgB, "won_history", long, map[string]any{"project": fmt.Sprintf("夹具项目 %03d", i), "category": cat})
	}
	pub.Add(orgB, "supplied_brands", long, map[string]any{"brand": "夹具品牌", "owners": []any{}, "owner_state": "not_ready"})

	t.Run("第4段用例1、3：从信号进入采购项目页，信号上下文条分别标注求值时间与生成时间；本租户信号分区按目标的 can_view_signals 过滤", func(t *testing.T) {
		p := getJSON(t, dm, t1, "/v1/objects/"+pp+"?signal_id="+s1, "", 200)
		sc, ok := p["signal_context"].(map[string]any)
		if !ok || sc["signal_id"] != s1 || p["basis"].(map[string]any)["view"] != "current" {
			t.Fatalf("%+v", p)
		}
		ev, _ := time.Parse(time.RFC3339Nano, sc["evaluated_at"].(string))
		cr, _ := time.Parse(time.RFC3339Nano, sc["created_at"].(string))
		if !ev.Equal(evaluated) || !cr.Equal(created) || sc["snapshot_available"] != true {
			t.Fatalf("%+v", sc)
		}
		if m := p["meter"].(map[string]any); m["outcome"] != "counted" || m["kind"] != "object_view" {
			t.Fatalf("%+v", m)
		}
		if w := p["watermark"].(map[string]any); w["tenant_id"] != t1 || w["member"] != dm {
			t.Fatalf("%+v", w)
		}
		if got := fmt.Sprint(p["sections"]); got != "[summary lots parent_projects notice_timeline]" {
			t.Fatal(got)
		}
		// 参与人 bd 与决策者 dm 看到两条（发现时间新到旧，跨页游标）；非参与的业务开拓者 bd2 看不到
		if got := signalsOf(t, bd, t1, "object_id="+pp); !slices.Equal(got, []string{s2, s1}) {
			t.Fatalf("%v", got)
		}
		first := getJSON(t, dm, t1, "/v1/detection/object-signals?limit=1&object_id="+pp, "", 200)
		cur, _ := first["next_cursor"].(string)
		if len(first["signals"].([]any)) != 1 || cur == "" {
			t.Fatalf("%+v", first)
		}
		second := getJSON(t, dm, t1, "/v1/detection/object-signals?limit=1&object_id="+pp+"&cursor="+cur, "", 200)
		if r := second["signals"].([]any); len(r) != 1 || r[0].(map[string]any)["id"] != s1 || r[0].(map[string]any)["procurement_project_id"] != pp {
			t.Fatalf("%+v", second)
		}
		getJSON(t, bd, t1, "/v1/detection/object-signals?limit=1&object_id="+pp+"&cursor="+cur, "", 400, "invalid_cursor")
		if got := signalsOf(t, bd2, t1, "object_id="+pp); len(got) != 0 {
			t.Fatalf("非参与的业务开拓者看到了 %v", got)
		}
		if p := getJSON(t, bd2, t1, "/v1/objects/"+pp+"?signal_id="+s1, "", 200); p["signal_context"] != nil {
			t.Fatalf("非参与人看到了信号上下文：%+v", p)
		}
		// 组织（中标方 / 采购人）与项目（经归属边）
		if got := signalsOf(t, dm, t1, "role=winner&object_id="+orgA); !slices.Equal(got, []string{s1}) {
			t.Fatalf("%v", got)
		}
		if got := signalsOf(t, dm, t1, "role=buyer&object_id="+buyer); !slices.Equal(got, []string{s2, s1}) {
			t.Fatalf("%v", got)
		}
		if got := signalsOf(t, dm, t1, "role=winner&object_id="+buyer); len(got) != 0 {
			t.Fatalf("%v", got)
		}
		if got := signalsOf(t, dm, t1, "object_id="+prj); !slices.Equal(got, []string{s2, s1}) {
			t.Fatalf("%v", got)
		}
		getJSON(t, dm, t1, "/v1/detection/object-signals?role=buyer&object_id="+pp, "", 400)
		// 归属边读取失败、目标访问判断失败：失败关闭（503），不当作没有信号
		facts.Fail(true)
		getJSON(t, dm, t1, "/v1/detection/object-signals?object_id="+prj, "", 503, "project_links_unavailable")
		facts.Fail(false)
		access.fail.Store(true)
		getJSON(t, dm, t1, "/v1/detection/object-signals?object_id="+pp, "", 503)
		access.fail.Store(false)
	})

	t.Run("第4段用例2、3、33：其他租户的信号标识与不存在的信号一样只显示公共部分；租户 B 看不到租户 A 的信号；无覆盖的租户在缓存预热后仍被拒绝", func(t *testing.T) {
		withOther := getJSON(t, admin2, t2, "/v1/objects/"+orgA+"?signal_id="+s1, "", 200)
		missing := getJSON(t, admin2, t2, "/v1/objects/"+orgA+"?signal_id="+ids.New("sig"), "", 200)
		for _, p := range []map[string]any{withOther, missing} {
			if p["signal_context"] != nil || p["signal_context_unavailable"] != nil || p["basis"].(map[string]any)["view"] != "current" {
				t.Fatalf("%+v", p)
			}
		}
		if fmt.Sprint(withOther["object"], withOther["sections"]) != fmt.Sprint(missing["object"], missing["sections"]) {
			t.Fatalf("响应可区分：%+v / %+v", withOther, missing)
		}
		// 历史视角同样不暴露：他租户信号的 signal_time 退回当前视角
		if p := getJSON(t, admin2, t2, "/v1/objects/"+orgA+"?view=signal_time&signal_id="+s1, "", 200); p["basis"].(map[string]any)["view"] != "current" {
			t.Fatalf("%+v", p)
		}
		if got := signalsOf(t, admin2, t2, "object_id="+orgA); len(got) != 0 {
			t.Fatalf("租户 B 看到了 %v", got)
		}
		// 租户 A 已打开 pp；租户 B 没有覆盖：只返回对象名称与原因，不返回内容，不计量
		before := meters(t, t2, pp)
		raw := expectKey(t, admin2, t2, false, "GET", "/v1/objects/"+pp, "", "", 403, "entitlement.coverage_insufficient")
		var prob map[string]any
		decode(t, raw, &prob)
		if o := prob["object"].(map[string]any); o["id"] != pp || o["name"] == "" || prob["sections"] != nil || prob["meter"] != nil {
			t.Fatalf("%s", raw)
		}
		expectKey(t, admin2, t2, false, "GET", "/v1/objects/"+pp+"/sections/notice_timeline", "", "", 403, "entitlement.coverage_insufficient")
		if meters(t, t2, pp) != before {
			t.Fatal("覆盖不足的请求被计量")
		}
	})

	t.Run("第4段用例4：外部协作者（非租户成员）与无会话的请求不能访问对象页与证据接口", func(t *testing.T) {
		outsider := "outsider-" + suffix
		for _, path := range []string{"/v1/objects/" + pp, "/v1/objects/" + pp + "/sections/summary", "/v1/evidence/" + ids.New("ev")} {
			if got, _ := do(t, outsider, t1, false, "GET", path, "", ""); got != 403 {
				t.Fatalf("%s：%d", path, got)
			}
			r, _ := http.NewRequest("GET", server.URL+path, nil)
			resp, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatalf("%s 无会话：%d", path, resp.StatusCode)
			}
		}
	})

	t.Run("第4段用例7：计量去重——刷新与网络重发只计一次，同一尝试键重放同一结果，跨计量日再计一次", func(t *testing.T) {
		before := usage(t, t1, "object_view")
		key := "page-" + suffix + "-0001"
		a := getJSON(t, ra, t1, "/v1/objects/"+pp, key, 200)["meter"].(map[string]any)
		b := getJSON(t, ra, t1, "/v1/objects/"+pp, key, 200)["meter"].(map[string]any)
		c := getJSON(t, ra, t1, "/v1/objects/"+pp+"/sections/summary", "", 200)["meter"].(map[string]any)
		if a["outcome"] != "counted" || fmt.Sprint(a) != fmt.Sprint(b) || c["outcome"] != "unchanged" {
			t.Fatalf("%+v %+v %+v", a, b, c)
		}
		if usage(t, t1, "object_view") != before+1 {
			t.Fatal("同一成员同一对象同一计量日计了不止一次")
		}
		dayShift.Store(int64(24 * time.Hour))
		if m := getJSON(t, ra, t1, "/v1/objects/"+pp, "", 200)["meter"].(map[string]any); m["outcome"] != "counted" {
			t.Fatalf("跨计量日：%+v", m)
		}
		dayShift.Store(0)
	})

	t.Run("第4段用例6、35：额度用完返回 quota.exhausted 与重置时间，已计过的对象照常；同一尝试重放失败；翻页超过上限 limit.safety_cap", func(t *testing.T) {
		day := now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
		if _, err := comOwner.Exec(ctx, `INSERT INTO com_view_usage (tenant_id, kind, metering_day, used) VALUES ($1, 'object_view', $2, 2000)
 ON CONFLICT (tenant_id, kind, metering_day) DO UPDATE SET used = 2000`, t1, day); err != nil {
			t.Fatal(err)
		}
		key := "quota-" + suffix + "-0001"
		raw := expectKey(t, dm, t1, false, "GET", "/v1/objects/"+buyer, "", key, 429, "quota.exhausted")
		var prob map[string]any
		decode(t, raw, &prob)
		if jsonNum(prob, "used") != 2000 || jsonNum(prob, "limit") != 2000 || prob["resets_at"] == nil || strings.Contains(string(raw), "upgrade") {
			t.Fatalf("%s", raw)
		}
		// 额度用完后的“缓存命中”式刷新仍经计量：已计过的对象 unchanged，新对象拒绝
		if m := getJSON(t, dm, t1, "/v1/objects/"+pp, "", 200)["meter"].(map[string]any); m["outcome"] != "unchanged" {
			t.Fatalf("%+v", m)
		}
		expectKey(t, dm, t1, false, "GET", "/v1/objects/"+buyer+"/sections/registration", "", "", 429, "quota.exhausted")
		if _, err := comOwner.Exec(ctx, `UPDATE com_view_usage SET used = 0 WHERE tenant_id = $1 AND kind = 'object_view' AND metering_day = $2`, t1, day); err != nil {
			t.Fatal(err)
		}
		// 同一尝试键重放该尝试的结果（含失败）；新的尝试照常计入
		expectKey(t, dm, t1, false, "GET", "/v1/objects/"+buyer, "", key, 429, "quota.exhausted")
		if m := getJSON(t, dm, t1, "/v1/objects/"+buyer, "", 200)["meter"].(map[string]any); m["outcome"] != "counted" {
			t.Fatalf("%+v", m)
		}
		// 组织页列表每页 20 条，翻到 200 条为止；之后的位置 422 limit.safety_cap（不读取、不计量）
		cursor, n := "", 0
		var last map[string]any
		for range 10 {
			path := "/v1/objects/" + orgA + "/sections/won_history"
			if cursor != "" {
				path += "?cursor=" + cursor
			}
			last = getJSON(t, dm, t1, path, "", 200)
			n += len(last["items"].([]any))
			cursor, _ = last["next_cursor"].(string)
		}
		if n != 200 || cursor != "" || last["capped"] != true {
			t.Fatalf("%d %q %+v", n, cursor, last["capped"])
		}
		prev := getJSON(t, dm, t1, "/v1/objects/"+orgA+"/sections/won_history", "", 200)["next_cursor"].(string)
		rawCur, _ := base64.RawURLEncoding.DecodeString(prev)
		var c map[string]any
		decode(t, rawCur, &c)
		c["o"] = 200
		forged, _ := json.Marshal(c)
		calls := pub.Requests("section")
		getJSON(t, dm, t1, "/v1/objects/"+orgA+"/sections/won_history?cursor="+base64.RawURLEncoding.EncodeToString(forged), "", 422, "limit.safety_cap")
		if pub.Requests("section") != calls {
			t.Fatal("超过安全上限仍读取了分区")
		}
	})

	t.Run("第4段用例36：计量服务不可用时放行并补记，覆盖与身份检查照常；补记按计量操作标识去重", func(t *testing.T) {
		meter.fail.Store(true)
		before, events := usage(t, t1, "object_view"), deferredEvents(t)
		p := getJSON(t, expert, t1, "/v1/objects/"+prj, "", 200)
		if p["meter"].(map[string]any)["outcome"] != "deferred" {
			t.Fatalf("%+v", p["meter"])
		}
		// 已计过的对象（dm 今天已看过 pp）同样登记补记
		getJSON(t, dm, t1, "/v1/objects/"+pp, "", 200)
		// 无覆盖的租户照常被拒绝，不登记补记
		expectKey(t, admin2, t2, false, "GET", "/v1/objects/"+pp, "", "", 403, "entitlement.coverage_insufficient")
		if got := deferredEvents(t); got != events+2 {
			t.Fatalf("补记事件 %d，期望 %d", got, events+2)
		}
		meter.fail.Store(false)
		hold["gateway.view_meter.deferred.v1"] = true
		pending = nil
		pump(t)
		if len(pending) != 2 {
			t.Fatalf("%d", len(pending))
		}
		for range 2 { // 重复投递
			for _, e := range pending {
				deliver(t, e)
			}
		}
		hold["gateway.view_meter.deferred.v1"] = false
		if usage(t, t1, "object_view") != before+1 || count(t, comOwner, `SELECT count(*) FROM com_view_meters WHERE target = $1 AND source = 'backfill'`, prj) != 1 {
			t.Fatalf("补记：用量 %d → %d", before, usage(t, t1, "object_view"))
		}
		// 恢复后同一成员同一对象在线查看为 unchanged（与补记共用计量操作标识）
		if m := getJSON(t, expert, t1, "/v1/objects/"+prj, "", 200)["meter"].(map[string]any); m["outcome"] != "unchanged" {
			t.Fatalf("%+v", m)
		}
	})

	t.Run("第4段用例37、39、40、19：信号判断时视角以 evaluated_at 为记录截止（不是 created_at），统计历史不可用，合并只另行提示", func(t *testing.T) {
		p := getJSON(t, dm, t1, "/v1/objects/"+pp+"?view=signal_time&signal_id="+s1, "", 200)
		basis := p["basis"].(map[string]any)
		cut, _ := time.Parse(time.RFC3339Nano, basis["recorded_cutoff"].(string))
		if basis["view"] != "signal_time" || !cut.Equal(evaluated) || len(basis["pinned"].([]any)) == 0 {
			t.Fatalf("%+v", basis)
		}
		sec := getJSON(t, dm, t1, "/v1/objects/"+pp+"/sections/notice_timeline?view=signal_time&signal_id="+s1, "", 200)
		if len(sec["items"].([]any)) != 1 || jsonNum(sec, "later_count") != 1 || sec["status"] != "ok" {
			t.Fatalf("%+v", sec)
		}
		b, _ := pub.LastSection()
		if !b.RecordedCutoff.Equal(evaluated) || b.View != "signal_time" {
			t.Fatalf("%+v", b)
		}
		if cur := getJSON(t, dm, t1, "/v1/objects/"+pp+"/sections/notice_timeline", "", 200); len(cur["items"].([]any)) != 2 || cur["later_count"] != nil {
			t.Fatalf("%+v", cur)
		}
		// 求值后合并：历史视角显示 A 并另行提示现已并入 B；当前视角显示 B 与合并来源
		hist := getJSON(t, dm, t1, "/v1/objects/"+orgA+"?view=signal_time&signal_id="+s1, "", 200)["object"].(map[string]any)
		cm, _ := hist["current_merge"].(map[string]any)
		if hist["id"] != orgA || cm == nil || cm["org_id"] != orgB || cm["state"] != "merged" {
			t.Fatalf("%+v", hist)
		}
		cur := getJSON(t, dm, t1, "/v1/objects/"+orgA, "", 200)["object"].(map[string]any)
		if cur["id"] != orgB || cur["requested_id"] != orgA || len(cur["merged_from"].([]any)) != 1 {
			t.Fatalf("%+v", cur)
		}
		calls := pub.Requests("section")
		st := getJSON(t, dm, t1, "/v1/objects/"+orgA+"/sections/statistics?view=signal_time&signal_id="+s1, "", 200)
		if st["status"] != "history_unavailable" || len(st["items"].([]any)) != 0 || pub.Requests("section") != calls {
			t.Fatalf("%+v", st)
		}
		if reg := getJSON(t, dm, t1, "/v1/objects/"+orgA+"/sections/registration?view=signal_time&signal_id="+s1, "", 200); reg["status"] != "ok" {
			t.Fatalf("%+v", reg)
		}
		// 视图快照已过保存期：依赖快照还原的分区历史不可用，不改用截止时点的查询
		var hash string
		if err := detOwner.QueryRow(ctx, `SELECT view_snapshot_hash FROM det_signals WHERE id = $1`, s1).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if _, err := detOwner.Exec(ctx, `UPDATE det_signals SET view_snapshot_hash = 'sha256:'||repeat('0', 64) WHERE id = $1`, s1); err != nil {
			t.Fatal(err)
		}
		if s := getJSON(t, dm, t1, "/v1/objects/"+pp+"/sections/summary?view=signal_time&signal_id="+s1, "", 200); s["status"] != "history_unavailable" {
			t.Fatalf("%+v", s)
		}
		if _, err := detOwner.Exec(ctx, `UPDATE det_signals SET view_snapshot_hash = $2 WHERE id = $1`, s1, hash); err != nil {
			t.Fatal(err)
		}
		// signal_time 必须带信号；detection 不可用时历史视角失败关闭，当前视角只缺上下文条
		getJSON(t, dm, t1, "/v1/objects/"+pp+"?view=signal_time", "", 400)
		explainer.fail.Store(true)
		getJSON(t, dm, t1, "/v1/objects/"+pp+"?view=signal_time&signal_id="+s1, "", 503)
		getJSON(t, dm, t1, "/v1/objects/"+pp+"/sections/notice_timeline?view=signal_time&signal_id="+s1, "", 503)
		if p := getJSON(t, dm, t1, "/v1/objects/"+pp+"?signal_id="+s1, "", 200); p["signal_context"] != nil || p["signal_context_unavailable"] != true {
			t.Fatalf("%+v", p)
		}
		explainer.fail.Store(false)
	})

	t.Run("第4段用例23、28、29：分区独立失败；含我方品类按本租户档案品类筛选，不写入公共数据；未接入的领域显示数据未接入", func(t *testing.T) {
		pub.Fail("section/supplied_brands", true)
		if s := getJSON(t, dm, t1, "/v1/objects/"+orgB+"/sections/supplied_brands", "", 200); s["status"] != "unavailable" || s["meter"] != nil {
			t.Fatalf("%+v", s)
		}
		if s := getJSON(t, dm, t1, "/v1/objects/"+orgB+"/sections/won_history", "", 200); s["status"] != "ok" {
			t.Fatalf("%+v", s)
		}
		pub.Fail("section/supplied_brands", false)
		mine := getJSON(t, dm, t1, "/v1/objects/"+orgB+"/sections/won_history?mine=true", "", 200)
		_, cats := pub.LastSection()
		if !slices.Equal(cats, []string{"CAT-PIPE-PE"}) || len(mine["items"].([]any)) != 20 {
			t.Fatalf("%v %+v", cats, mine)
		}
		for _, it := range mine["items"].([]any) {
			if it.(map[string]any)["category"] != "CAT-PIPE-PE" {
				t.Fatalf("%+v", it)
			}
		}
		if s := getJSON(t, dm, t1, "/v1/objects/"+orgB+"/sections/credit", "", 200); s["status"] != "not_connected" || s["meter"] != nil {
			t.Fatalf("%+v", s)
		}
		getJSON(t, dm, t1, "/v1/objects/"+orgB+"/sections/lots", "", 404, "section_not_found")
		pub.Fail("resolve", true)
		getJSON(t, dm, t1, "/v1/objects/"+pp, "", 503, "public_data_unavailable")
		pub.Fail("resolve", false)
	})

	t.Run("第4段用例11–14、34：证据展示级别按租户当前权利，派生证据取全部输入中最严格的级别，BFF 按级别再次裁剪；权利收紧后刷新即生效", func(t *testing.T) {
		ev1, ev2, ev3 := ids.New("ev"), ids.New("ev"), ids.New("ev")
		pub.PutEvidence(gwapi.Evidence{ID: ev1, Kind: "document", DisplayLevel: gwapi.FullText, ObjectID: orgA, Snippet: "中标金额：壹仟捌佰万元整",
			Context: "第一标包 中标人：夹具总包甲 中标金额：壹仟捌佰万元整", Locator: "第2页第3段", SnapshotSavedAt: evaluated,
			Source: gwapi.EvidenceSource{Name: "夹具交易平台", URL: "https://example.invalid/n/1", Reachable: false, UnreachableSince: evaluated}})
		pub.PutEvidence(gwapi.Evidence{ID: ev3, Kind: "record", DisplayLevel: gwapi.CitationOnly, ObjectID: orgA, RecordKey: "k", RecordValue: "v",
			Source: gwapi.EvidenceSource{URL: "https://example.invalid/r/1", Reachable: true}})
		pub.PutEvidence(gwapi.Evidence{ID: ev2, Kind: "derivation", DisplayLevel: gwapi.FullText, ObjectID: orgA, Transform: "区划新旧代码对照 2024 版",
			Snippet: "330106", Inputs: []gwapi.EvidenceInput{{ID: ev1, DisplayLevel: gwapi.FullText}, {ID: ev3, DisplayLevel: gwapi.CitationOnly}},
			Source: gwapi.EvidenceSource{Reachable: true}})
		pub.SetLevel(t2, ev1, gwapi.CitationOnly)
		a := getJSON(t, dm, t1, "/v1/evidence/"+ev1, "", 200)
		if a["display_level"] != "full_text" || a["context"] == nil || a["snippet"] == nil || a["source"].(map[string]any)["reachable"] != false ||
			a["source"].(map[string]any)["unreachable_since"] == nil || a["meter"].(map[string]any)["kind"] != "evidence_view" {
			t.Fatalf("%+v", a)
		}
		b := getJSON(t, admin2, t2, "/v1/evidence/"+ev1, "", 200)
		if b["display_level"] != "citation_only" || b["context"] != nil || b["snippet"] != nil || b["reason_code"] != "rights.restricted" ||
			b["source"].(map[string]any)["url"] == nil {
			t.Fatalf("租户 B 复用了租户 A 的展示级别：%+v", b)
		}
		d := getJSON(t, dm, t1, "/v1/evidence/"+ev2, "", 200)
		if d["display_level"] != "citation_only" || d["snippet"] != nil || d["transform"] == nil {
			t.Fatalf("%+v", d)
		}
		pub.SetLevel(t1, ev1, gwapi.NoDisplay)
		r := getJSON(t, dm, t1, "/v1/evidence/"+ev1, "", 200)
		if r["display_level"] != "none" || r["snippet"] != nil || r["context"] != nil || r["locator"] != nil || r["source"].(map[string]any)["name"] != nil {
			t.Fatalf("权利收紧后仍返回内容：%+v", r)
		}
		if meters(t, t1, ev1) != 1 {
			t.Fatal("同一成员同一证据同一计量日应只计一次")
		}
		getJSON(t, dm, t1, "/v1/evidence/"+ids.New("ev"), "", 404, "evidence_not_found")
		// 证据所属对象不在覆盖范围：拒绝且不计量
		ev4 := ids.New("ev")
		pub.PutEvidence(gwapi.Evidence{ID: ev4, Kind: "document", DisplayLevel: gwapi.FullText, ObjectID: pp, Snippet: "x"})
		expectKey(t, admin2, t2, false, "GET", "/v1/evidence/"+ev4, "", "", 403, "entitlement.coverage_insufficient")
		if meters(t, t2, ev4) != 0 {
			t.Fatal("覆盖不足的证据查看被计量")
		}
	})

	t.Run("第4段生命周期：移除参与人后对象页的本租户信号与信号上下文立即不可见", func(t *testing.T) {
		if got := signalsOf(t, bd, t1, "object_id="+pp); len(got) != 2 {
			t.Fatalf("%v", got)
		}
		setParticipants(t, admin, expert)
		pump(t)
		if got := signalsOf(t, bd, t1, "object_id="+pp); len(got) != 0 {
			t.Fatalf("移除后仍可见 %v", got)
		}
		if p := getJSON(t, bd, t1, "/v1/objects/"+pp+"?view=signal_time&signal_id="+s1, "", 200); p["signal_context"] != nil || p["basis"].(map[string]any)["view"] != "current" {
			t.Fatalf("%+v", p)
		}
	})
}

// flakyMeter 注入查看计量的失败（commerce 不可用）。
type flakyMeter struct {
	next commerceapi.ViewMetering
	fail atomic.Bool
}

func (f *flakyMeter) RecordView(ctx context.Context, kind, target, key string) (*commerceapi.Receipt, error) {
	if f.fail.Load() {
		return nil, errors.New("注入：计量服务不可用")
	}
	return f.next.RecordView(ctx, kind, target, key)
}

// flakyExplainer 注入信号解释读取的失败（detection 不可用）。
type flakyExplainer struct {
	next detectionapi.SignalExplainer
	fail atomic.Bool
}

func (f *flakyExplainer) Explain(ctx context.Context, signalID string) (*detectionapi.Explanation, error) {
	if f.fail.Load() {
		return nil, errors.New("注入：detection 不可用")
	}
	return f.next.Explain(ctx, signalID)
}

func jsonNum(m map[string]any, k string) int64 {
	v, _ := m[k].(float64)
	return int64(v)
}
