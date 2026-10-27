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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	ccapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection"
	detapi "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api/facttest"
	detremote "github.com/superailabs/miaoqia/services/subsystems/detection/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api/datasettest"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit"
)

// flakySessions 是可注入不可用的 identity 会话核验（customer-context 以被分派人本人核验时使用）。
type flakySessions struct {
	idapi.SessionResolver
	fail atomic.Bool
}

func (f *flakySessions) Resolve(ctx context.Context) (*idapi.Session, error) {
	if f.fail.Load() {
		return nil, errors.New("identity 不可用（测试注入）")
	}
	return f.SessionResolver.Resolve(ctx)
}

// flakyCandidates 是可注入不可用的 detection 候选负责人读取（pursuit 生产适配器的端口故障）。
type flakyCandidates struct {
	detapi.SignalCandidates
	fail atomic.Bool
}

func (f *flakyCandidates) SignalCandidate(ctx context.Context, id string) (*detapi.SignalCandidate, error) {
	if f.fail.Load() {
		return nil, binding.ErrTransient
	}
	return f.SignalCandidates.SignalCandidate(ctx, id)
}

// legacyPolicies 包装生效策略读取：strip 为真时去掉分派配置，模拟 0.7.0 之前的提供方（信号记为“分派规则不可用”）。
type legacyPolicies struct {
	inner ccapi.ActivePolicies
	strip atomic.Bool
}

func (l *legacyPolicies) ActivePolicies(ctx context.Context, limit int, cursor string) (*ccapi.ActivePolicyList, error) {
	page, err := l.inner.ActivePolicies(ctx, limit, cursor)
	if err != nil || !l.strip.Load() {
		return page, err
	}
	out := *page
	out.Policies = slices.Clone(page.Policies)
	for i := range out.Policies {
		out.Policies[i].Assignment = cc.OptActiveAssignment{}
	}
	return &out, nil
}

// C-14（PRD 第6段 F1 第2条、用例4）：目标分派规则的配置（customer-context）、信号生成时按规则计算候选负责人（detection）与 pursuit
// 创建面板经生产适配器读取候选（detection 的 mTLS 内部端口与远程绑定）。真实 PostgreSQL（各子系统自己的库、行级安全、应用账号）+
// 真实 identity 与 OpenFGA + 外部网关令牌校验 + 生成的 HTTP 服务端。事实为夹具（facttest）。
func TestAssignmentRules(t *testing.T) {
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
	admin, dm, dm2, bd, bd2, lead, expert, bd3 := user("admin"), user("dm"), user("dm2"), user("bd"), user("bd2"), user("lead"), user("expert"), user("bd3")
	admin2, bdx := user("admin2"), user("bdx") // 他租户 t2 的成员
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
		{`INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['decision_maker'],1), ($1,$4,'active',ARRAY['decision_maker'],1),
 ($1,$5,'active',ARRAY['business_developer'],1), ($1,$6,'active',ARRAY['business_developer'],1), ($1,$7,'active',ARRAY['pursuit_lead'],1),
 ($1,$8,'active',ARRAY['domain_expert'],1), ($1,$9,'active',ARRAY['business_developer'],1),
 ($10,$11,'active',ARRAY['admin','domain_expert','decision_maker','business_developer'],1), ($10,$12,'active',ARRAY['business_developer'],1)`,
			[]any{t1, admin, dm, dm2, bd, bd2, lead, expert, bd3, t2, admin2, bdx}},
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

	packs, _ := filepath.Abs(filepath.Join("..", "..", "..", "packs"))
	config := func(k string) string {
		switch k {
		case "PACKS_DIR":
			return packs
		case "SIGNING_KEY":
			return base64.StdEncoding.EncodeToString([]byte("pursuit-test-signing-key-0123456789abcdef"))
		}
		return ""
	}
	type started struct {
		env   *runtime.Env
		owner *pgx.Conn
	}
	start := func(m runtime.Module, role string) started {
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
		env := &runtime.Env{DB: app, Bindings: bindings, Log: quiet, Cell: "cell-01", Config: config, Role: role}
		if err := m.Start(ctx, env); err != nil {
			t.Fatal(err)
		}
		return started{env, owner}
	}

	// customer-context：会话核验经可注入不可用的包装（被分派人以其本人核验）
	localSessions, _ := runtime.Lookup[idapi.SessionResolver](bindings, idapi.SessionBinding)
	sessions := &flakySessions{SessionResolver: localSessions}
	bindings.Provide(idapi.SessionBinding, idapi.SessionResolver(sessions))
	ccModule := customercontext.Module()
	ccs := start(ccModule, "api")
	bindings.Provide(idapi.SessionBinding, localSessions)
	public, internal := http.NewServeMux(), http.NewServeMux()
	idModule.Routes(idEnv, public, internal)
	ccModule.Routes(ccs.env, public, internal)

	today := now().UTC().Truncate(24 * time.Hour)
	const org1 = "org_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	fixture := datasettest.New(today.AddDate(0, 0, -365))
	fixture.Add(datasettest.A02(today, org1)...)
	lrnModule := learning.ModuleWith(learning.Adapters{Dataset: fixture, Domains: fixture, Now: now})
	lrns := start(lrnModule, "api")
	lrnModule.Routes(lrns.env, public, internal)

	// detection：生效策略读取经可去掉分派配置的包装（模拟旧版本提供方）
	localActive, _ := runtime.Lookup[ccapi.ActivePolicies](bindings, ccapi.ActivePoliciesBinding)
	legacy := &legacyPolicies{inner: localActive}
	bindings.Provide(ccapi.ActivePoliciesBinding, ccapi.ActivePolicies(legacy))
	facts := facttest.New(t1, t2)
	detModule := detection.ModuleWith(detection.Adapters{Facts: facts, Candidates: facts, Now: now})
	dets := start(detModule, "api")
	detModule.Routes(dets.env, public, internal)
	bindings.Provide(ccapi.ActivePoliciesBinding, localActive)
	localCandidates, ok := runtime.Lookup[detapi.SignalCandidates](bindings, detapi.CandidatesBinding)
	if !ok {
		t.Fatal("detection 未提供 SignalCandidates")
	}

	// mTLS 内部端口：pursuit 经远程绑定读取 detection 的候选负责人（生产适配器，不注入 Candidates）
	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	intern := httptest.NewUnstartedServer(internal)
	intern.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	intern.StartTLS()
	defer intern.Close()
	remoteCandidates, err := detremote.NewCandidates(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	portN := &flakyCandidates{SignalCandidates: remoteCandidates}
	bindings.Provide(detapi.CandidatesBinding, detapi.SignalCandidates(portN))
	purModule := pursuit.Module()
	purs := start(purModule, "api")
	purModule.Routes(purs.env, public, internal)

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
	expectAs := func(t *testing.T, subject, tenant string, method, path, body, key string, status int, code ...string) []byte {
		t.Helper()
		got, raw := do(t, subject, tenant, false, method, path, body, key)
		if got != status || (len(code) > 0 && codeOf(raw) != code[0]) {
			t.Fatalf("%s %s %s：得到 %d，期望 %d %v：%s", subject, method, path, got, status, code, raw)
		}
		return raw
	}
	expect := func(t *testing.T, subject, method, path, body string, status int, code ...string) []byte {
		t.Helper()
		return expectAs(t, subject, t1, method, path, body, "", status, code...)
	}
	expectMFA := func(t *testing.T, subject, method, path, body string, status int) []byte {
		t.Helper()
		got, raw := do(t, subject, t1, true, method, path, body, "")
		if got != status {
			t.Fatalf("%s %s %s：得到 %d，期望 %d：%s", subject, method, path, got, status, raw)
		}
		return raw
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

	// 发件箱泵：事件按契约校验后交给订阅它的消费者；消费者返回错误时留待下一次重试
	type source struct {
		owner *pgx.Conn
		last  int64
	}
	sources := []*source{{owner: iamOwner}, {owner: ccs.owner}, {owner: lrns.owner}, {owner: dets.owner}, {owner: purs.owner}}
	type route struct {
		module   runtime.Module
		env      *runtime.Env
		consumer string
	}
	routes := map[string][]route{
		"identity.approval.requested.v1":        {{ccModule, ccs.env, "customer-context.policy-approvals"}},
		"identity.approval.decided.v1":          {{ccModule, ccs.env, "customer-context.policy-approvals"}},
		"identity.approval.closed.v1":           {{ccModule, ccs.env, "customer-context.policy-approvals"}},
		"customer_context.policy.drafted.v1":    {{lrnModule, lrns.env, "learning.policy-simulations"}},
		"learning.simulation.completed.v1":      {{ccModule, ccs.env, "customer-context.policy-simulations"}},
		"customer_context.objective.changed.v1": {{ccModule, ccs.env, "customer-context.objective-participants"}, {purModule, purs.env, "pursuit.objective-changes"}},
		"identity.member.changed.v1":            {{purModule, purs.env, "pursuit.member-changes"}},
		"pursuit.pursuit.signal_linked.v1":      {{detModule, dets.env, "detection.pursuit-links"}},
		"pursuit.pursuit.signal_unlinked.v1":    {{detModule, dets.env, "detection.pursuit-links"}},
		"pursuit.team.changed.v1":               {{purModule, purs.env, "pursuit.authorization-sync"}},
	}
	type pendingEvent struct {
		r route
		e outbox.Envelope
	}
	var pending []pendingEvent
	var pumpMu sync.Mutex
	handle := func(t *testing.T, r route, e outbox.Envelope) error {
		t.Helper()
		for _, c := range r.module.Consumers(r.env) {
			if c.Name != r.consumer {
				continue
			}
			ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Tenant: e.Tenant, Cell: e.Cell, Purpose: "event"}), c.Subsystem)
			return r.env.DB.Tx(ectx, func(tx pgx.Tx) error { return c.Handlers[e.Type](ectx, tx, &e) })
		}
		t.Fatalf("没有消费者 %s", r.consumer)
		return nil
	}
	pump := func(t *testing.T) {
		t.Helper()
		pumpMu.Lock()
		defer pumpMu.Unlock()
		retry := pending
		pending = nil
		for _, p := range retry {
			if err := handle(t, p.r, p.e); err != nil {
				pending = append(pending, p)
			}
		}
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
						t.Fatalf("%s 不符合契约：%v：%s", e.Type, err, e.Data)
					}
					for _, r := range routes[e.Type] {
						if err := handle(t, r, e); err != nil {
							pending = append(pending, pendingEvent{r, e})
						}
					}
				}
			}
			if n == 0 {
				return
			}
		}
	}
	drained := func(t *testing.T) {
		t.Helper()
		pump(t)
		if len(pending) != 0 {
			t.Fatalf("仍有 %d 个事件待重试：%s", len(pending), pending[0].e.Type)
		}
	}
	count := func(t *testing.T, owner *pgx.Conn, sql string, args ...any) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 公共事实与匹配
	fact := func(t *testing.T, b facttest.Bid) (string, outbox.Envelope) {
		t.Helper()
		notice := ids.New("ntc")
		b.Notice, b.Result = notice, ids.New("bdr")
		if b.Title == "" {
			b.Title = "夹具：某管网工程中标结果"
		}
		if b.Location == "" {
			b.Location = "330106"
		}
		if b.Categories == nil {
			b.Categories = []string{"CAT-PIPE-PE"}
		}
		if b.Winner == "" {
			b.Winner = ids.New("org")
		}
		b.Amount, b.HistoryCategories, b.WinnerName = 6_000_000, []string{"CAT-PIPE-PE"}, "夹具总包"
		facts.Put(notice, 1, facttest.BidResult(b))
		data, _ := json.Marshal(map[string]any{"info_domain": "INF-PROC", "fact_id": notice, "changed_fields": []string{"/results"}, "rights_tags": []string{"fixture"},
			"snapshot_ref": ids.New("snp"), "revision_ref": ids.New("exr"), "object_type": "tendering.notice", "fact_version": 1, "schema_version": "1.0.0"})
		return notice, outbox.Envelope{SpecVersion: "1.0", ID: ids.New("evt"), Source: "miaoqia/domain-intelligence", Type: "domain.fact.upserted.v1",
			Time: now().UTC().Add(-time.Minute).Truncate(time.Millisecond), DataContentType: "application/json", Zone: reqctx.ZonePublic, Data: data}
	}
	newSignal := func(t *testing.T, b facttest.Bid) string {
		t.Helper()
		notice, envelope := fact(t, b)
		for _, c := range detModule.Consumers(dets.env) {
			if c.Name != "detection.fact-matching" {
				continue
			}
			ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Purpose: "event"}), c.Subsystem)
			if err := dets.env.DB.Tx(ectx, func(tx pgx.Tx) error { return c.Handlers[envelope.Type](ectx, tx, &envelope) }); err != nil {
				t.Fatal(err)
			}
		}
		pump(t)
		var id string
		if err := dets.owner.QueryRow(ctx, `SELECT id FROM det_signals WHERE subject_id = $1 AND tenant_id = $2`, notice, t1).Scan(&id); err != nil {
			var outcomes []string
			rows, _ := dets.owner.Query(ctx, `SELECT tenant_id || ':' || outcome FROM det_matches WHERE unit_key LIKE '%' || $1 || '%'`, notice)
			if rows != nil {
				outcomes, _ = pgx.CollectRows(rows, pgx.RowTo[string])
			}
			t.Fatalf("没有信号：%v；匹配 %v", err, outcomes)
		}
		return id
	}
	stored := func(t *testing.T, signal string) (*string, *int64) {
		t.Helper()
		var owner *string
		var version *int64
		if err := dets.owner.QueryRow(ctx, `SELECT candidate_owner, assignment_version FROM det_signals WHERE id = $1`, signal).Scan(&owner, &version); err != nil {
			t.Fatal(err)
		}
		return owner, version
	}
	expectStored := func(t *testing.T, signal, owner string, version int64) {
		t.Helper()
		o, v := stored(t, signal)
		if v == nil || *v != version || (owner == "" && o != nil) || (owner != "" && (o == nil || *o != owner)) {
			t.Fatalf("信号 %s 的候选：%v %v，期望 %q v%d", signal, o, v, owner, version)
		}
	}

	// 档案与 A02 目标：决策者 dm 启用；bd、bd2（业务开拓者）、lead（项目推进者）、expert（业务专家）、admin 参与；bd3 不参与；
	// bdx 是他租户 t2 的成员，被写进参与人（identity 同步时拒绝，不获得参与关系）
	content := fmt.Sprintf(`{"subject_type":"organization","entities":[{"org_id":"%s","qualifications_complete":true}],"value_chain_roles":["ROLE-MFR"],
"industry_packs":["IND-BUILDING-MATERIALS"],"offering_categories":["CAT-PIPE-PE"],"brands":["示例品牌"],"service_regions":["330000"],"qualifications":[]}`, org1)
	expect(t, expert, "PUT", "/v1/customer-context/profile", `{"expected_version":0,"content":`+content+`}`, 200)
	var objective cc.Objective
	decode(t, expect(t, dm, "POST", "/v1/customer-context/objectives",
		fmt.Sprintf(`{"template_id":"APP-TENDER.A02","org_unit_id":"root","participants":["%s","%s","%s","%s","%s","%s"]}`, bd, bd2, lead, expert, admin, bdx), 200), &objective)
	objPath := "/v1/customer-context/objectives/" + objective.ID
	policyPath := objPath + "/policy"
	rulesPath := objPath + "/assignment"
	pump(t)
	{
		var p cc.Policy
		decode(t, expect(t, dm, "GET", policyPath, "", 200), &p)
		if !p.Draft.Set {
			a, _ := p.Active.Get()
			raw, _ := json.Marshal(&a.Form)
			var form map[string]any
			decode(t, raw, &form)
			body, _ := json.Marshal(map[string]any{"expected_version": p.DraftVersion, "form": form})
			expect(t, dm, "PUT", policyPath+"/draft", string(body), 200)
			pump(t)
			decode(t, expect(t, dm, "GET", policyPath, "", 200), &p)
		}
		d := p.Draft.Or(cc.PolicyDraft{})
		sim := d.Simulation.Or(cc.PolicySimulationRef{})
		body, _ := json.Marshal(map[string]any{"expected_version": p.DraftVersion, "revision": d.Revision, "simulation_id": sim.SimulationID, "note": "首次激活"})
		var r cc.PolicyRequest
		decode(t, expectMFA(t, dm, "POST", policyPath+"/activation-requests", string(body), 200), &r)
		pump(t)
		var a struct {
			Version int64 `json:"version"`
		}
		decode(t, expect(t, dm2, "GET", "/v1/identity/approvals/"+r.ApprovalID, "", 200), &a)
		expectMFA(t, dm2, "POST", "/v1/identity/approvals/"+r.ApprovalID+"/decide", fmt.Sprintf(`{"expected_version":%d,"decision":"approve"}`, a.Version), 200)
		drained(t)
	}
	objectiveVersion := func(t *testing.T) int64 {
		t.Helper()
		var o cc.Objective
		decode(t, expect(t, dm, "GET", objPath, "", 200), &o)
		return o.Version
	}
	setParticipants := func(t *testing.T, participants ...string) {
		t.Helper()
		var o cc.Objective
		decode(t, expect(t, dm, "GET", objPath, "", 200), &o)
		params, _ := json.Marshal(o.Params)
		list, _ := json.Marshal(participants)
		expect(t, dm, "PUT", objPath, fmt.Sprintf(`{"expected_version":%d,"owner":"%s","participants":%s,"params":%s}`, o.Version, dm, list, params), 200)
		drained(t)
	}
	putMember := func(t *testing.T, subject, state, roles string) {
		t.Helper()
		var m struct {
			MemberVersion int64 `json:"member_version"`
		}
		decode(t, expectMFA(t, admin, "GET", "/v1/identity/members/"+subject, "", 200), &m)
		expectMFA(t, admin, "PUT", "/v1/identity/members/"+subject, fmt.Sprintf(`{"state":%q,"roles":%s,"expected_version":%d}`, state, roles, m.MemberVersion), 200)
		drained(t)
	}
	rulesVersion := func(t *testing.T) int64 {
		t.Helper()
		var r cc.AssignmentRules
		decode(t, expect(t, dm, "GET", rulesPath, "", 200), &r)
		return r.Version
	}
	putRules := func(t *testing.T, who string, expected int64, rules, key string, status int, code ...string) map[string]any {
		t.Helper()
		return obj(t, expectAs(t, who, t1, "PUT", rulesPath, fmt.Sprintf(`{"expected_version":%d,"rules":%s}`, expected, rules), key, status, code...))
	}
	defaults := func(t *testing.T, who, signal string, status int, code ...string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, "GET", "/v1/pursuit/create-defaults?objective_id="+objective.ID+"&signal_id="+signal, "", status, code...))
	}
	expectDefault := func(t *testing.T, who, signal, owner, source, state string) {
		t.Helper()
		d := defaults(t, who, signal, 200)
		_, hasCandidate := d["candidate"]
		if d["owner"] != owner || d["owner_source"] != source || d["candidate_state"] != state || (state != "valid" && hasCandidate) ||
			(state == "valid" && d["candidate"] != owner) {
			t.Fatalf("%s 的缺省值：%+v，期望 %s %s %s", who, d, owner, source, state)
		}
	}
	userCtx := func(subject, tenant string) context.Context {
		return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: subject, Kind: "user"}})
	}
	candidateErr := func(t *testing.T, c detapi.SignalCandidates, subject, tenant, signal string) *detapi.Error {
		t.Helper()
		_, err := c.SignalCandidate(userCtx(subject, tenant), signal)
		var safe *detapi.Error
		if !errors.As(err, &safe) {
			t.Fatalf("%s 读取 %s 的候选：%v", subject, signal, err)
		}
		return safe
	}
	run := func(name string, f func(t *testing.T)) {
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	// 规则 1：杭州（330100）的 PE 燃气管 → lead；规则 2：宁波（330200）→ bd2；规则 3：全部 → bd（档案服务区域为浙江 330000）
	rules := fmt.Sprintf(`[{"assignee":%q,"regions":["330100"],"categories":["CAT-PIPE-PE-GAS"]},{"assignee":%q,"regions":["330200"]},{"assignee":%q}]`, lead, bd2, bd)
	var sigHZ, sigNB, sigWZ string

	run("C-14 用例4：决策者配置分派规则（版本 0→1）；读取按级别白名单；生效策略读取带分派配置；信号生成时第一条命中的规则写入候选；创建面板经远程绑定默认候选并以候选本人再检查", func(t *testing.T) {
		if r := obj(t, expect(t, dm, "GET", rulesPath, "", 200)); r["version"] != float64(0) || r["view"] != "full" || len(r["rules"].([]any)) != 0 {
			t.Fatalf("%+v", r)
		}
		key := "assign-" + suffix + "-0001"
		res := putRules(t, dm, 0, rules, key, 200)
		if res["version"] != float64(1) || res["rule_count"] != float64(3) || res["view"] != "full" {
			t.Fatalf("%+v", res)
		}
		// 同一键重放返回首次结果，不再写入
		if again := putRules(t, dm, 0, rules, key, 200); again["version"] != float64(1) {
			t.Fatalf("%+v", again)
		}
		// 同样的规则、当前版本：无变化，版本不变
		if same := putRules(t, dm, 1, rules, "", 200); same["version"] != float64(1) {
			t.Fatalf("%+v", same)
		}
		if n := count(t, ccs.owner, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1'
 AND envelope->'data'->>'action' = 'customer_context.objective.assignment_configured'`); n != 1 {
			t.Fatalf("审计 %d 条", n)
		}
		// 级别白名单：能分派跟进的业务开拓者看全文；只能查看目标的业务专家只见版本与条数
		if r := obj(t, expect(t, bd, "GET", rulesPath, "", 200)); r["view"] != "full" || len(r["rules"].([]any)) != 3 {
			t.Fatalf("%+v", r)
		}
		r := obj(t, expect(t, expert, "GET", rulesPath, "", 200))
		if _, leaked := r["rules"]; leaked || r["view"] != "summary" || r["rule_count"] != float64(3) || r["version"] != float64(1) {
			t.Fatalf("%+v", r)
		}
		if _, leaked := r["updated_at"]; leaked {
			t.Fatalf("%+v", r)
		}
		// 生效策略读取（服务主体）带分派配置
		page, err := localActive.ActivePolicies(reqctx.Service(reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01"}), "detection"), 0, "")
		if err != nil || len(page.Policies) != 1 {
			t.Fatalf("%v %+v", err, page)
		}
		if a, ok := page.Policies[0].Assignment.Get(); !ok || a.Version != 1 || len(a.Rules) != 3 || string(a.Rules[0].Assignee) != lead {
			t.Fatalf("%+v", page.Policies[0].Assignment)
		}
		sigHZ = newSignal(t, facttest.Bid{Location: "330106", Categories: []string{"CAT-PIPE-PE-GAS"}})
		sigNB = newSignal(t, facttest.Bid{Location: "330206"})
		sigWZ = newSignal(t, facttest.Bid{Location: "330302"})
		expectStored(t, sigHZ, lead, 1)
		expectStored(t, sigNB, bd2, 1)
		expectStored(t, sigWZ, bd, 1)
		// 杭州但不是燃气管：跳过规则 1，规则 2 区域不符，落到规则 3
		other := newSignal(t, facttest.Bid{Location: "330106", Categories: []string{"CAT-PIPE-PE"}})
		expectStored(t, other, bd, 1)
		expectDefault(t, bd, sigHZ, lead, "candidate", "valid")
		expectDefault(t, dm, sigNB, bd2, "candidate", "valid")
		expectDefault(t, bd, sigWZ, bd, "candidate", "valid")
		// 读取接口只经内部端口：外部网关上不存在
		expect(t, dm, "GET", "/v1/detection/signals/"+sigHZ+"/candidate-owner", "", 404)
		// 内部读取（远程绑定，以调用者本人）：能分派跟进的成员得到候选与配置版本
		c, err := remoteCandidates.SignalCandidate(userCtx(bd, t1), sigHZ)
		if err != nil || string(c.State) != "assigned" || c.CandidateOwner.Or("") != lead || c.AssignmentVersion.Or(-1) != 1 || c.ObjectiveID != objective.ID {
			t.Fatalf("%v %+v", err, c)
		}
	})

	run("C-14 用例4：规则清空后没有候选（none）；信号生成时分派规则不可用（旧版本提供方或 C-14 之前的信号）为 rules_unavailable，不当作没有候选", func(t *testing.T) {
		putRules(t, dm, 1, `[]`, "", 200)
		none := newSignal(t, facttest.Bid{Location: "330106"})
		expectStored(t, none, "", 2)
		expectDefault(t, bd, none, bd, "self", "none")
		legacy.strip.Store(true)
		old := newSignal(t, facttest.Bid{Location: "330106"})
		legacy.strip.Store(false)
		if o, v := stored(t, old); o != nil || v != nil {
			t.Fatalf("%v %v", o, v)
		}
		expectDefault(t, bd, old, bd, "self", "rules_unavailable")
		c, err := localCandidates.SignalCandidate(userCtx(dm, t1), old)
		if err != nil || string(c.State) != "rules_unavailable" || c.CandidateOwner.Set || c.AssignmentVersion.Set {
			t.Fatalf("%v %+v", err, c)
		}
		putRules(t, dm, 2, rules, "", 200)
	})

	run("C-14 越权与反例测试 另一租户：他租户成员读写分派规则、读取候选与创建面板一律 404，不暴露目标或信号是否存在", func(t *testing.T) {
		expectAs(t, admin2, t2, "GET", rulesPath, "", "", 404, "objective_not_found")
		expectAs(t, admin2, t2, "PUT", rulesPath, fmt.Sprintf(`{"expected_version":3,"rules":[{"assignee":%q}]}`, admin2), "", 404, "objective_not_found")
		expectAs(t, admin2, t2, "GET", "/v1/pursuit/create-defaults?objective_id="+objective.ID+"&signal_id="+sigHZ, "", "", 404, "objective_not_found")
		for _, c := range []detapi.SignalCandidates{localCandidates, remoteCandidates} {
			if e := candidateErr(t, c, admin2, t2, sigHZ); e.Status != 404 || e.Code != "signal_not_found" {
				t.Fatalf("%+v", e)
			}
		}
		if v := rulesVersion(t); v != 3 {
			t.Fatalf("%d", v)
		}
	})

	run("C-14 越权与反例测试 非管理员写配置：业务开拓者、业务专家与租户管理员（参与人）403，非参与人 404，不写入", func(t *testing.T) {
		body := fmt.Sprintf(`[{"assignee":%q}]`, bd)
		for _, who := range []string{bd, expert, admin} {
			putRules(t, who, 3, body, "", 403, "forbidden")
		}
		putRules(t, bd3, 3, body, "", 404, "objective_not_found")
		if v := rulesVersion(t); v != 3 {
			t.Fatalf("%d", v)
		}
		// 业务专家能查看信号但不能分派跟进：候选人标识不可见（403），不能查看的非参与人 404
		if e := candidateErr(t, remoteCandidates, expert, t1, sigHZ); e.Status != 403 || e.Code != "forbidden" {
			t.Fatalf("%+v", e)
		}
		if e := candidateErr(t, remoteCandidates, bd3, t1, sigHZ); e.Status != 404 || e.Code != "signal_not_found" {
			t.Fatalf("%+v", e)
		}
	})

	run("C-14 越权与反例测试 规则指向他租户成员或非参与人：422 assignee_no_access 不写入；即使经篡改写入规则，候选也以本人再检查为无访问且不返回标识", func(t *testing.T) {
		putRules(t, dm, 3, fmt.Sprintf(`[{"assignee":%q}]`, bdx), "", 422, "assignee_no_access")
		putRules(t, dm, 3, fmt.Sprintf(`[{"assignee":%q}]`, bd3), "", 422, "assignee_no_access")
		putRules(t, dm, 3, fmt.Sprintf(`[{"assignee":%q}]`, admin2), "", 422, "assignee_no_access")
		putRules(t, dm, 3, fmt.Sprintf(`[{"assignee":%q,"regions":["001100"]}]`, bd), "", 422, "invalid_region")
		putRules(t, dm, 3, fmt.Sprintf(`[{"assignee":%q,"categories":["CAT-NOT-INSTALLED"]}]`, bd), "", 422, "unknown_category")
		if v := rulesVersion(t); v != 3 {
			t.Fatalf("%d", v)
		}
		// 篡改：直接改写存储的规则指向他租户成员（bdx 在参与人名单中，但不是本租户成员）
		if _, err := ccs.owner.Exec(ctx, `UPDATE ctx_assignment_rules SET rules = $2, version = 4 WHERE objective_id = $1`, objective.ID,
			fmt.Sprintf(`[{"assignee":%q}]`, bdx)); err != nil {
			t.Fatal(err)
		}
		s := newSignal(t, facttest.Bid{Location: "330106"})
		expectStored(t, s, bdx, 4)
		expectDefault(t, bd, s, bd, "self", "no_access")
		// 篡改：信号上存储的候选改为非参与人
		if _, err := dets.owner.Exec(ctx, `UPDATE det_signals SET candidate_owner = $2 WHERE id = $1`, sigHZ, bd3); err != nil {
			t.Fatal(err)
		}
		expectDefault(t, bd, sigHZ, bd, "self", "no_access")
		if _, err := dets.owner.Exec(ctx, `UPDATE det_signals SET candidate_owner = $2 WHERE id = $1`, sigHZ, lead); err != nil {
			t.Fatal(err)
		}
		// 篡改：存储的规则首条指向非参与人 bd3：生成信号时跳过该规则（只从同一次读取的负责人与参与人中选），落到下一条
		if _, err := ccs.owner.Exec(ctx, `UPDATE ctx_assignment_rules SET rules = $2 WHERE objective_id = $1`, objective.ID,
			fmt.Sprintf(`[{"assignee":%q},{"assignee":%q}]`, bd3, bd2)); err != nil {
			t.Fatal(err)
		}
		expectStored(t, newSignal(t, facttest.Bid{Location: "330106"}), bd2, 4)
		putRules(t, dm, 4, rules, "", 200)
	})

	run("C-14 越权与反例测试 过期配置版本：409 version_conflict 不覆盖；同一键重放返回同一失败（确定失败固定在键上），键相同请求不同 409 idempotency_conflict", func(t *testing.T) {
		key := "assign-" + suffix + "-stale"
		body := fmt.Sprintf(`[{"assignee":%q}]`, bd2)
		putRules(t, dm, 1, body, key, 409, "version_conflict")
		putRules(t, dm, 1, body, key, 409, "version_conflict")
		putRules(t, dm, 5, body, key, 409, "idempotency_conflict")
		if v := rulesVersion(t); v != 5 {
			t.Fatalf("%d", v)
		}
		// 422 同样固定：被分派人之后成为参与人，同一键重放仍是首次的失败
		key = "assign-" + suffix + "-no-access"
		putRules(t, dm, 5, fmt.Sprintf(`[{"assignee":%q}]`, bd3), key, 422, "assignee_no_access")
		setParticipants(t, bd, bd2, lead, expert, admin, bdx, bd3)
		putRules(t, dm, 5, fmt.Sprintf(`[{"assignee":%q}]`, bd3), key, 422, "assignee_no_access")
		setParticipants(t, bd, bd2, lead, expert, admin, bdx)
		if n := count(t, ccs.owner, `SELECT count(*) FROM ctx_commands WHERE operation = 'objective.assignment' AND response ? 'failure'`); n < 2 {
			t.Fatalf("登记的确定失败 %d 条", n)
		}
	})

	run("C-14 越权与反例测试 候选已退出或不能查看目标：创建面板以候选本人再检查为 no_access 且不返回标识；写规则时 422；移出参与人时指向他的规则一并移除（分派配置版本加一）", func(t *testing.T) {
		putMember(t, lead, "removed", `[]`)
		expectDefault(t, bd, sigHZ, bd, "self", "no_access")
		putRules(t, dm, rulesVersion(t), fmt.Sprintf(`[{"assignee":%q}]`, lead), "", 422, "assignee_no_access")
		putMember(t, lead, "active", `["pursuit_lead"]`)
		setParticipants(t, bd, bd2, expert, admin, bdx)
		setParticipants(t, bd, bd2, lead, expert, admin, bdx)
		expectDefault(t, bd, sigHZ, lead, "candidate", "valid")
		// bd2 被移出目标：已存储的候选以本人再检查无访问；新信号跳过指向 bd2 的规则，落到下一条
		before := rulesVersion(t)
		setParticipants(t, bd, lead, expert, admin, bdx)
		expectDefault(t, bd, sigNB, bd, "self", "no_access")
		v := rulesVersion(t)
		var r cc.AssignmentRules
		decode(t, expect(t, dm, "GET", rulesPath, "", 200), &r)
		if v != before+1 || slices.ContainsFunc(r.Rules, func(x cc.AssignmentRule) bool { return string(x.Assignee) == bd2 }) {
			t.Fatalf("%d → %d：%+v", before, v, r.Rules)
		}
		s := newSignal(t, facttest.Bid{Location: "330206"})
		expectStored(t, s, bd, v)
		putRules(t, dm, v, fmt.Sprintf(`[{"assignee":%q}]`, bd2), "", 422, "assignee_no_access")
		setParticipants(t, bd, bd2, lead, expert, admin, bdx)
	})

	run("C-14 越权与反例测试 端口故障：detection 候选读取不可用时创建面板 503 不退回本人；identity 会话核验不可用时写规则 503 且不固定在键上，恢复后同一键成功", func(t *testing.T) {
		portN.fail.Store(true)
		defaults(t, bd, sigHZ, 503, "assignment_rules_unavailable")
		portN.fail.Store(false)
		expectDefault(t, bd, sigHZ, lead, "candidate", "valid")
		v := rulesVersion(t)
		key := "assign-" + suffix + "-flaky"
		sessions.fail.Store(true)
		putRules(t, dm, v, rules, key, 503, "identity_unavailable")
		sessions.fail.Store(false)
		if res := putRules(t, dm, v, fmt.Sprintf(`[{"assignee":%q}]`, bd2), key, 200); res["version"] != float64(v+1) {
			t.Fatalf("%d：%+v", v, res)
		}
		if got := rulesVersion(t); got != v+1 {
			t.Fatalf("%d", got)
		}
		putRules(t, dm, v+1, rules, "", 200)
	})

	run("C-14 越权与反例测试 并发：写规则与移除被分派人在同一目标行锁下串行（写规则在锁下再核对参与人，移除参与人在锁下移除指向他的规则），结果的规则不会指向已不是参与人的成员", func(t *testing.T) {
		for i := range 4 {
			v := rulesVersion(t)
			ov := objectiveVersion(t)
			var o cc.Objective
			decode(t, expect(t, dm, "GET", objPath, "", 200), &o)
			params, _ := json.Marshal(o.Params)
			var wg sync.WaitGroup
			var ruleStatus, cfgStatus int
			wg.Add(2)
			go func() {
				defer wg.Done()
				ruleStatus, _ = do(t, dm, t1, false, "PUT", rulesPath, fmt.Sprintf(`{"expected_version":%d,"rules":[{"assignee":%q}]}`, v, bd2), "")
			}()
			go func() {
				defer wg.Done()
				list, _ := json.Marshal([]string{bd, lead, expert, admin, bdx})
				cfgStatus, _ = do(t, dm, t1, false, "PUT", objPath, fmt.Sprintf(`{"expected_version":%d,"owner":"%s","participants":%s,"params":%s}`, ov, dm, list, params), "")
			}()
			wg.Wait()
			if cfgStatus != 200 {
				t.Fatalf("第 %d 轮：调整参与人 %d", i, cfgStatus)
			}
			var r cc.AssignmentRules
			decode(t, expect(t, dm, "GET", rulesPath, "", 200), &r)
			decode(t, expect(t, dm, "GET", objPath, "", 200), &o)
			for _, rule := range r.Rules {
				if !slices.Contains(o.Participants, rule.Assignee) && o.Owner != rule.Assignee {
					t.Fatalf("第 %d 轮：规则指向非参与人 %s（规则写入 %d）", i, rule.Assignee, ruleStatus)
				}
			}
			if (ruleStatus != 200 && ruleStatus != 422) || (ruleStatus == 200 && r.Version < v+1) || (ruleStatus != 200 && r.Version > v+1) {
				t.Fatalf("第 %d 轮：写规则 %d，版本 %d → %d", i, ruleStatus, v, r.Version)
			}
			drained(t)
			setParticipants(t, bd, bd2, lead, expert, admin, bdx)
		}
		putRules(t, dm, rulesVersion(t), rules, "", 200)
		drained(t)
	})
}
