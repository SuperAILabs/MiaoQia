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
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	ccapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	ccremote "github.com/superailabs/miaoqia/services/subsystems/customercontext/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/detection"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api/facttest"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api/datasettest"
)

// flakyActive 注入生效策略读取的失败（customer-context 主库不可读）。
type flakyActive struct {
	next  ccapi.ActivePolicies
	fail  atomic.Bool
	reads atomic.Int64
}

func (f *flakyActive) ActivePolicies(ctx context.Context, limit int, cursor string) (*ccapi.ActivePolicyList, error) {
	f.reads.Add(1)
	if f.fail.Load() {
		return nil, errors.New("注入：生效策略不可读")
	}
	return f.next.ActivePolicies(ctx, limit, cursor)
}

// flakyAccess 注入目标访问判断的失败（customer-context 或授权服务不可用）。
type flakyAccess struct {
	ccapi.ObjectiveAccess
	fail atomic.Bool
}

func (f *flakyAccess) Access(ctx context.Context, objective string, relations []string) ([]string, error) {
	if f.fail.Load() {
		return nil, errors.New("注入：目标访问判断不可用")
	}
	return f.ObjectiveAccess.Access(ctx, objective, relations)
}

// PRD 第3段服务端切片（信号收件箱、处理与反馈、批量与撤销、反馈汇总）与第7段用例 1、15、16（生效边界上的匹配）：真实 PostgreSQL
// （各子系统自己的库、行级安全、应用账号）+ 真实 identity 目录与 OpenFGA + 外部网关令牌校验 + 生成的 HTTP 服务端；detection 读取
// 生效策略经 mTLS 内部端口的远程绑定。事实视图与订阅反查使用标明为夹具的内存实现（facttest，事实均为虚构），learning 的仿真数据集
// 使用 datasettest。发件箱事件按契约校验后直接交给消费处理。用例编号对应 PRD 第3段第12节（另注“第7段”的对应第7段第12节）。
func TestSignalInboxFeedback(t *testing.T) {
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
	// 批量作业的执行钩子：逐项执行前调用（用于在固定位置注入故障、并发与中断）
	var hookMu sync.Mutex
	var hook func(batchID string, position int) error
	setHook := func(h func(string, int) error) {
		hookMu.Lock()
		defer hookMu.Unlock()
		hook = h
	}
	detModule := detection.ModuleWith(detection.Adapters{Facts: facts, Candidates: facts, Now: now, LeaseTTL: time.Second,
		BatchHook: func(_ context.Context, batchID string, position int) error {
			hookMu.Lock()
			h := hook
			hookMu.Unlock()
			if h == nil {
				return nil
			}
			return h(batchID, position)
		}})
	detEnv, detOwner := start(detModule)
	detModule.Routes(detEnv, public, internal)

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
		{name: "detection", owner: detOwner}}
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

	// 收件箱读取与处理的辅助
	page := func(t *testing.T, who, query string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, false, "GET", inbox+query, "", 200))
	}
	rowsOf := func(p map[string]any) []map[string]any {
		var out []map[string]any
		for _, r := range p["signals"].([]any) {
			out = append(out, r.(map[string]any))
		}
		return out
	}
	detail := func(t *testing.T, who, id string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, false, "GET", "/v1/detection/signals/"+id, "", 200))
	}
	row := func(t *testing.T, who, id string) map[string]any {
		t.Helper()
		return detail(t, who, id)["signal"].(map[string]any)
	}
	num := func(m map[string]any, k string) int64 {
		v, _ := m[k].(float64)
		return int64(v)
	}
	var ops atomic.Int64
	opID := func() string { return fmt.Sprintf("op-%s-%04d", suffix, ops.Add(1)) }
	// triageBody 以查看者当前看到的版本构造单条处理请求
	triageBody := func(t *testing.T, who, id, action string, extra map[string]any) map[string]any {
		t.Helper()
		r := row(t, who, id)
		m := map[string]any{"operation_id": opID(), "action": action, "seen_content_version": num(r, "content_version"),
			"state_version": num(r, "state_version"), "feedback_version": num(r, "feedback_version")}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	triageRaw := func(t *testing.T, who, id string, body map[string]any, key string) (int, []byte) {
		t.Helper()
		b, _ := json.Marshal(body)
		return do(t, who, t1, false, "POST", "/v1/detection/signals/"+id+"/triage", string(b), key)
	}
	triage := func(t *testing.T, who, id, action string, extra map[string]any, status int, code ...string) map[string]any {
		t.Helper()
		got, raw := triageRaw(t, who, id, triageBody(t, who, id, action, extra), "")
		if got != status || (len(code) > 0 && codeOf(raw) != code[0]) {
			t.Fatalf("%s %s %s：得到 %d，期望 %d %v：%s", who, action, id, got, status, code, raw)
		}
		return obj(t, raw)
	}
	fb := func(result string, reasons ...string) map[string]any {
		if reasons == nil {
			reasons = []string{}
		}
		return map[string]any{"feedback": map[string]any{"result": result, "reasons": reasons}}
	}
	batchItems := func(t *testing.T, who string, signals []string) []map[string]any {
		t.Helper()
		var items []map[string]any
		for _, id := range signals {
			r := row(t, who, id)
			items = append(items, map[string]any{"signal_id": id, "seen_content_version": num(r, "content_version"), "state_version": num(r, "state_version"),
				"feedback_version": num(r, "feedback_version")})
		}
		return items
	}
	batch := func(t *testing.T, who string, body map[string]any, key string, status int, code ...string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		return obj(t, expectKey(t, who, t1, false, "POST", "/v1/detection/objectives/"+objective.ID+"/signal-batches", string(b), key, status, code...))
	}
	itemsOf := func(b map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, it := range b["items"].([]any) {
			m := it.(map[string]any)
			out[m["signal_id"].(string)] = m
		}
		return out
	}
	summaryOf := func(b map[string]any) string {
		s := b["summary"].(map[string]any)
		return fmt.Sprintf("%s total=%d ok=%d same=%d definite=%d transient=%d unprocessed=%d", b["state"], num(s, "total"), num(s, "succeeded"),
			num(s, "no_change"), num(s, "failed_definite"), num(s, "failed_transient"), num(s, "unprocessed"))
	}
	undo := func(t *testing.T, who string, target map[string]any, status int, code ...string) map[string]any {
		t.Helper()
		target["operation_id"] = opID()
		b, _ := json.Marshal(target)
		return obj(t, expect(t, who, false, "POST", "/v1/detection/triage-undos", string(b), status, code...))
	}
	feedbackOf := func(t *testing.T, who, id string) map[string]any {
		t.Helper()
		f, _ := row(t, who, id)["my_feedback"].(map[string]any)
		return f
	}
	opsCount := func(t *testing.T, where string, args ...any) int {
		t.Helper()
		return count(t, detOwner, `SELECT count(*) FROM det_triage_ops WHERE `+where, args...)
	}

	// 信号：跟进候选、复核候选（品类与中标方识别低于证据门槛）、观察（候选人公示）、排除（中标方是我方主体）、
	// 同标包两家中标（其一金额未公布）、服务区域外（不产生信号）
	var pursue, review, watch, excluded, lotA, lotB string
	var pursueEnv outbox.Envelope
	t.Run("第7段用例1、第3段用例1：首次激活前不产生信号、收件箱为空；激活后之后开始的匹配批次产生信号，每条固定策略版本、代次与上下文版本", func(t *testing.T) {
		before, envBefore := fact(t, facttest.Bid{Amount: 6_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}})
		mustMatch(t, envBefore)
		if n := count(t, detOwner, `SELECT count(*) FROM det_signals`) + count(t, detOwner, `SELECT count(*) FROM det_matches`); n != 0 {
			t.Fatalf("激活前产生了 %d 条信号或匹配记录", n)
		}
		if p := page(t, dm, ""); len(rowsOf(p)) != 0 || p["viewer_can_feedback"] != true {
			t.Fatalf("%+v", p)
		}
		activate(t, "首次激活")
		p := getPolicy(t)
		if p.Generation != 1 || p.ActiveVersion != 1 {
			t.Fatalf("%+v", p)
		}
		var lot string
		var env outbox.Envelope
		var n1, n2, n3, n4, n6 string
		var e2, e3, e4, e5, e6 outbox.Envelope
		n1, pursueEnv = fact(t, facttest.Bid{Amount: 6_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}, WinnerName: "夹具总包甲"})
		n2, e2 = fact(t, facttest.Bid{Amount: 1_000_000, CategoryConfidence: 0.5, WinnerConfidence: 0.5})
		n3, e3 = fact(t, facttest.Bid{Candidate: true, Amount: 800_000})
		n4, e4 = fact(t, facttest.Bid{Winner: org1, Amount: 3_000_000})
		lot, e5 = fact(t, facttest.Bid{Lot: "L1", Amount: 2_000_000}, facttest.Bid{Lot: "L1"})
		n6, e6 = fact(t, facttest.Bid{Location: "110105", Amount: 9_000_000})
		_ = env
		for _, e := range []outbox.Envelope{pursueEnv, e2, e3, e4, e5, e6} {
			mustMatch(t, e)
		}
		pursue, review, watch, excluded = sig(t, n1)[0], sig(t, n2)[0], sig(t, n3)[0], sig(t, n4)[0]
		lots := sig(t, lot)
		if len(lots) != 2 || len(sig(t, n6)) != 0 || len(sig(t, before)) != 0 {
			t.Fatalf("%v %v", lots, sig(t, n6))
		}
		lotA, lotB = lots[0], lots[1]
		if count(t, detOwner, `SELECT count(*) FROM det_matches WHERE unit_key LIKE '%'||$1||'%' AND outcome = 'no_signal'`, n6) != 1 {
			t.Fatal("服务区域外的结果没有 no_signal 匹配记录")
		}
		var policyID string
		if err := ccOwner.QueryRow(ctx, `SELECT policy_id FROM ctx_policies WHERE objective_id = $1`, objective.ID).Scan(&policyID); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{pursue: "pursue_candidate", review: "review_candidate", watch: "watch_signal", excluded: "excluded"}
		for id, d := range want {
			r := row(t, dm, id)
			pol := r["policy"].(map[string]any)
			if r["disposition"] != d || num(pol, "version") != 1 || num(pol, "generation") != 1 || pol["policy_id"] != policyID ||
				r["objective_id"] != objective.ID {
				t.Fatalf("%s：%+v", d, r)
			}
		}
		if r := row(t, dm, excluded); r["reason_code"] != "SELF_IS_WINNER" {
			t.Fatalf("%+v", r)
		}
		// 业务时间与记录时间分开：evaluated_at 为触发事件的时间，created_at 为写入时间
		r := row(t, dm, pursue)
		evaluated, _ := time.Parse(time.RFC3339Nano, r["evaluated_at"].(string))
		created, _ := time.Parse(time.RFC3339Nano, r["created_at"].(string))
		if !evaluated.Equal(pursueEnv.Time.Truncate(time.Second)) || !created.After(evaluated) {
			t.Fatalf("%v %v %v", evaluated, created, pursueEnv.Time)
		}
		var ctxVersion int64
		if err := detOwner.QueryRow(ctx, `SELECT context_version FROM det_signals WHERE id = $1`, pursue).Scan(&ctxVersion); err != nil || ctxVersion < 1 {
			t.Fatalf("%d %v", ctxVersion, err)
		}
		// 信号事件：固定的输入随事件送达 learning
		var detected int
		for _, e := range delivered {
			if e.Type == "detection.signal.detected.v1" {
				detected++
				var d map[string]any
				_ = json.Unmarshal(e.Data, &d)
				if num(d, "policy_generation") != 1 || num(d, "context_snapshot_version") != ctxVersion || d["objective_id"] != objective.ID {
					t.Fatalf("%+v", d)
				}
			}
		}
		if detected != 6 || count(t, lrnOwner, `SELECT count(*) FROM lrn_signals`) != 6 {
			t.Fatalf("%d", detected)
		}
		// 不同目标的信号不混排：另一个目标（未激活）的收件箱为空，本目标的每一行都属于本目标
		var other cc.Objective
		decode(t, expect(t, dm, false, "POST", "/v1/customer-context/objectives", `{"template_id":"APP-TENDER.A01","org_unit_id":"root","participants":[]}`, 200), &other)
		pump(t)
		if p := obj(t, expect(t, dm, false, "GET", "/v1/detection/objectives/"+other.ID+"/signals", "", 200)); len(rowsOf(p)) != 0 {
			t.Fatalf("%+v", p)
		}
		for _, r := range rowsOf(page(t, dm, "?tab=open")) {
			if r["objective_id"] != objective.ID {
				t.Fatalf("%+v", r)
			}
		}
	})

	// 他租户中的一条信号（以拥有者账号复制本租户的一行到 t2，模拟他租户的数据）
	foreign := ids.New("sig")
	if _, err := detOwner.Exec(ctx, `INSERT INTO det_signals SELECT (jsonb_populate_record(NULL::det_signals, to_jsonb(s) || jsonb_build_object('id', $2::text, 'tenant_id', $3::text))).*
 FROM det_signals s WHERE id = $1`, pursue, foreign, t2); err != nil {
		t.Fatal(err)
	}

	t.Run("用例2、3、4：参与的业务开拓者可看可反馈；非参与人看不到；参与的业务专家与管理员只读；非参与的管理员看不到信号；跨租户与不存在相同", func(t *testing.T) {
		if p := page(t, bd, ""); p["viewer_can_feedback"] != true || len(rowsOf(p)) == 0 {
			t.Fatalf("%+v", p)
		}
		for _, who := range []string{expert, admin} {
			if p := page(t, who, ""); p["viewer_can_feedback"] != false || len(rowsOf(p)) == 0 {
				t.Fatalf("%s：%+v", who, p)
			}
			detail(t, who, pursue)
			expect(t, who, false, "GET", "/v1/detection/signals/"+pursue+"/explanation", "", 200)
			triage(t, who, pursue, "dismiss", nil, 403, "forbidden")
			batch(t, who, map[string]any{"action": "dismiss", "items": batchItems(t, who, []string{pursue})}, "", 403, "forbidden")
			expect(t, who, false, "GET", "/v1/detection/objectives/"+objective.ID+"/signal-selection", "", 403, "forbidden")
		}
		expect(t, adminx, false, "GET", inbox, "", 403, "forbidden")
		expect(t, adminx, false, "GET", "/v1/detection/signals/"+pursue, "", 404, "signal_not_found")
		expect(t, bd2, false, "GET", inbox, "", 404, "objective_not_found")
		missing := ids.New("sig")
		for _, path := range []string{"", "/explanation", "/triage-records"} {
			for _, id := range []string{pursue, foreign, missing} {
				expect(t, bd2, false, "GET", "/v1/detection/signals/"+id+path, "", 404, "signal_not_found")
				expectKey(t, admin2, t2, false, "GET", "/v1/detection/signals/"+id+path, "", "", 404, "signal_not_found")
			}
			expect(t, bd, false, "GET", "/v1/detection/signals/"+foreign+path, "", 404, "signal_not_found")
			expect(t, bd, false, "GET", "/v1/detection/signals/"+missing+path, "", 404, "signal_not_found")
		}
		body := `{"operation_id":"op-foreign-0001","action":"dismiss","seen_content_version":0,"state_version":0,"feedback_version":0}`
		a := expect(t, bd, false, "POST", "/v1/detection/signals/"+foreign+"/triage", body, 404, "signal_not_found")
		b := expectKey(t, bd, t1, false, "POST", "/v1/detection/signals/"+missing+"/triage", strings.Replace(body, "0001", "0002", 1), "", 404, "signal_not_found")
		if string(a) != string(b) {
			t.Fatalf("他租户与不存在的结果不同：%s %s", a, b)
		}
		expectKey(t, admin2, t2, false, "GET", inbox, "", "", 404, "objective_not_found")
		expectKey(t, admin2, t2, false, "GET", "/v1/learning/objectives/"+objective.ID+"/feedback-summary", "", "", 404, "objective_not_found")
		// 批量中混入的他租户标识逐项失败，不返回其内容
		items := batchItems(t, bd, []string{watch})
		items = append(items, map[string]any{"signal_id": foreign, "seen_content_version": 0, "state_version": 0, "feedback_version": 0})
		res := batch(t, bd, map[string]any{"action": "relevant", "reasons": []string{"NEW_LEAD"}, "items": items}, "", 202)
		it := itemsOf(res)
		if it[foreign]["result"] != "failed" || it[foreign]["failure"] != "definite" || it[foreign]["code"] != "signal_not_found" || it[watch]["result"] != "succeeded" {
			t.Fatalf("%+v", res)
		}
		if raw, _ := json.Marshal(res); strings.Contains(string(raw), "夹具") {
			t.Fatalf("批量结果包含信号内容：%s", raw)
		}
		// 恢复 watch 为未反馈（撤销该批次）
		undo(t, bd, map[string]any{"target_batch_id": res["id"]}, 200)
		if feedbackOf(t, bd, watch) != nil {
			t.Fatal("撤销后仍有反馈")
		}
	})

	t.Run("参与人撤权立即生效：移除参与人后即使 identity 同步失败，收件箱、详情与处理也立即为不存在；授权不可用时为 503 而非无权限", func(t *testing.T) {
		syncDir.fail.Store(true)
		hold["customer_context.objective.changed.v1"] = true
		setParticipants(t, admin, expert)
		pump(t)
		expect(t, bd, false, "GET", inbox, "", 404, "objective_not_found")
		expect(t, bd, false, "GET", "/v1/detection/signals/"+pursue, "", 404, "signal_not_found")
		body := fmt.Sprintf(`{"operation_id":"%s","action":"dismiss","seen_content_version":0,"state_version":0,"feedback_version":0}`, opID())
		expect(t, bd, false, "POST", "/v1/detection/signals/"+pursue+"/triage", body, 404, "signal_not_found")
		expect(t, bd, false, "GET", "/v1/learning/objectives/"+objective.ID+"/feedback-summary", "", 404, "objective_not_found")
		page(t, dm, "") // 单元角色不受影响
		syncDir.fail.Store(false)
		delete(hold, "customer_context.objective.changed.v1")
		for _, e := range pending {
			deliver(t, e)
		}
		pending = nil
		setParticipants(t, bd, admin, expert)
		pump(t)
		page(t, bd, "")
		access.fail.Store(true)
		expect(t, bd, false, "GET", inbox, "", 503)
		expect(t, bd, false, "GET", "/v1/detection/signals/"+pursue, "", 503)
		access.fail.Store(false)
	})

	t.Run("用例5、6、7：详情显示处置结论、逐条规则与分值；三类未知原因；固定的求值输入", func(t *testing.T) {
		d := detail(t, bd, pursue)
		ex := d["explanation"].(map[string]any)
		score := 50
		for _, tr := range ex["trace"].([]any) {
			m := tr.(map[string]any)
			if m["result"] == "true" && m["clause"] == "boost" {
				score += int(num(m, "weight"))
			}
			if m["result"] == "true" && m["clause"] == "penalize" {
				score -= int(num(m, "weight"))
			}
		}
		if int(num(ex, "score")) != score || score != 70 || ex["disposition"] != "pursue_candidate" || ex["conclusion"] == "" {
			t.Fatalf("得分与轨迹不一致：%d %+v", score, ex)
		}
		in := ex["inputs"].(map[string]any)
		names := map[string]bool{}
		for _, v := range in["view_items"].([]any) {
			names[v.(map[string]any)["name"].(string)] = true
		}
		if in["snapshot_available"] != true || !names["fact"] || !names["winner.profile"] || num(in, "context_snapshot_version") < 1 ||
			!strings.HasPrefix(in["trigger_event_id"].(string), "evt_") || in["trigger_event_id"] != pursueEnv.ID {
			t.Fatalf("%+v", in)
		}
		if enabled := in["policy"].(map[string]any)["enabled_domains"].([]any); slices.Contains(enabled, any("INF-CRED")) {
			t.Fatalf("%v", enabled)
		}
		ex = obj(t, expect(t, bd, false, "GET", "/v1/detection/signals/"+review+"/explanation", "", 200))
		got := map[string]string{}
		for _, tr := range ex["trace"].([]any) {
			m := tr.(map[string]any)
			reason, _ := m["unknown_reason"].(string)
			got[m["id"].(string)] = m["result"].(string) + "/" + reason
		}
		want := map[string]string{"R-CAT": "unknown/CATEGORY_UNCERTAIN", "R-WINNER": "unknown/BELOW_EVIDENCE", "X-SELF": "unknown/BELOW_EVIDENCE",
			"P-CREDIT": "unknown/DOMAIN_NOT_ENABLED", "R-REGION": "true/"}
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s：得到 %s，期望 %s", id, got[id], w)
			}
		}
		if ex["disposition"] != "review_candidate" || ex["needs_review"] != true {
			t.Fatalf("%+v", ex)
		}
		if r := row(t, bd, review); num(r, "unknown_count") < 3 {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("用例29：同标包多家中标每家一条，行内标同标包家数；金额未公布时不出现金额", func(t *testing.T) {
		a, b := row(t, bd, lotA), row(t, bd, lotB)
		if num(a, "same_lot_count") != 2 || num(b, "same_lot_count") != 2 {
			t.Fatalf("%+v %+v", a, b)
		}
		_, hasA := a["amount"]
		_, hasB := b["amount"]
		if hasA == hasB {
			t.Fatalf("恰有一家金额未公布：%+v %+v", a, b)
		}
		if num(row(t, bd, pursue), "same_lot_count") != 1 {
			t.Fatal("没有标包的结果计为 1")
		}
	})

	var n8env, n9env outbox.Envelope
	var n8, n9 string
	t.Run("第7段用例15：读取生效策略失败时该批重试、不产出信号；激活返回后立即到达的公告按新版本求值（激活事件尚未送达）", func(t *testing.T) {
		n8, n8env = fact(t, facttest.Bid{Amount: 1_500_000})
		active.fail.Store(true)
		if err := match(n8env); err == nil {
			t.Fatal("读取失败时匹配应返回错误以便重试")
		}
		if len(sig(t, n8)) != 0 || count(t, detOwner, `SELECT count(*) FROM det_matches WHERE unit_key LIKE '%'||$1||'%'`, n8) != 0 {
			t.Fatal("读取失败时按缓存产出了信号或匹配记录")
		}
		active.fail.Store(false)
		mustMatch(t, n8env)
		if s := sig(t, n8); len(s) != 1 || num(row(t, dm, s[0])["policy"].(map[string]any), "generation") != 1 {
			t.Fatalf("%v", s)
		}
		hold["customer_context.policy.activated.v1"] = true
		activate(t, "第二版")
		if p := getPolicy(t); p.Generation != 2 || p.ActiveVersion != 2 {
			t.Fatalf("%+v", p)
		}
		n9, n9env = fact(t, facttest.Bid{Amount: 1_600_000})
		mustMatch(t, n9env)
		pol := row(t, dm, sig(t, n9)[0])["policy"].(map[string]any)
		if num(pol, "version") != 2 || num(pol, "generation") != 2 {
			t.Fatalf("%+v", pol)
		}
		delete(hold, "customer_context.policy.activated.v1")
		for _, e := range pending {
			deliver(t, e)
		}
		pending = nil
	})

	t.Run("第7段用例16、第3段用例8：激活与回滚不重算已有信号；重复投递不按新代次再求值；旧版本产生的信号保留原版本与解释", func(t *testing.T) {
		matches := count(t, detOwner, `SELECT count(*) FROM det_matches`)
		before := detail(t, dm, pursue)
		mustMatch(t, pursueEnv)
		mustMatch(t, n8env)
		if count(t, detOwner, `SELECT count(*) FROM det_matches`) != matches {
			t.Fatal("重复投递产生了新的匹配记录")
		}
		after := detail(t, dm, pursue)
		a, _ := json.Marshal(before["explanation"])
		b, _ := json.Marshal(after["explanation"])
		if string(a) != string(b) || num(after["signal"].(map[string]any)["policy"].(map[string]any), "version") != 1 {
			t.Fatalf("%s\n%s", a, b)
		}
		var rb cc.PolicyRequest
		decode(t, expect(t, dm, true, "POST", policyPath+"/rollback-requests", `{"target_version":1,"note":"回到第一版"}`, 200), &rb)
		approve(t, rb.ApprovalID)
		pump(t)
		if p := getPolicy(t); p.Generation != 3 || p.ActiveVersion != 1 {
			t.Fatalf("%+v", p)
		}
		mustMatch(t, n9env)
		pol := row(t, dm, sig(t, n9)[0])["policy"].(map[string]any)
		if num(pol, "version") != 2 || num(pol, "generation") != 2 {
			t.Fatalf("回滚后重算了已有信号：%+v", pol)
		}
		n10, e10 := fact(t, facttest.Bid{Amount: 1_700_000})
		mustMatch(t, e10)
		pol = row(t, dm, sig(t, n10)[0])["policy"].(map[string]any)
		if num(pol, "version") != 1 || num(pol, "generation") != 3 {
			t.Fatalf("%+v", pol)
		}
	})

	t.Run("用例11、37：不相关必须选包内原因，“其他”须写说明；不勾选忽略时只记录反馈；已被同事忽略的信号接受本人的新反馈", func(t *testing.T) {
		triage(t, bd, lotA, "feedback", fb("irrelevant"), 422, "invalid_feedback")
		triage(t, bd, lotA, "feedback", fb("irrelevant", "OTHER"), 422, "invalid_feedback")
		triage(t, bd, lotA, "feedback", fb("irrelevant", "NOT_IN_PACK"), 422, "invalid_feedback")
		triage(t, bd, lotA, "feedback", fb("relevant", "TOO_SMALL"), 422, "invalid_feedback")
		other := fb("irrelevant", "OTHER")
		other["feedback"].(map[string]any)["note"] = "夹具说明"
		res := triage(t, bd, lotA, "feedback", other, 200)
		if res["result"] != "succeeded" || row(t, bd, lotA)["status"] != "open" {
			t.Fatalf("%+v", res)
		}
		// 同事忽略后，本人“不相关 + 忽略”：处理状态无变化、反馈照常写入
		triage(t, dm, lotB, "dismiss", nil, 200)
		with := fb("irrelevant", "TOO_SMALL")
		with["dismiss"] = true
		res = triage(t, bd, lotB, "feedback", with, 200)
		parts := res["parts"].(map[string]any)
		if res["result"] != "succeeded" || parts["status"].(map[string]any)["change"] != "unchanged" || parts["feedback"].(map[string]any)["change"] != "changed" {
			t.Fatalf("%+v", res)
		}
		res = triage(t, dm, lotB, "feedback", fb("relevant", "ALREADY_KNOWN"), 200)
		if res["result"] != "succeeded" {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("用例12、13、34：修改与撤回都追加记录、以最新为准；同一幂等键与同一操作标识都只产生一条记录；两名成员各自保存", func(t *testing.T) {
		body := triageBody(t, bd, pursue, "feedback", fb("relevant", "NEW_LEAD"))
		key := "same-key-0000000001"
		s1, r1 := triageRaw(t, bd, pursue, body, key)
		s2, r2 := triageRaw(t, bd, pursue, body, key)
		if s1 != 200 || s2 != 200 || string(r1) != string(r2) {
			t.Fatalf("%d %s\n%d %s", s1, r1, s2, r2)
		}
		// 已提交而响应丢失：新幂等键、同一操作标识重试返回已提交的结果
		s3, r3 := triageRaw(t, bd, pursue, body, "")
		if s3 != 200 || string(r3) != string(r1) || opsCount(t, `operation_id = $1`, body["operation_id"]) != 1 {
			t.Fatalf("%d %s", s3, r3)
		}
		changed := map[string]any{}
		for k, v := range body {
			changed[k] = v
		}
		changed["feedback"] = map[string]any{"result": "relevant", "reasons": []string{"ALREADY_KNOWN"}}
		if s, raw := triageRaw(t, bd, pursue, changed, ""); s != 409 || codeOf(raw) != "operation_conflict" {
			t.Fatalf("%d %s", s, raw)
		}
		if s, raw := triageRaw(t, dm, pursue, body, ""); s != 409 || codeOf(raw) != "operation_conflict" {
			t.Fatalf("他人使用同一操作标识：%d %s", s, raw)
		}
		if s, raw := triageRaw(t, bd, pursue, changed, key); s != 409 || codeOf(raw) != "idempotency_conflict" {
			t.Fatalf("%d %s", s, raw)
		}
		triage(t, bd, pursue, "feedback", fb("irrelevant", "CATEGORY_MISMATCH"), 200)
		triage(t, bd, pursue, "withdraw_feedback", nil, 200)
		if feedbackOf(t, bd, pursue) != nil || num(row(t, bd, pursue), "feedback_version") != 3 {
			t.Fatalf("%+v", row(t, bd, pursue))
		}
		triage(t, bd, pursue, "feedback", fb("relevant", "NEW_LEAD"), 200)
		triage(t, dm, pursue, "feedback", fb("irrelevant", "TOO_SMALL"), 200)
		d := detail(t, dm, pursue)
		if len(d["feedback"].([]any)) != 2 {
			t.Fatalf("%+v", d["feedback"])
		}
		if n := opsCount(t, `signal_id = $1 AND actor = $2 AND action IN ('feedback','withdraw_feedback') AND outcome = 'succeeded'`, pursue, bd); n != 4 {
			t.Fatalf("记录 %d 条", n)
		}
		recs := obj(t, expect(t, dm, false, "GET", "/v1/detection/signals/"+pursue+"/triage-records", "", 200))
		if len(recs["records"].([]any)) != 5 || recs["next_cursor"] != nil || recs["records"].([]any)[0].(map[string]any)["actor"] != dm {
			t.Fatalf("%+v", recs)
		}
		// 记录分页（每页 50 条，游标绑定信号）：在另一条信号上产生 52 条记录
		n10 := sigN10(t, detOwner, ctx)
		for i := range 52 {
			action := "dismiss"
			if i%2 == 1 {
				action = "restore"
			}
			triage(t, dm, n10, action, nil, 200)
		}
		first := obj(t, expect(t, dm, false, "GET", "/v1/detection/signals/"+n10+"/triage-records", "", 200))
		next, _ := first["next_cursor"].(string)
		if len(first["records"].([]any)) != 50 || next == "" {
			t.Fatalf("%d", len(first["records"].([]any)))
		}
		second := obj(t, expect(t, dm, false, "GET", "/v1/detection/signals/"+n10+"/triage-records?cursor="+url.QueryEscape(next), "", 200))
		if len(second["records"].([]any)) != 2 || second["next_cursor"] != nil {
			t.Fatalf("%+v", second)
		}
		expect(t, dm, false, "GET", "/v1/detection/signals/"+pursue+"/triage-records?cursor="+url.QueryEscape(next), "", 400, "invalid_cursor")
	})

	t.Run("用例33：暂时失败不固定在操作标识上；原幂等键重发返回那次失败，新幂等键同一操作标识重试成功，只有一条记录", func(t *testing.T) {
		body := triageBody(t, bd, n8sig(t, sig, n8), "feedback", fb("relevant", "NEW_LEAD"))
		access.fail.Store(true)
		key := "transient-key-00001"
		s1, r1 := triageRaw(t, bd, sig(t, n8)[0], body, key)
		access.fail.Store(false)
		s2, r2 := triageRaw(t, bd, sig(t, n8)[0], body, key)
		if s1 != 503 || s2 != 503 || string(r1) != string(r2) {
			t.Fatalf("%d %s %d %s", s1, r1, s2, r2)
		}
		if s, raw := triageRaw(t, bd, sig(t, n8)[0], body, ""); s != 200 {
			t.Fatalf("%d %s", s, raw)
		}
		if opsCount(t, `operation_id = $1`, body["operation_id"]) != 1 {
			t.Fatal("重复记录")
		}
	})

	t.Run("用例14、36：并发忽略一人成功一人无变化；忽略与调整处置并发时后者冲突并得到当前值；并发调为不同处置冲突、相同处置无变化", func(t *testing.T) {
		race := func(a, b func() (int, []byte)) [2]struct {
			status int
			raw    []byte
		} {
			var out [2]struct {
				status int
				raw    []byte
			}
			gate := make(chan struct{})
			var wg sync.WaitGroup
			for i, f := range []func() (int, []byte){a, b} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-gate
					out[i].status, out[i].raw = f()
				}()
			}
			close(gate)
			wg.Wait()
			return out
		}
		target := n9sig(t, sig, n9)
		b1 := triageBody(t, bd, target, "dismiss", nil)
		b2 := triageBody(t, dm, target, "dismiss", nil)
		res := race(func() (int, []byte) { return triageRaw(t, bd, target, b1, "") }, func() (int, []byte) { return triageRaw(t, dm, target, b2, "") })
		results := []string{}
		for _, r := range res {
			if r.status != 200 {
				t.Fatalf("%d %s", r.status, r.raw)
			}
			results = append(results, obj(t, r.raw)["result"].(string))
		}
		slices.Sort(results)
		if !slices.Equal(results, []string{"no_change", "succeeded"}) || opsCount(t, `signal_id = $1 AND action = 'dismiss' AND outcome = 'succeeded'`, target) != 1 {
			t.Fatalf("%v", results)
		}
		triage(t, bd, target, "restore", nil, 200)
		b1 = triageBody(t, dm, target, "dismiss", nil)
		b2 = triageBody(t, bd, target, "adjust", map[string]any{"manual_disposition": "watch_signal", "reason": "先观察"})
		res = race(func() (int, []byte) { return triageRaw(t, dm, target, b1, "") }, func() (int, []byte) { return triageRaw(t, bd, target, b2, "") })
		statuses := []int{res[0].status, res[1].status}
		slices.Sort(statuses)
		if !slices.Equal(statuses, []int{200, 409}) {
			t.Fatalf("%d %s %d %s", res[0].status, res[0].raw, res[1].status, res[1].raw)
		}
		for _, r := range res {
			if r.status == 409 {
				p := obj(t, r.raw)
				if p["code"] != "conflict" || p["current"] == nil {
					t.Fatalf("%s", r.raw)
				}
			}
		}
		// 回到待处理，再并发调为不同处置
		r := row(t, dm, target)
		if r["status"] == "dismissed" {
			triage(t, dm, target, "restore", nil, 200)
		} else {
			undoOp := obj(t, expect(t, bd, false, "GET", "/v1/detection/signals/"+target+"/triage-records?limit=1", "", 200))["records"].([]any)[0].(map[string]any)
			undo(t, bd, map[string]any{"target_operation_id": undoOp["operation_id"]}, 200)
		}
		b1 = triageBody(t, dm, target, "adjust", map[string]any{"manual_disposition": "pursue_candidate", "reason": "值得跟进"})
		b2 = triageBody(t, bd, target, "adjust", map[string]any{"manual_disposition": "watch_signal", "reason": "先观察"})
		res = race(func() (int, []byte) { return triageRaw(t, dm, target, b1, "") }, func() (int, []byte) { return triageRaw(t, bd, target, b2, "") })
		statuses = []int{res[0].status, res[1].status}
		slices.Sort(statuses)
		if !slices.Equal(statuses, []int{200, 409}) {
			t.Fatalf("%d %s %d %s", res[0].status, res[0].raw, res[1].status, res[1].raw)
		}
		cur := row(t, dm, target)
		b1 = triageBody(t, dm, target, "adjust", map[string]any{"manual_disposition": "review_candidate", "reason": "复核"})
		b2 = triageBody(t, bd, target, "adjust", map[string]any{"manual_disposition": "review_candidate", "reason": "复核"})
		res = race(func() (int, []byte) { return triageRaw(t, dm, target, b1, "") }, func() (int, []byte) { return triageRaw(t, bd, target, b2, "") })
		results = nil
		for _, r := range res {
			if r.status != 200 {
				t.Fatalf("%d %s", r.status, r.raw)
			}
			results = append(results, obj(t, r.raw)["result"].(string))
		}
		slices.Sort(results)
		if !slices.Equal(results, []string{"no_change", "succeeded"}) || num(row(t, dm, target), "state_version") != num(cur, "state_version")+1 {
			t.Fatalf("%v", results)
		}
	})

	t.Run("用例15、16：调整处置须写原因，系统处置、得分与轨迹不变；排除信号只能反馈排除正确或有误", func(t *testing.T) {
		triage(t, dm, review, "adjust", map[string]any{"manual_disposition": "pursue_candidate"}, 422, "invalid_feedback")
		before := detail(t, dm, review)
		triage(t, dm, review, "adjust", map[string]any{"manual_disposition": "pursue_candidate", "reason": "已与中标方接触"}, 200)
		after := detail(t, dm, review)
		r := after["signal"].(map[string]any)
		if r["disposition"] != "review_candidate" || r["manual_disposition"] != "pursue_candidate" || r["status"] != "triaged" ||
			num(r, "score") != num(before["signal"].(map[string]any), "score") || after["manual_reason"] != "已与中标方接触" {
			t.Fatalf("%+v", after)
		}
		a, _ := json.Marshal(before["explanation"])
		b, _ := json.Marshal(after["explanation"])
		if string(a) != string(b) {
			t.Fatal("调整处置改变了解释")
		}
		triage(t, bd, excluded, "feedback", fb("relevant", "NEW_LEAD"), 422, "not_applicable")
		triage(t, bd, excluded, "dismiss", nil, 422, "not_applicable")
		triage(t, bd, excluded, "adjust", map[string]any{"manual_disposition": "pursue_candidate", "reason": "试"}, 422, "not_applicable")
		triage(t, bd, excluded, "feedback", fb("exclusion_wrong"), 422, "invalid_feedback")
		triage(t, bd, excluded, "feedback", fb("exclusion_wrong", "WINNER_NOT_SELF"), 200)
		triage(t, dm, excluded, "feedback", fb("exclusion_correct"), 200)
		if rows := rowsOf(page(t, bd, "?tab=excluded")); len(rows) != 1 || rows[0]["id"] != excluded {
			t.Fatalf("%+v", rows)
		}
		if rows := rowsOf(page(t, bd, "?tab=open")); slices.ContainsFunc(rows, func(r map[string]any) bool { return r["id"] == excluded }) {
			t.Fatal("排除信号出现在待处理")
		}
	})

	t.Run("用例9、10：内容更新后旧内容版本的反馈被拒绝，刷新后重新确认成功，已有反馈标为基于更新前的版本；已关闭信号不能忽略或调整，仍可反馈", func(t *testing.T) {
		triage(t, bd, watch, "feedback", fb("relevant", "NEW_LEAD"), 200)
		stale := triageBody(t, dm, watch, "feedback", fb("relevant", "NEW_LEAD"))
		// 重新求值的生产者尚未实现：以拥有者账号写入一次内容更新
		if _, err := detOwner.Exec(ctx, `UPDATE det_signals SET content_version = content_version + 1, material_updated_at = now() WHERE id = $1;`, watch); err != nil {
			t.Fatal(err)
		}
		if _, err := detOwner.Exec(ctx, `INSERT INTO det_signal_updates (tenant_id, signal_id, content_version, trigger, changed_fields, disposition_before, disposition_after, material)
 VALUES ($1, $2, 1, 'tendering.result.corrected', '{amount}', 'watch_signal', 'watch_signal', true)`, t1, watch); err != nil {
			t.Fatal(err)
		}
		if s, raw := triageRaw(t, dm, watch, stale, ""); s != 409 || codeOf(raw) != "signal_updated" || obj(t, raw)["current"] == nil {
			t.Fatalf("%d %s", s, raw)
		}
		d := detail(t, dm, watch)
		if len(d["updates"].([]any)) != 1 || d["signal"].(map[string]any)["updated_recently"] != true {
			t.Fatalf("%+v", d)
		}
		if f := feedbackOf(t, bd, watch); f == nil || num(f, "based_on_content_version") != 0 {
			t.Fatalf("%+v", f)
		}
		triage(t, dm, watch, "feedback", fb("relevant", "NEW_LEAD"), 200)
		if rows := rowsOf(page(t, dm, "?marker=updated")); len(rows) != 1 || rows[0]["id"] != watch {
			t.Fatalf("%+v", rows)
		}
		// 公告撤回：信号关闭（撤回的生产者随重新求值实现，以拥有者账号写入）
		if _, err := detOwner.Exec(ctx, `UPDATE det_signals SET status = 'closed', state_version = state_version + 1 WHERE id = $1`, lotA); err != nil {
			t.Fatal(err)
		}
		triage(t, bd, lotA, "dismiss", nil, 409, "signal_closed")
		triage(t, dm, lotA, "adjust", map[string]any{"manual_disposition": "watch_signal", "reason": "试"}, 409, "signal_closed")
		res := triage(t, dm, lotA, "feedback", fb("irrelevant", "TOO_LATE"), 200)
		if res["parts"].(map[string]any)["feedback"].(map[string]any)["change"] != "changed" {
			t.Fatalf("%+v", res)
		}
		with := fb("irrelevant", "TOO_LATE")
		with["dismiss"] = true
		res = triage(t, bd, lotA, "feedback", with, 200)
		if res["parts"].(map[string]any)["status"].(map[string]any)["change"] != "not_applicable" {
			t.Fatalf("%+v", res)
		}
		if rows := rowsOf(page(t, dm, "?tab=closed")); len(rows) != 1 || rows[0]["id"] != lotA {
			t.Fatalf("%+v", rows)
		}
	})

	t.Run("收件箱筛选与游标分页：每页不重复不遗漏；游标绑定筛选；区域、品类、金额、本人反馈、未知与排序", func(t *testing.T) {
		all := rowsOf(page(t, dm, "?tab=open&limit=50"))
		var seen []string
		cursor := ""
		for i := 0; i < 20; i++ {
			q := "?tab=open&limit=2"
			if cursor != "" {
				q += "&cursor=" + url.QueryEscape(cursor)
			}
			p := page(t, dm, q)
			for _, r := range rowsOf(p) {
				seen = append(seen, r["id"].(string))
			}
			next, _ := p["next_cursor"].(string)
			if next == "" {
				break
			}
			cursor = next
		}
		if len(seen) != len(all) || len(all) < 4 {
			t.Fatalf("%d %d", len(seen), len(all))
		}
		for i, r := range all {
			if seen[i] != r["id"] {
				t.Fatalf("分页顺序与整页不同：%v", seen)
			}
			if i > 0 && num(all[i-1], "score") < num(r, "score") {
				t.Fatal("没有按排序分降序")
			}
		}
		p := page(t, dm, "?tab=open&limit=1")
		expect(t, dm, false, "GET", inbox+"?tab=handled&limit=1&cursor="+url.QueryEscape(p["next_cursor"].(string)), "", 400, "invalid_cursor")
		if rows := rowsOf(page(t, dm, "?region=110000")); len(rows) != 0 {
			t.Fatalf("%+v", rows)
		}
		if rows := rowsOf(page(t, dm, "?tab=open&region=330100&category=CAT-PIPE")); len(rows) != len(all) {
			t.Fatalf("%d", len(rows))
		}
		for _, r := range rowsOf(page(t, dm, "?tab=open&amount_min=5000000")) {
			if r["id"] != pursue {
				t.Fatalf("%+v", r)
			}
		}
		for _, r := range rowsOf(page(t, bd, "?my_feedback=relevant")) {
			if r["my_feedback"].(map[string]any)["result"] != "relevant" {
				t.Fatalf("%+v", r)
			}
		}
		if rows := rowsOf(page(t, dm, "?marker=unknown")); !slices.ContainsFunc(rows, func(r map[string]any) bool { return r["id"] == review }) {
			t.Fatal("未知标记筛选缺少复核候选")
		}
		amounts := rowsOf(page(t, dm, "?tab=open&sort=amount"))
		for i := 1; i < len(amounts); i++ {
			prev, _ := amounts[i-1]["amount"].(map[string]any)
			if prev == nil {
				if _, ok := amounts[i]["amount"]; ok {
					t.Fatal("金额未公布的没有排在后面")
				}
			}
		}
	})

	t.Run("用例24：learning 暂不可用时反馈照常提交；恢复后补齐且重复投递不重复计入", func(t *testing.T) {
		pump(t)
		hold["detection.signal.triaged.v1"] = true
		triage(t, ra, review, "feedback", fb("relevant", "NEW_LEAD"), 200)
		pump(t)
		held := slices.Clone(pending)
		pending = nil
		delete(hold, "detection.signal.triaged.v1")
		if len(held) != 1 || count(t, lrnOwner, `SELECT count(*) FROM lrn_feedback WHERE signal_id = $1 AND member = $2`, review, ra) != 0 {
			t.Fatalf("%d", len(held))
		}
		for range 2 {
			for _, e := range held {
				deliver(t, e)
			}
		}
		if count(t, lrnOwner, `SELECT count(*) FROM lrn_feedback WHERE signal_id = $1 AND member = $2`, review, ra) != 1 ||
			count(t, lrnOwner, `SELECT count(*) FROM lrn_feedback_ops WHERE operation_id = $1`, data(held[0])["operation_id"]) != 1 {
			t.Fatal("补齐后重复计入")
		}
	})

	t.Run("用例12、22、23、40：反馈汇总的分歧、旧版本反馈单列、排除反馈；查看不产生反馈；反馈不改变策略与系统处置；窗口按发现时间", func(t *testing.T) {
		policy := getPolicy(t)
		for range 3 {
			detail(t, ra, pursue)
		}
		if count(t, detOwner, `SELECT count(*) FROM det_feedback WHERE signal_id = $1 AND member = $2`, pursue, ra) != 0 {
			t.Fatal("查看产生了反馈")
		}
		pump(t)
		sum := obj(t, expect(t, bd, false, "GET", "/v1/learning/objectives/"+objective.ID+"/feedback-summary", "", 200))
		raw, _ := json.Marshal(sum)
		sig := sum["signals"].(map[string]any)
		rel := sum["relevance"].(map[string]any)
		exc := sum["exclusions"].(map[string]any)
		// pursue：bd 相关、dm 不相关 → 分歧；lotB：bd 不相关、dm 相关 → 分歧；watch：bd 基于旧版本（单列）、dm 相关；review：ra 相关
		if num(rel, "disagreement") != 2 || num(sum, "stale_feedback") != 1 || num(exc, "with_feedback") != 1 || num(exc, "disagreement") != 1 ||
			sum["sample_sufficient"] != false || rel["relevant_ratio"] != nil {
			t.Fatalf("%s", raw)
		}
		if num(sig, "total") != num(sig, "with_feedback")+num(sig, "without_feedback") || num(sig, "without_feedback") < 1 {
			t.Fatalf("%s", raw)
		}
		reasons := map[string]int64{}
		for _, r := range sum["reasons"].([]any) {
			m := r.(map[string]any)
			reasons[m["code"].(string)] = num(m, "signals")
		}
		// 原因只计当前版本、无分歧的不相关：lotA 两人都为“时机已过”
		if len(reasons) != 1 || reasons["TOO_LATE"] != 1 || num(rel, "relevant") != 3 || num(rel, "irrelevant") != 1 || num(sum, "sample") != 6 {
			t.Fatalf("%s", raw)
		}
		// 窗口按发现时间近 30 天：把一条信号的发现时间移到窗口外
		total := num(sig, "total")
		if _, err := lrnOwner.Exec(ctx, `UPDATE lrn_signals SET discovered_at = discovered_at - interval '40 days' WHERE signal_id = $1`, sigN10(t, detOwner, ctx)); err != nil {
			t.Fatal(err)
		}
		sum = obj(t, expect(t, bd, false, "GET", "/v1/learning/objectives/"+objective.ID+"/feedback-summary", "", 200))
		if num(sum["signals"].(map[string]any), "total") != total-1 {
			t.Fatalf("%+v", sum)
		}
		expect(t, bd2, false, "GET", "/v1/learning/objectives/"+objective.ID+"/feedback-summary", "", 404, "objective_not_found")
		expect(t, adminx, false, "GET", "/v1/learning/objectives/"+objective.ID+"/feedback-summary", "", 403, "forbidden")
		// 反馈与调整处置不改变策略、草案与系统处置；learning 不创建草案或提案
		after := getPolicy(t)
		if after.Generation != policy.Generation || after.ActiveVersion != policy.ActiveVersion || after.DraftVersion != policy.DraftVersion || after.Draft.Set != policy.Draft.Set {
			t.Fatalf("%+v %+v", policy, after)
		}
		if row(t, dm, pursue)["disposition"] != "pursue_candidate" || num(row(t, dm, pursue), "score") != 70 {
			t.Fatal("反馈改变了系统处置")
		}
		if n := count(t, lrnOwner, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' NOT IN ('learning.simulation.completed.v1', 'audit.entry.recorded.v1')`); n != 0 {
			t.Fatalf("learning 发出了 %d 条其他事件", n)
		}
	})

	// 批量用例的信号：20 条（区域 330110）与 23 条（区域 330109）
	group := func(t *testing.T, n int, location string) []string {
		t.Helper()
		bids := make([]facttest.Bid, n)
		for i := range bids {
			bids[i] = facttest.Bid{Location: location, Amount: int64(1_000_000 + i)}
		}
		notice, env := fact(t, bids...)
		mustMatch(t, env)
		out := sig(t, notice)
		if len(out) != n {
			t.Fatalf("%d", len(out))
		}
		return out
	}
	var g1, g2 []string

	t.Run("用例19、33、37、41：批量部分失败：成功、无变化、确定失败、暂时失败；只重试暂时失败项，已提交项不重复执行；同一幂等键重放同一作业", func(t *testing.T) {
		g1 = group(t, 20, "330110")
		a := g1[:10]
		triage(t, dm, a[0], "dismiss", nil, 200) // 同事已忽略：本人不相关 + 忽略时状态无变化、只写入反馈
		with := fb("irrelevant", "CATEGORY_MISMATCH")
		with["dismiss"] = true
		triage(t, bd, a[3], "feedback", with, 200) // 本人已是相同反馈并已忽略：无变化
		items := batchItems(t, bd, a)
		if _, err := detOwner.Exec(ctx, `UPDATE det_signals SET content_version = content_version + 1 WHERE id = $1`, a[1]); err != nil {
			t.Fatal(err)
		}
		setHook(func(_ string, pos int) error {
			access.fail.Store(pos == 2)
			return nil
		})
		key := "batch-a-key-0000001"
		res := batch(t, bd, map[string]any{"action": "irrelevant", "reasons": []string{"CATEGORY_MISMATCH"}, "items": items}, key, 202)
		setHook(nil)
		access.fail.Store(false)
		it := itemsOf(res)
		if summaryOf(res) != "partial total=10 ok=7 same=1 definite=1 transient=1 unprocessed=0" {
			t.Fatalf("%s：%+v", summaryOf(res), res)
		}
		if it[a[1]]["code"] != "signal_updated" || it[a[1]]["failure"] != "definite" || it[a[2]]["failure"] != "transient" || it[a[3]]["result"] != "no_change" {
			t.Fatalf("%+v", res)
		}
		p0 := it[a[0]]["parts"].(map[string]any)
		if it[a[0]]["result"] != "succeeded" || p0["status"].(map[string]any)["change"] != "unchanged" || p0["feedback"].(map[string]any)["change"] != "changed" {
			t.Fatalf("%+v", it[a[0]])
		}
		if row(t, bd, a[4])["status"] != "dismissed" {
			t.Fatal("不相关缺省同时忽略")
		}
		again := batch(t, bd, map[string]any{"action": "irrelevant", "reasons": []string{"CATEGORY_MISMATCH"}, "items": items}, key, 202)
		if again["id"] != res["id"] || count(t, detOwner, `SELECT count(*) FROM det_signal_batches`) < 1 {
			t.Fatalf("%+v", again)
		}
		id := res["id"].(string)
		expect(t, dm, false, "POST", "/v1/detection/signal-batches/"+id+"/retry", "", 404, "signal_batch_not_found")
		expect(t, dm, false, "GET", "/v1/detection/signal-batches/"+id, "", 404, "signal_batch_not_found")
		retried := obj(t, expect(t, bd, false, "POST", "/v1/detection/signal-batches/"+id+"/retry", "", 202))
		it = itemsOf(retried)
		if summaryOf(retried) != "partial total=10 ok=8 same=1 definite=1 transient=0 unprocessed=0" || num(it[a[2]], "attempts") != 2 ||
			num(it[a[4]], "attempts") != 1 || num(it[a[1]], "attempts") != 1 {
			t.Fatalf("%s %+v", summaryOf(retried), retried)
		}
		if n := opsCount(t, `batch_id = $1 AND outcome = 'succeeded'`, id); n != 8 {
			t.Fatalf("%d", n)
		}
		// 用例41：整批撤销，只写入了反馈的项撤回本人反馈，处理状态保持已忽略
		u := undo(t, bd, map[string]any{"target_batch_id": id}, 200)
		if len(u["items"].([]any)) != 8 {
			t.Fatalf("%+v", u)
		}
		for _, x := range u["items"].([]any) {
			if x.(map[string]any)["result"] != "succeeded" {
				t.Fatalf("%+v", u)
			}
		}
		if r := row(t, bd, a[0]); r["status"] != "dismissed" || r["my_feedback"] != nil {
			t.Fatalf("%+v", r)
		}
		if r := row(t, bd, a[4]); r["status"] != "open" || r["my_feedback"] != nil {
			t.Fatalf("%+v", r)
		}
		if r := row(t, bd, a[3]); r["status"] != "dismissed" || r["my_feedback"] == nil {
			t.Fatalf("无变化的项不在撤销范围：%+v", r)
		}
	})

	t.Run("用例20：执行中成员失去 can_feedback 后之后的项逐项失败；作业中断后未处理项可继续", func(t *testing.T) {
		b := g1[10:15]
		reached, resume := make(chan struct{}), make(chan struct{})
		setHook(func(_ string, pos int) error {
			if pos == 2 {
				close(reached)
				<-resume
			}
			return nil
		})
		done := make(chan map[string]any)
		items := batchItems(t, bd, b)
		go func() {
			raw, _ := json.Marshal(map[string]any{"action": "relevant", "reasons": []string{"NEW_LEAD"}, "items": items})
			s, body := do(t, bd, t1, false, "POST", "/v1/detection/objectives/"+objective.ID+"/signal-batches", string(raw), "")
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			m["_status"] = s
			done <- m
		}()
		<-reached
		setParticipants(t, admin, expert)
		pump(t)
		close(resume)
		res := <-done
		setHook(nil)
		if res["_status"] != 202 || summaryOf(res) != "partial total=5 ok=2 same=0 definite=3 transient=0 unprocessed=0" {
			t.Fatalf("%v %+v", res["_status"], res)
		}
		for _, id := range b[2:] {
			if it := itemsOf(res)[id]; it["code"] != "signal_not_found" {
				t.Fatalf("%+v", it)
			}
		}
		setParticipants(t, bd, admin, expert)
		pump(t)
		// 中断：执行者在第 3 项前退出且不释放租约；租约过期后为 interrupted，继续时只处理未处理项
		c := g1[15:20]
		setHook(func(_ string, pos int) error {
			if pos == 3 {
				return errors.New("注入：进程中断")
			}
			return nil
		})
		res = batch(t, bd, map[string]any{"action": "dismiss", "items": batchItems(t, bd, c)}, "", 202)
		setHook(nil)
		id := res["id"].(string)
		if res["state"] != "running" {
			t.Fatalf("%+v", res)
		}
		expect(t, bd, false, "POST", "/v1/detection/signal-batches/"+id+"/retry", "", 409, "signal_batch_running")
		time.Sleep(1200 * time.Millisecond)
		got := obj(t, expect(t, bd, false, "GET", "/v1/detection/signal-batches/"+id, "", 200))
		if summaryOf(got) != "interrupted total=5 ok=3 same=0 definite=0 transient=0 unprocessed=2" {
			t.Fatalf("%s", summaryOf(got))
		}
		got = obj(t, expect(t, bd, false, "POST", "/v1/detection/signal-batches/"+id+"/retry", "", 202))
		if summaryOf(got) != "completed total=5 ok=5 same=0 definite=0 transient=0 unprocessed=0" || num(itemsOf(got)[c[0]], "attempts") != 1 {
			t.Fatalf("%s", summaryOf(got))
		}
	})

	t.Run("用例35：执行到 12/23 时取消，已尝试的项保留结果，其余未处理；继续时只处理未处理项", func(t *testing.T) {
		g2 = group(t, 23, "330109")
		reached, resume := make(chan struct{}), make(chan struct{})
		setHook(func(_ string, pos int) error {
			if pos == 12 {
				close(reached)
				<-resume
			}
			return nil
		})
		items := batchItems(t, bd, g2)
		done := make(chan []byte)
		go func() {
			raw, _ := json.Marshal(map[string]any{"action": "relevant", "reasons": []string{"NEW_LEAD"}, "items": items})
			_, body := do(t, bd, t1, false, "POST", "/v1/detection/objectives/"+objective.ID+"/signal-batches", string(raw), "")
			done <- body
		}()
		<-reached
		var id string
		if err := detOwner.QueryRow(ctx, `SELECT id FROM det_signal_batches WHERE lease_token IS NOT NULL`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		expect(t, dm, false, "POST", "/v1/detection/signal-batches/"+id+"/cancel", "", 404, "signal_batch_not_found")
		expect(t, bd, false, "POST", "/v1/detection/signal-batches/"+id+"/cancel", "", 200)
		close(resume)
		res := obj(t, <-done)
		setHook(nil)
		if summaryOf(res) != "cancelled total=23 ok=13 same=0 definite=0 transient=0 unprocessed=10" {
			t.Fatalf("%s", summaryOf(res))
		}
		got := obj(t, expect(t, bd, false, "POST", "/v1/detection/signal-batches/"+id+"/retry", "", 202))
		if summaryOf(got) != "completed total=23 ok=23 same=0 definite=0 transient=0 unprocessed=0" {
			t.Fatalf("%s", summaryOf(got))
		}
		for _, it := range itemsOf(got) {
			if num(it, "attempts") != 1 {
				t.Fatalf("%+v", it)
			}
		}
		if n := opsCount(t, `batch_id = $1`, id); n != 23 {
			t.Fatalf("%d", n)
		}
	})

	t.Run("用例21、30、31、32：撤销只补偿被改变的部分；补偿前核对版本，之后被改过、内容已更新或已关闭的项不撤销", func(t *testing.T) {
		// 用例30：原为“相关 · 已掌握”，批量改为“不相关 · 品类不符”并忽略后撤销，反馈恢复、处理状态回到操作前；原来没有反馈的撤销后为未反馈
		x, y := g2[0], g2[1]
		triage(t, bd, x, "feedback", fb("relevant", "ALREADY_KNOWN"), 200)
		triage(t, bd, y, "withdraw_feedback", nil, 200)
		before := feedbackOf(t, bd, x)
		res := batch(t, bd, map[string]any{"action": "irrelevant", "reasons": []string{"CATEGORY_MISMATCH"}, "items": batchItems(t, bd, []string{x, y})}, "", 202)
		if row(t, bd, x)["status"] != "dismissed" {
			t.Fatal("没有忽略")
		}
		undo(t, bd, map[string]any{"target_batch_id": res["id"]}, 200)
		after := feedbackOf(t, bd, x)
		if after["result"] != "relevant" || fmt.Sprint(after["reasons"]) != fmt.Sprint(before["reasons"]) ||
			num(after, "based_on_content_version") != num(before, "based_on_content_version") || row(t, bd, x)["status"] != "open" {
			t.Fatalf("%+v %+v", before, after)
		}
		if feedbackOf(t, bd, y) != nil || row(t, bd, y)["status"] != "open" {
			t.Fatalf("%+v", row(t, bd, y))
		}
		// 用例31：撤销批量恢复，重新回到已忽略；其中一条在撤销前已被同事调整处置，不撤销并显示当前值
		k1, k2 := g2[2], g2[3]
		batch(t, bd, map[string]any{"action": "dismiss", "items": batchItems(t, bd, []string{k1, k2})}, "", 202)
		restore := batch(t, bd, map[string]any{"action": "restore", "items": batchItems(t, bd, []string{k1, k2})}, "", 202)
		triage(t, dm, k2, "adjust", map[string]any{"manual_disposition": "watch_signal", "reason": "改为观察"}, 200)
		u := undo(t, bd, map[string]any{"target_batch_id": restore["id"]}, 200)
		results := map[string]map[string]any{}
		for _, x := range u["items"].([]any) {
			m := x.(map[string]any)
			results[m["signal_id"].(string)] = m
		}
		if results[k1]["result"] != "succeeded" || results[k2]["code"] != "conflict" || results[k2]["current"] == nil {
			t.Fatalf("%+v", u)
		}
		if row(t, bd, k1)["status"] != "dismissed" || row(t, bd, k2)["manual_disposition"] != "watch_signal" {
			t.Fatal("撤销结果不对")
		}
		// 用例32：撤销调整处置（原来没有时回到待处理）；撤销忽略回到原状态；本人之后又修改了反馈的不撤销；内容已更新、已关闭的不撤销
		z := g2[4]
		adj := triage(t, dm, z, "adjust", map[string]any{"manual_disposition": "pursue_candidate", "reason": "跟进"}, 200)
		undo(t, dm, map[string]any{"target_operation_id": adj["operation_id"]}, 200)
		if r := row(t, dm, z); r["status"] != "open" || r["manual_disposition"] != nil {
			t.Fatalf("%+v", r)
		}
		dis := triage(t, dm, z, "dismiss", nil, 200)
		u = undo(t, dm, map[string]any{"target_operation_id": dis["operation_id"]}, 200)
		if row(t, dm, z)["status"] != "open" {
			t.Fatal("撤销忽略没有回到待处理")
		}
		expect(t, dm, false, "POST", "/v1/detection/triage-undos", fmt.Sprintf(`{"operation_id":"%s","target_operation_id":"%s"}`, opID(), u["items"].([]any)[0].(map[string]any)["operation_id"]), 422, "not_applicable")
		expect(t, bd, false, "POST", "/v1/detection/triage-undos", fmt.Sprintf(`{"operation_id":"%s","target_operation_id":"%s"}`, opID(), dis["operation_id"]), 404, "triage_operation_not_found")
		expect(t, bd, false, "POST", "/v1/detection/triage-undos", fmt.Sprintf(`{"operation_id":"%s"}`, opID()), 400, "invalid_request")
		first := triage(t, bd, z, "feedback", fb("relevant", "ALREADY_KNOWN"), 200) // 批量 D 已写入“相关 · 新线索”
		triage(t, bd, z, "feedback", fb("irrelevant", "TOO_SMALL"), 200)
		u = undo(t, bd, map[string]any{"target_operation_id": first["operation_id"]}, 200)
		if item := u["items"].([]any)[0].(map[string]any); item["code"] != "conflict" || feedbackOf(t, bd, z)["result"] != "irrelevant" {
			t.Fatalf("%+v", u)
		}
		w := g2[5]
		op := triage(t, bd, w, "dismiss", nil, 200)
		if _, err := detOwner.Exec(ctx, `UPDATE det_signals SET content_version = content_version + 1 WHERE id = $1`, w); err != nil {
			t.Fatal(err)
		}
		u = undo(t, bd, map[string]any{"target_operation_id": op["operation_id"]}, 200)
		if item := u["items"].([]any)[0].(map[string]any); item["code"] != "signal_updated" {
			t.Fatalf("%+v", u)
		}
		v := g2[6]
		op = triage(t, bd, v, "feedback", fb("relevant", "ALREADY_KNOWN"), 200)
		if _, err := detOwner.Exec(ctx, `UPDATE det_signals SET status = 'closed', state_version = state_version + 1 WHERE id = $1`, v); err != nil {
			t.Fatal(err)
		}
		u = undo(t, bd, map[string]any{"target_operation_id": op["operation_id"]}, 200)
		if item := u["items"].([]any)[0].(map[string]any); item["code"] != "signal_closed" {
			t.Fatalf("%+v", u)
		}
	})

	t.Run("用例17：“全部符合筛选”得到固定清单；之后有新信号到达，批量只处理选中清单；超过 200 条时提示收窄筛选", func(t *testing.T) {
		sel := obj(t, expect(t, bd, false, "GET", "/v1/detection/objectives/"+objective.ID+"/signal-selection?tab=open&region=330109", "", 200))
		items := sel["items"].([]any)
		if sel["exceeds_limit"] != false || num(sel, "total") != int64(len(items)) || len(items) == 0 {
			t.Fatalf("%+v", sel)
		}
		late, env := fact(t, facttest.Bid{Location: "330109", Amount: 1_234_567})
		mustMatch(t, env)
		res := batch(t, bd, map[string]any{"action": "relevant", "reasons": []string{"NEW_LEAD"}, "items": items}, "", 202)
		if num(res["summary"].(map[string]any), "total") != int64(len(items)) {
			t.Fatalf("%s", summaryOf(res))
		}
		if _, ok := itemsOf(res)[sig(t, late)[0]]; ok {
			t.Fatal("新到达的信号进入了选中清单")
		}
		bulk := make([]facttest.Bid, 201)
		for i := range bulk {
			bulk[i] = facttest.Bid{Location: "330206", Amount: int64(2_000_000 + i)}
		}
		_, env = fact(t, bulk...)
		mustMatch(t, env)
		sel = obj(t, expect(t, bd, false, "GET", "/v1/detection/objectives/"+objective.ID+"/signal-selection?region=330200", "", 200))
		if sel["exceeds_limit"] != true || num(sel, "total") != 201 || len(sel["items"].([]any)) != 0 {
			t.Fatalf("%+v", sel)
		}
	})

	t.Run("用例28：忽略、恢复、调整处置、批量作业与撤销都有审计记录（操作者与信号版本，原因留在处理记录）", func(t *testing.T) {
		rows, err := detOwner.Query(ctx, `SELECT envelope->'data'->>'action', envelope->'data'->>'resource' FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1'`)
		if err != nil {
			t.Fatal(err)
		}
		actions := map[string]int{}
		for rows.Next() {
			var a, r string
			if err := rows.Scan(&a, &r); err != nil {
				t.Fatal(err)
			}
			actions[a]++
			if strings.HasPrefix(a, "detection.signal.") && !strings.Contains(r, "/state:") {
				t.Fatalf("审计资源缺少信号版本：%s %s", a, r)
			}
		}
		rows.Close()
		for _, a := range []string{"detection.signal.dismiss", "detection.signal.restore", "detection.signal.adjust", "detection.signal.undo", "detection.signal_batch.create"} {
			if actions[a] == 0 {
				t.Fatalf("缺少审计 %s：%v", a, actions)
			}
		}
	})
}

// n8sig、n9sig 与 sigN10 是子测试之间共享信号标识的小工具（避免在闭包外声明更多变量）。
func n8sig(t *testing.T, sig func(*testing.T, string) []string, notice string) string {
	t.Helper()
	s := sig(t, notice)
	if len(s) != 1 {
		t.Fatalf("%v", s)
	}
	return s[0]
}

func n9sig(t *testing.T, sig func(*testing.T, string) []string, notice string) string {
	return n8sig(t, sig, notice)
}

// sigN10 返回第三代（回滚后）产生的那条信号。
func sigN10(t *testing.T, owner *pgx.Conn, ctx context.Context) string {
	t.Helper()
	var id string
	if err := owner.QueryRow(ctx, `SELECT id FROM det_signals WHERE generation = 3 ORDER BY created_at LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func data(e outbox.Envelope) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(e.Data, &m)
	return m
}
