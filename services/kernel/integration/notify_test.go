package integration_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	"github.com/superailabs/miaoqia/services/kernel/scheduler"
	"github.com/superailabs/miaoqia/services/subsystems/channels"
	chapi "github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api/wecomtest"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	ccapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	ccremote "github.com/superailabs/miaoqia/services/subsystems/customercontext/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/detection"
	detectionapi "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api/facttest"
	detremote "github.com/superailabs/miaoqia/services/subsystems/detection/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	idremote "github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/learning"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api/datasettest"
	pursuitapi "github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// C-08（PRD 第5段 F7，用例5、41）：信号的即时通知。真实 PostgreSQL（各子系统自己的库、行级安全、应用账号）+ 真实 identity 目录与 OpenFGA +
// 外部网关令牌校验。channels 经 mTLS 内部端口的远程绑定读取 customer-context 的生效策略（0.6.0 起带负责人与参与人）、detection 的信号卡片与
// identity 的审批；企业微信为内存实现（wecomtest），来源权利为标明为夹具的端口。detection 的信号事件经发件箱泵交给 channels 的消费者，
// 通知投递经 scheduler 角色的 channels.deliver-notices 任务执行；channels 与 detection 共用测试控制的业务时钟。
func TestInstantNotifications(t *testing.T) {
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
	if os.Getenv("NOTIFY_DEBUG") != "" {
		quiet = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	now := time.Now
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	clock := &testClock{}

	iam := testenv.NewDatabase(t)
	idModule := identity.ModuleWith(identity.Adapters{Now: now})
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", idModule.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2 := ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	user := func(role string) string { return role + "-" + suffix }
	admin, expert, dm, dm2, bd, bd2, lv, admin2 := user("admin"), user("expert"), user("dm"), user("dm2"), user("bd"), user("bd2"), user("lv"), user("admin2")
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
		{`INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['domain_expert'],1),
 ($1,$4,'active',ARRAY['decision_maker'],1), ($1,$5,'active',ARRAY['decision_maker'],1), ($1,$6,'active',ARRAY['business_developer'],1),
 ($1,$7,'active',ARRAY['business_developer'],1), ($1,$8,'active',ARRAY['business_developer'],1), ($9,$10,'active',ARRAY['admin','domain_expert','decision_maker'],1)`,
			[]any{t1, admin, expert, dm, dm2, bd, bd2, lv, t2, admin2}},
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
			return base64.StdEncoding.EncodeToString([]byte("notify-test-signing-key-0123456789abcdef"))
		}
		return ""
	}
	type started struct {
		env   *runtime.Env
		owner *pgx.Conn
	}
	start := func(m runtime.Module) started {
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
		return started{env, owner}
	}
	ccModule := customercontext.Module()
	ccs := start(ccModule)
	public, internal := http.NewServeMux(), http.NewServeMux()
	idModule.Routes(idEnv, public, internal)
	ccModule.Routes(ccs.env, public, internal)

	today := now().UTC().Truncate(24 * time.Hour)
	const org1 = "org_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	fixture := datasettest.New(today.AddDate(0, 0, -365))
	fixture.Add(datasettest.A02(today, org1)...)
	lrnModule := learning.ModuleWith(learning.Adapters{Dataset: fixture, Domains: fixture, Now: now})
	lrns := start(lrnModule)
	lrnModule.Routes(lrns.env, public, internal)
	facts := facttest.New(t1, t2)
	// detection 与 channels 共用测试控制的业务时钟：信号的发现时间（事件的 created_at）即即时通知的到达时间
	detModule := detection.ModuleWith(detection.Adapters{Facts: facts, Candidates: facts, Now: clock.Now})
	dets := start(detModule)
	detModule.Routes(dets.env, public, internal)

	// mTLS 内部端口：channels 经远程绑定读取 customer-context 的生效策略、detection 的信号卡片与 identity 的审批
	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	intern := httptest.NewUnstartedServer(internal)
	intern.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	intern.StartTLS()
	defer intern.Close()
	remoteBriefing, err := detremote.NewBriefing(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	bindings.Provide(detectionapi.BriefingBinding, remoteBriefing)
	remotePolicies, err := ccremote.NewActivePolicies(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	bindings.Provide(ccapi.ActivePoliciesBinding, remotePolicies)
	idClient, err := idremote.NewAuthorizer(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	approvals, ok := idClient.(idapi.ApprovalReader)
	if !ok {
		t.Fatal("identity 远程客户端未实现 ApprovalReader")
	}
	bindings.Provide(idapi.ApprovalReaderBinding, approvals)

	wc := wecomtest.New()
	rights := &fakeRights{override: map[string]map[string]chapi.FieldRight{}}
	bindings.Provide(pursuitapi.ActionsBinding, pursuitapi.Actions(unwiredPursuits{}))
	bindings.Provide(pursuitapi.TrackingsBinding, pursuitapi.Trackings(unwiredPursuits{}))
	chnModule := channels.ModuleWith(channels.Adapters{WeCom: wc, Coverage: &fakeCoverage{interrupted: map[string]time.Time{}}, Rights: rights, Now: clock.Now})
	chns := start(chnModule)
	chnModule.Routes(chns.env, public, internal)
	chnOwner := chns.owner
	briefings, _ := runtime.Lookup[chapi.Briefings](bindings, chapi.BriefingsBinding)
	callbacks, _ := runtime.Lookup[chapi.Callbacks](bindings, chapi.CallbacksBinding)
	if briefings == nil || callbacks == nil {
		t.Fatal("channels 未提供本地绑定")
	}

	issuer := testissuer.New(t)
	resolver, _ := runtime.Lookup[idapi.SessionResolver](bindings, idapi.SessionBinding)
	secured, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", public, resolver)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(secured)
	defer server.Close()

	var keys atomic.Int64
	do := func(t *testing.T, subject, tenant string, mfa bool, method, path, body string) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+issuer.Token(t, subject, tenant, "cell-01", mfa))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(subject, method, path, body, keys.Add(1)))))[:40])
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
	expectAs := func(t *testing.T, subject, tenant string, mfa bool, method, path, body string, status int, code ...string) []byte {
		t.Helper()
		got, raw := do(t, subject, tenant, mfa, method, path, body)
		if got != status || (len(code) > 0 && codeOf(raw) != code[0]) {
			t.Fatalf("%s %s %s：得到 %d，期望 %d %v：%s", subject, method, path, got, status, code, raw)
		}
		return raw
	}
	expect := func(t *testing.T, subject string, mfa bool, method, path, body string, status int, code ...string) []byte {
		t.Helper()
		return expectAs(t, subject, t1, mfa, method, path, body, status, code...)
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
	count := func(t *testing.T, owner *pgx.Conn, sql string, args ...any) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 发件箱泵：全部事件按契约校验；detection 的信号事件交给 learning 与 channels（即时通知），identity 的成员事件交给 channels
	type source struct {
		name  string
		owner *pgx.Conn
		last  int64
	}
	sources := []*source{{name: "identity", owner: iamOwner}, {name: "customer-context", owner: ccs.owner}, {name: "learning", owner: lrns.owner},
		{name: "detection", owner: dets.owner}, {name: "channels", owner: chnOwner}}
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
		"customer_context.objective.changed.v1": {{ccModule, ccs.env, "customer-context.objective-participants"}},
		"detection.signal.detected.v1":          {{lrnModule, lrns.env, "learning.signal-feedback"}, {chnModule, chns.env, "channels.signal-notices"}},
		"detection.signal.triaged.v1":           {{lrnModule, lrns.env, "learning.signal-feedback"}},
		"identity.member.changed.v1":            {{chnModule, chns.env, "channels.member-changes"}},
	}
	var delivered []outbox.Envelope
	// consume 把一条事件按契约校验后交给订阅它的消费者（发件箱泵与伪造、重放的事件共用）
	consume := func(t *testing.T, e outbox.Envelope, only string) {
		t.Helper()
		schema, err := events.Schema(e.Type)
		if err != nil {
			t.Fatal(err)
		}
		if err := events.Validate(schema, e.Data); err != nil {
			t.Fatalf("%s 不符合契约：%v：%s", e.Type, err, e.Data)
		}
		for _, r := range routes[e.Type] {
			for _, c := range r.module.Consumers(r.env) {
				if c.Name != r.consumer || (only != "" && c.Name != only) {
					continue
				}
				if !slices.Contains(c.Topics, outbox.Topic(e.Zone, "cell-01", strings.SplitN(e.Type, ".", 2)[0])) {
					t.Fatalf("%s 不订阅 %s 的主题", c.Name, e.Type)
				}
				ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Tenant: e.Tenant, Cell: e.Cell, Purpose: "event"}), c.Subsystem)
				if err := r.env.DB.Tx(ectx, func(tx pgx.Tx) error { return c.Handlers[e.Type](ectx, tx, &e) }); err != nil {
					t.Fatalf("%s 处理 %s：%v", c.Name, e.Type, err)
				}
			}
		}
	}
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
					delivered = append(delivered, e)
					consume(t, e, "")
				}
			}
			if n == 0 {
				return
			}
		}
	}
	eventsOf := func(typ string) []map[string]any {
		var out []map[string]any
		for _, e := range delivered {
			if e.Type == typ {
				var d map[string]any
				_ = json.Unmarshal(e.Data, &d)
				out = append(out, d)
			}
		}
		return out
	}
	envelopeOf := func(t *testing.T, typ, signal string) outbox.Envelope {
		t.Helper()
		for _, e := range delivered {
			var d struct {
				SignalID string `json:"signal_id"`
			}
			if e.Type == typ && json.Unmarshal(e.Data, &d) == nil && d.SignalID == signal {
				return e
			}
		}
		t.Fatalf("没有 %s 的 %s", signal, typ)
		return outbox.Envelope{}
	}
	audits := func(t *testing.T, action, resource string) int {
		t.Helper()
		return count(t, chnOwner, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1' AND envelope->'data'->>'action' = $1
 AND envelope->'data'->>'resource' = $2`, action, resource)
	}

	// 公共事实与匹配（同 TestBriefingQuickActions）
	fact := func(t *testing.T, bid facttest.Bid) (string, outbox.Envelope) {
		t.Helper()
		notice := ids.New("ntc")
		bid.Notice = notice
		if bid.Result == "" {
			bid.Result = ids.New("bdr")
		}
		if bid.Title == "" {
			bid.Title = "夹具：某管网工程中标结果"
		}
		if bid.Location == "" {
			bid.Location = "330106"
		}
		if bid.Categories == nil {
			bid.Categories = []string{"CAT-PIPE-PE"}
		}
		if bid.Winner == "" {
			bid.Winner = ids.New("org")
		}
		facts.Put(notice, 1, facttest.BidResult(bid))
		data, _ := json.Marshal(map[string]any{"info_domain": "INF-PROC", "fact_id": notice, "changed_fields": []string{"/results"}, "rights_tags": []string{"fixture"},
			"snapshot_ref": ids.New("snp"), "revision_ref": ids.New("exr"), "object_type": "tendering.notice", "fact_version": 1, "schema_version": "1.0.0"})
		return notice, outbox.Envelope{SpecVersion: "1.0", ID: ids.New("evt"), Source: "miaoqia/domain-intelligence", Type: "domain.fact.upserted.v1",
			Time: now().UTC().Add(-time.Minute).Truncate(time.Millisecond), DataContentType: "application/json", Zone: reqctx.ZonePublic, Data: data}
	}
	// newSignal 在业务时钟 at 时由 detection 生成一条信号（事件经发件箱泵到达 channels）
	newSignal := func(t *testing.T, at time.Time, bid facttest.Bid) string {
		t.Helper()
		clock.set(at, 0)
		n, e := fact(t, bid)
		for _, c := range detModule.Consumers(dets.env) {
			if c.Name != "detection.fact-matching" {
				continue
			}
			ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Purpose: "event"}), c.Subsystem)
			if err := dets.env.DB.Tx(ectx, func(tx pgx.Tx) error { return c.Handlers[e.Type](ectx, tx, &e) }); err != nil {
				t.Fatal(err)
			}
		}
		pump(t)
		var id string
		if err := dets.owner.QueryRow(ctx, `SELECT id FROM det_signals WHERE subject_id = $1`, n).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pursueBid := func(name string) facttest.Bid {
		return facttest.Bid{Amount: 6_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}, WinnerName: name}
	}
	reviewBid := facttest.Bid{Amount: 1_000_000, CategoryConfidence: 0.5, WinnerConfidence: 0.5}

	// 档案与 A02 目标：决策者 dm 启用并为负责人；业务开拓者 bd、lv，管理员 admin 与业务专家 expert 参与；bd2 不参与
	content := fmt.Sprintf(`{"subject_type":"organization","entities":[{"org_id":"%s","qualifications_complete":true}],"value_chain_roles":["ROLE-MFR"],
"industry_packs":["IND-BUILDING-MATERIALS"],"offering_categories":["CAT-PIPE-PE"],"brands":["示例品牌"],"service_regions":["330000"],"qualifications":[]}`, org1)
	expect(t, expert, false, "PUT", "/v1/customer-context/profile", `{"expected_version":0,"content":`+content+`}`, 200)
	var objective cc.Objective
	decode(t, expect(t, dm, false, "POST", "/v1/customer-context/objectives",
		fmt.Sprintf(`{"template_id":"APP-TENDER.A02","org_unit_id":"root","participants":["%s","%s","%s","%s"]}`, bd, lv, admin, expert), 200), &objective)
	objPath := "/v1/customer-context/objectives/" + objective.ID
	policyPath := objPath + "/policy"
	pump(t)
	getPolicy := func(t *testing.T) cc.Policy {
		t.Helper()
		var p cc.Policy
		decode(t, expect(t, dm, false, "GET", policyPath, "", 200), &p)
		return p
	}
	// activate 以生效表单起草（notify 为 pursue_candidate 的通知设置，空表示沿用），经仿真与审批激活（目标负责人的策略偏好）
	activate := func(t *testing.T, notify map[string]any, note string) {
		t.Helper()
		p := getPolicy(t)
		if notify != nil || !p.Draft.Set {
			var form map[string]any
			if a, ok := p.Active.Get(); ok {
				raw, _ := json.Marshal(&a.Form)
				decode(t, raw, &form)
			} else {
				d := p.Draft.Or(cc.PolicyDraft{})
				raw, _ := json.Marshal(&d.Form)
				decode(t, raw, &form)
			}
			if notify != nil {
				n, _ := form["notify"].(map[string]any)
				if n == nil {
					n = map[string]any{}
				}
				n["pursue_candidate"] = notify
				form["notify"] = n
			}
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
		pump(t)
		var a struct {
			Version int64 `json:"version"`
		}
		decode(t, expect(t, dm2, false, "GET", "/v1/identity/approvals/"+r.ApprovalID, "", 200), &a)
		expect(t, dm2, true, "POST", "/v1/identity/approvals/"+r.ApprovalID+"/decide", fmt.Sprintf(`{"expected_version":%d,"decision":"approve"}`, a.Version), 200)
		pump(t)
	}
	setParticipants := func(t *testing.T, participants ...string) {
		t.Helper()
		var o cc.Objective
		decode(t, expect(t, dm, false, "GET", objPath, "", 200), &o)
		params, _ := json.Marshal(o.Params)
		list, _ := json.Marshal(participants)
		expect(t, dm, false, "PUT", objPath, fmt.Sprintf(`{"expected_version":%d,"owner":"%s","participants":%s,"params":%s}`, o.Version, dm, list, params), 200)
		pump(t)
	}
	activate(t, nil, "首次激活（模板缺省：跟进候选即时发给目标负责人）")

	// 渠道：t1 的企业 corp1 与 t2 的企业 corp2（安装已启用），绑定直接写入（绑定流程见 TestChannelBindingAndSSO）
	corp1, corp2 := "corp1-"+suffix, "corp2-"+suffix
	install := func(tenant, corp string) string {
		inst := ids.New("chi")
		wc.Corp(corp, "企业"+corp)
		if _, err := chnOwner.Exec(ctx, `INSERT INTO chn_installations (id, tenant_id, channel, state, version, corp_id, corp_name, verified_at) VALUES ($1, $2, 'wecom', 'enabled', 3, $3, $4, now())`,
			inst, tenant, corp, "企业"+corp); err != nil {
			t.Fatal(err)
		}
		if _, err := chnOwner.Exec(ctx, `INSERT INTO chn_corp_routes (channel, corp_id, tenant_id, cell, installation_id) VALUES ('wecom', $1, $2, 'cell-01', $3)`, corp, tenant, inst); err != nil {
			t.Fatal(err)
		}
		return inst
	}
	inst1, inst2 := install(t1, corp1), install(t2, corp2)
	bind := func(tenant, inst, corp, subject, account string) {
		if _, err := chnOwner.Exec(ctx, `INSERT INTO chn_bindings (id, tenant_id, installation_id, channel, corp_id, channel_user_id, display_name, subject, member_version, state, version, bound_at)
 VALUES ($1, $2, $3, 'wecom', $4, $5, $6, $7, 1, 'bound', 1, now())`, ids.New("chb"), tenant, inst, corp, account, "成员"+account, subject); err != nil {
			t.Fatal(err)
		}
	}
	account := map[string]string{bd: "u-bd", dm: "u-dm", expert: "u-ex", lv: "u-lv", bd2: "u-bd2", admin2: "u-a2"}
	for _, s := range []string{bd, dm, expert, lv, bd2} {
		bind(t1, inst1, corp1, s, account[s])
	}
	bind(t2, inst2, corp2, admin2, account[admin2])

	// 通知投递经 scheduler 角色的 channels.deliver-notices 任务执行（R-01）：逐个 active 租户以 svc:channels 调用 DeliverNotices
	chnJobs, err := chnModule.Jobs(chns.env)
	if err != nil {
		t.Fatal(err)
	}
	var noticeJob scheduler.Job
	for _, j := range chnJobs {
		if j.Name == "channels.deliver-notices" {
			noticeJob = j
		}
	}
	if noticeJob.Run == nil {
		t.Fatalf("channels 应声明 channels.deliver-notices：%+v", chnJobs)
	}
	// deliverN 在业务时钟 at 时执行 rounds 轮投递任务（第一轮展开受众，之后发送）
	deliverN := func(t *testing.T, at time.Time, rounds int) {
		t.Helper()
		clock.set(at, 0)
		for range rounds {
			var roundErr error
			r := &scheduler.Runner{OnRound: func(_ string, _ scheduler.Outcome, _ int, err error) { roundErr = err }}
			if o := r.Once(ctx, scheduler.Bound{Job: noticeJob, DB: chns.env.DB}); o != scheduler.Ran {
				t.Fatalf("%s %v", o, roundErr)
			}
		}
		pump(t)
	}
	svc := func(tenant string) context.Context {
		return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:notify-test", Kind: "service"}})
	}
	gw := reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: "svc:gateway", Kind: "service"}})
	compose := func(t *testing.T, subject string, when time.Time) *chapi.ComposeResult {
		t.Helper()
		clock.set(when, 0)
		res, err := briefings.Compose(svc(t1), subject, when)
		if err != nil {
			t.Fatal(err)
		}
		pump(t)
		return res
	}
	noticeOf := func(t *testing.T, signal, recipient string) (state, reason string) {
		t.Helper()
		var r *string
		if err := chnOwner.QueryRow(ctx, `SELECT state, reason FROM chn_notices WHERE kind = 'signal_immediate' AND signal_id = $1 AND recipient = $2`, signal, recipient).
			Scan(&state, &r); err != nil {
			t.Fatalf("%s → %s：%v", signal, recipient, err)
		}
		if r != nil {
			reason = *r
		}
		return
	}
	noticeCount := func(t *testing.T, signal string) int {
		t.Helper()
		return count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE kind = 'signal_immediate' AND signal_id = $1`, signal)
	}
	tokensFor := func(t *testing.T, signal, recipient string) int {
		t.Helper()
		return count(t, chnOwner, `SELECT count(*) FROM chn_action_tokens WHERE signal_id = $1 AND recipient = $2`, signal, recipient)
	}
	linkTarget := func(t *testing.T, u string) (token, kind, target string) {
		t.Helper()
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		token = parsed.Query().Get("link")
		sum := sha256.Sum256([]byte(token))
		if err := chnOwner.QueryRow(ctx, `SELECT kind, target FROM chn_briefing_links WHERE token_digest = $1`, hex.EncodeToString(sum[:])).Scan(&kind, &target); err != nil {
			t.Fatalf("%s：%v", u, err)
		}
		return
	}
	button := func(c chapi.Card, key string) chapi.Button {
		for _, b := range c.Buttons {
			if b.Key == key {
				return b
			}
		}
		return chapi.Button{}
	}
	// cardsOf 返回发给账号的消息中，自第 from 条起的信号卡片：信号 → 卡片
	cardsOf := func(t *testing.T, subject string, from int) map[string]chapi.Message {
		t.Helper()
		out := map[string]chapi.Message{}
		for _, m := range wc.Messages(corp1, account[subject])[from:] {
			if m.Kind != "signal_card" || m.Card == nil {
				continue
			}
			_, kind, target := linkTarget(t, button(*m.Card, "view").URL)
			if kind != "signal" {
				t.Fatalf("%+v", m.Card)
			}
			out[target] = m
		}
		return out
	}
	sent := func(subject string) int { return len(wc.Messages(corp1, account[subject])) }
	var evSeq atomic.Int64
	click := func(t *testing.T, subject, cardID, token string) {
		t.Helper()
		ev := fmt.Sprintf("act-%s-%d", suffix, evSeq.Add(1))
		res, err := callbacks.ReceiveInbound(gw, "wecom", wc.Sign(&chapi.Callback{CorpID: corp1, EventID: ev, Timestamp: clock.Now(), Nonce: "n",
			Body: wecomtest.ActionBody(account[subject], cardID, token)}))
		if err != nil || res.Outcome != chapi.InboundQueued {
			t.Fatalf("%+v %v", res, err)
		}
		if _, err := briefings.ProcessActions(svc(t1), 50); err != nil {
			t.Fatal(err)
		}
		pump(t)
	}
	// 投递事件只带白名单字段（通知、种类、收件人、渠道、信号、目标、尝试数与结果），不带任何卡片内容
	allowed := []string{"notice_id", "kind", "recipient", "channel", "signal_id", "objective_id", "attempt", "accepted_at", "state", "reason", "final"}
	checkWhitelist := func(t *testing.T) {
		t.Helper()
		for _, typ := range []string{"channels.signal_notice.sent.v1", "channels.signal_notice.failed.v1"} {
			for _, d := range eventsOf(typ) {
				for k := range d {
					if !slices.Contains(allowed, k) {
						t.Fatalf("%s 含白名单外的字段 %s：%+v", typ, k, d)
					}
				}
			}
		}
	}
	failedFor := func(signal, recipient string) []map[string]any {
		var out []map[string]any
		for _, d := range eventsOf("channels.signal_notice.failed.v1") {
			if d["signal_id"] == signal && d["recipient"] == recipient {
				out = append(out, d)
			}
		}
		return out
	}
	sentFor := func(signal, recipient string) []map[string]any {
		var out []map[string]any
		for _, d := range eventsOf("channels.signal_notice.sent.v1") {
			if d["signal_id"] == signal && d["recipient"] == recipient {
				out = append(out, d)
			}
		}
		return out
	}

	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, shanghai) } // 11-10 为周二
	run := func(name string, f func(t *testing.T)) {
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	var p1, p2, r1 string
	run("C-08 用例41：跟进候选即时发给目标负责人（只发负责人），卡片与简报同一模板与按钮，点击“相关”经 detection 以本人执行；复核候选按 digest 只进入简报", func(t *testing.T) {
		from := map[string]int{dm: sent(dm), bd: sent(bd), lv: sent(lv), expert: sent(expert), bd2: sent(bd2)}
		p1 = newSignal(t, at(10, 10, 0), pursueBid("夹具总包甲"))
		p2 = newSignal(t, at(10, 10, 1), pursueBid("夹具总包乙"))
		r1 = newSignal(t, at(10, 10, 2), reviewBid)
		// 记下的是待展开的一行（受众尚未确定），不是任何人的通知
		if c := count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE signal_id = ANY($1) AND recipient = '*' AND state = 'pending'`, []string{p1, p2, r1}); c != 3 {
			t.Fatalf("待展开 %d", c)
		}
		deliverN(t, at(10, 10, 3), 2)
		for _, s := range []string{p1, p2} {
			if st, _ := noticeOf(t, s, dm); st != "delivered" || noticeCount(t, s) != 2 {
				t.Fatalf("%s：%s，%d 行", s, st, noticeCount(t, s))
			}
		}
		// 复核候选：route 为 digest，不展开
		if noticeCount(t, r1) != 1 || count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE signal_id = $1 AND state = 'expanded' AND expanded_recipients = 0`, r1) != 1 {
			t.Fatal("复核候选不应即时通知")
		}
		got := cardsOf(t, dm, from[dm])
		c1, ok := got[p1]
		if !ok || len(got) != 2 {
			t.Fatalf("负责人应收到两张即时通知卡片：%+v", got)
		}
		if !strings.HasPrefix(c1.Text, "即时通知：") || button(*c1.Card, "relevant").Token == "" || button(*c1.Card, "irrelevant:OUT_OF_REGION").Token == "" ||
			button(*c1.Card, "view").URL == "" || !slices.ContainsFunc(c1.Card.Lines, func(l string) bool { return strings.Contains(l, "夹具总包甲") }) {
			t.Fatalf("%+v", c1)
		}
		for _, s := range []string{bd, lv, expert, bd2} {
			if len(cardsOf(t, s, from[s])) != 0 {
				t.Fatalf("%s 不是负责人，不应收到即时通知", s)
			}
		}
		if ev := sentFor(p1, dm); len(ev) != 1 || ev[0]["attempt"] != float64(1) || ev[0]["objective_id"] != objective.ID {
			t.Fatalf("%+v", ev)
		}
		checkWhitelist(t)
		// 点击“相关”：同一令牌机制，以点击者本人经 detection 执行，渠道为企业微信
		click(t, dm, c1.Card.CardID, button(*c1.Card, "relevant").Token)
		tri := eventsOf("detection.signal.triaged.v1")
		if len(tri) == 0 || tri[len(tri)-1]["signal_id"] != p1 || tri[len(tri)-1]["channel"] != "wecom" || tri[len(tri)-1]["actor"] != dm {
			t.Fatalf("%+v", tri)
		}
		// 再次投递不重复发送
		n := sent(dm)
		deliverN(t, at(10, 10, 30), 2)
		if sent(dm) != n {
			t.Fatal("已送达的即时通知不得重复发送")
		}
	})

	run("C-08 用例41（简报）：下一期简报中仍待处理的已通知信号照常列出并标“已通知”，不重复推送信号卡片，只在总览中列出", func(t *testing.T) {
		from := sent(dm)
		res := compose(t, dm, at(11, 7, 31))
		if res.Outcome != "composed" || res.DeliveryID == "" {
			t.Fatalf("%+v", res)
		}
		b := obj(t, expect(t, dm, false, "GET", "/v1/channels/briefings/"+res.BriefingID, "", 200))
		notified := map[string]bool{}
		for _, s := range b["sections"].([]any) {
			for _, it := range s.(map[string]any)["items"].([]any) {
				m := it.(map[string]any)
				if id, ok := m["signal_id"].(string); ok {
					notified[id] = m["notified"] == true
				}
			}
		}
		if !notified[p1] || !notified[p2] {
			t.Fatalf("已通知的信号应列出并标已通知：%+v", notified)
		}
		if notified[r1] {
			t.Fatal("没有即时通知的信号不标已通知")
		}
		cards := cardsOf(t, dm, from)
		if _, dup := cards[p2]; dup {
			t.Fatal("已通知的信号不得再推送信号卡片")
		}
		if _, ok := cards[r1]; !ok {
			t.Fatalf("未通知的信号照常推送卡片：%+v", cards)
		}
		overview := wc.Messages(corp1, account[dm])[from]
		if overview.Kind != "briefing_overview" || !slices.ContainsFunc(overview.Card.Lines, func(l string) bool { return strings.HasSuffix(l, "（已通知）") }) {
			t.Fatalf("%+v", overview)
		}
	})

	run("C-08 越权与反例测试 安静时段（用例5）：安静时段内到达的跟进候选不发送、不渲染、不签发令牌，延后到结束；结束时简报即将发送的并入简报（标“夜间到达”）、不单独推送；周末结束后没有即将发送的简报，照常推送", func(t *testing.T) {
		from := sent(dm)
		night := newSignal(t, at(11, 23, 0), pursueBid("夹具总包丙"))
		deliverN(t, at(11, 23, 1), 2)
		deliverN(t, at(12, 3, 0), 2)
		if st, _ := noticeOf(t, night, dm); st != "pending" || sent(dm) != from || tokensFor(t, night, dm) != 0 {
			t.Fatalf("安静时段内不发送、不签发令牌：%s %d", st, tokensFor(t, night, dm))
		}
		var next time.Time
		if err := chnOwner.QueryRow(ctx, `SELECT next_attempt_at FROM chn_notices WHERE signal_id = $1 AND recipient = $2`, night, dm).Scan(&next); err != nil ||
			!next.Equal(at(12, 7, 0)) {
			t.Fatalf("延后到安静时段结束：%v %v", next, err)
		}
		if count(t, chnOwner, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1' AND envelope->'data'->>'action' = 'channels.notice.deferred'`) == 0 {
			t.Fatal("延后应写审计")
		}
		// 07:00 安静时段结束，07:30 简报即将发送：并入简报，不单独推送
		deliverN(t, at(12, 7, 0), 2)
		if st, reason := noticeOf(t, night, dm); st != "cancelled" || reason != "merged_into_briefing" || sent(dm) != from {
			t.Fatalf("%s %s", st, reason)
		}
		if f := failedFor(night, dm); len(f) != 1 || f[0]["reason"] != "merged_into_briefing" || f[0]["final"] != true || f[0]["attempt"] != float64(0) {
			t.Fatalf("%+v", f)
		}
		res := compose(t, dm, at(12, 7, 31))
		b := obj(t, expect(t, dm, false, "GET", "/v1/channels/briefings/"+res.BriefingID, "", 200))
		overnight := false
		for _, s := range b["sections"].([]any) {
			for _, it := range s.(map[string]any)["items"].([]any) {
				m := it.(map[string]any)
				if m["signal_id"] == night {
					overnight = m["arrived_overnight"] == true && m["notified"] != true
				}
			}
		}
		if !overnight {
			t.Fatalf("并入简报的条目标夜间到达：%+v", b)
		}
		card, ok := cardsOf(t, dm, from)[night]
		if !ok || !slices.ContainsFunc(card.Card.Lines, func(l string) bool { return strings.Contains(l, "夜间到达") }) {
			t.Fatalf("简报中的卡片标夜间到达：%+v %q", card, card.Card.Lines)
		}
		// 周五夜间到达、周六 07:00 结束：下一期在周一，照常单独推送
		from = sent(dm)
		weekend := newSignal(t, at(13, 23, 0), pursueBid("夹具总包丁"))
		deliverN(t, at(13, 23, 5), 2)
		deliverN(t, at(14, 7, 0), 2)
		if st, _ := noticeOf(t, weekend, dm); st != "delivered" {
			t.Fatal(st)
		}
		if _, ok := cardsOf(t, dm, from)[weekend]; !ok {
			t.Fatal("周末结束后照常推送")
		}
	})

	run("C-08 越权与反例测试 节流：每名收件人每小时至多 6 张即时通知卡片，超出的不单独推送（throttled，信号照常进入简报）；窗口过后恢复", func(t *testing.T) {
		from := sent(dm)
		var burst []string
		for i := range 7 {
			burst = append(burst, newSignal(t, at(16, 10, i), pursueBid(fmt.Sprintf("夹具节流%d", i))))
		}
		deliverN(t, at(16, 10, 10), 2)
		states := map[string]int{}
		for _, s := range burst {
			st, reason := noticeOf(t, s, dm)
			states[st+"/"+reason]++
		}
		if states["delivered/"] != 6 || states["cancelled/throttled"] != 1 || len(cardsOf(t, dm, from)) != 6 {
			t.Fatalf("%+v", states)
		}
		later := newSignal(t, at(16, 11, 30), pursueBid("夹具节流后"))
		deliverN(t, at(16, 11, 31), 2)
		if st, _ := noticeOf(t, later, dm); st != "delivered" {
			t.Fatalf("窗口过后恢复：%s", st)
		}
	})

	run("C-08 越权与反例测试 失去目标可见性与非成员：受众为全部参与人时非参与人收不到；展开后、发送前被移出目标的不发送、不签发令牌（objective_not_visible）；首次发送失败后被移出的，重试前再检查而取消；只读参与人的卡片只有“查看”", func(t *testing.T) {
		activate(t, map[string]any{"audience": []string{"objective_members"}, "mode": "immediate"}, "跟进候选即时发给全部参与人")
		from := map[string]int{dm: sent(dm), bd: sent(bd), lv: sent(lv), expert: sent(expert), bd2: sent(bd2)}
		s1 := newSignal(t, at(17, 10, 0), pursueBid("夹具成员甲"))
		deliverN(t, at(17, 10, 1), 1) // 只展开
		if count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE signal_id = $1 AND recipient <> '*'`, s1) != 5 ||
			count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE signal_id = $1 AND recipient = $2`, s1, bd2) != 0 {
			t.Fatal("受众为负责人与参与人，非参与人不在其中")
		}
		// 发送前 bd 被移出目标（本地提交后立即撤权）
		setParticipants(t, lv, admin, expert)
		deliverN(t, at(17, 10, 2), 2)
		if st, reason := noticeOf(t, s1, bd); st != "cancelled" || reason != "objective_not_visible" || tokensFor(t, s1, bd) != 0 || len(cardsOf(t, bd, from[bd])) != 0 {
			t.Fatalf("%s %s", st, reason)
		}
		for _, s := range []string{dm, lv, expert} {
			if st, _ := noticeOf(t, s1, s); st != "delivered" {
				t.Fatalf("%s：%s", s, st)
			}
		}
		if len(cardsOf(t, bd2, from[bd2])) != 0 {
			t.Fatal("非参与人不得收到")
		}
		// 业务专家只读：卡片只有“查看”
		ex := cardsOf(t, expert, from[expert])[s1]
		if ex.Card == nil || len(ex.Card.Buttons) != 1 || ex.Card.Buttons[0].Key != "view" {
			t.Fatalf("%+v", ex.Card)
		}
		// 重试前再检查：全部首次发送失败，之后 lv 被移出，重试时 lv 取消、其余以同一张卡片送达
		setParticipants(t, bd, lv, admin, expert)
		s2 := newSignal(t, at(17, 11, 0), pursueBid("夹具成员乙"))
		wc.SetSendFailure(true)
		deliverN(t, at(17, 11, 1), 2)
		wc.SetSendFailure(false)
		var firstCard string
		if err := chnOwner.QueryRow(ctx, `SELECT message->'Card'->>'CardID' FROM chn_notices WHERE signal_id = $1 AND recipient = $2 AND state = 'retrying'`, s2, bd).
			Scan(&firstCard); err != nil || firstCard == "" {
			t.Fatalf("首次发送失败后待重试：%v", err)
		}
		if f := failedFor(s2, lv); len(f) != 1 || f[0]["state"] != "retrying" || f[0]["final"] != false {
			t.Fatalf("%+v", f)
		}
		setParticipants(t, bd, admin, expert)
		from[lv], from[bd] = sent(lv), sent(bd)
		deliverN(t, at(17, 11, 5), 2)
		if st, reason := noticeOf(t, s2, lv); st != "cancelled" || reason != "objective_not_visible" || len(cardsOf(t, lv, from[lv])) != 0 {
			t.Fatalf("重试前失去可见性：%s %s", st, reason)
		}
		got, ok := cardsOf(t, bd, from[bd])[s2]
		if !ok || got.Card.CardID != firstCard {
			t.Fatalf("重试发送同一张卡片：%+v", got)
		}
		checkWhitelist(t)
	})

	run("C-08 越权与反例测试 来源权利撤销：首次发送失败后来源权利收紧（full_text→none、→citation_only），重试发送的同一张卡片按当前权利重新裁剪，不再显示中标方与金额；权利不可得时按 none", func(t *testing.T) {
		from := sent(dm)
		s := newSignal(t, at(18, 10, 0), pursueBid("夹具权利总包"))
		wc.SetSendFailure(true)
		deliverN(t, at(18, 10, 1), 2)
		wc.SetSendFailure(false)
		rights.set(s, "winner", chapi.FieldRight{Levels: []string{"none"}})
		rights.set(s, "amount", chapi.FieldRight{Levels: []string{"citation_only"}, SourceURL: "https://example.com/notice/rights"})
		deliverN(t, at(18, 10, 5), 2)
		card, ok := cardsOf(t, dm, from)[s]
		if !ok {
			t.Fatal("重试应送达")
		}
		joined := strings.Join(card.Card.Lines, "\n")
		if strings.Contains(joined, "夹具权利总包") || strings.Contains(joined, "600") || !strings.Contains(joined, "中标方：见详情") ||
			!strings.Contains(joined, "见原文 https://example.com/notice/rights") {
			t.Fatalf("重试按当前权利裁剪：%s", joined)
		}
		// 权利服务不可得：失败关闭为 none
		s3 := newSignal(t, at(18, 11, 0), pursueBid("夹具权利不可得"))
		rights.fail.Store(true)
		deliverN(t, at(18, 11, 1), 2)
		rights.fail.Store(false)
		card, ok = cardsOf(t, dm, from)[s3]
		if !ok || strings.Contains(strings.Join(card.Card.Lines, "\n"), "夹具权利不可得") {
			t.Fatalf("%+v", card)
		}
	})

	run("C-08 越权与反例测试 另一租户、伪造与重放的事件：重放不新增通知、不重复发送；他租户信封携带本租户的信号与目标不展开任何收件人；伪造的信号、处置与目标在发送前以收件人本人再检查而取消，不渲染、不签发令牌；投递统计只给本租户管理员", func(t *testing.T) {
		activate(t, map[string]any{"audience": []string{"objective_owner"}, "mode": "immediate"}, "恢复为只发负责人")
		before, rows := sent(dm), count(t, chnOwner, `SELECT count(*) FROM chn_notices`)
		// 重放同一事件
		orig := envelopeOf(t, "detection.signal.detected.v1", p1)
		consume(t, orig, "channels.signal-notices")
		deliverN(t, at(19, 10, 0), 2)
		if count(t, chnOwner, `SELECT count(*) FROM chn_notices`) != rows || sent(dm) != before {
			t.Fatal("重放不得新增通知或重复发送")
		}
		forge := func(tenant string, mutate func(map[string]any)) outbox.Envelope {
			var d map[string]any
			_ = json.Unmarshal(orig.Data, &d)
			mutate(d)
			raw, _ := json.Marshal(d)
			e := orig
			e.ID, e.Tenant, e.Data = ids.New("evt"), tenant, raw
			return e
		}
		// 他租户：t2 的信封携带 t1 的信号与目标
		other := forge(t2, func(d map[string]any) {})
		consume(t, other, "channels.signal-notices")
		a2 := len(wc.Messages(corp2, account[admin2]))
		deliverN(t, at(19, 10, 5), 2)
		if len(wc.Messages(corp2, account[admin2])) != a2 || sent(dm) != before {
			t.Fatal("他租户的事件不得发送")
		}
		// 伪造：不存在的信号、把复核候选写成跟进候选、不存在的目标
		ghost := ids.New("sig")
		consume(t, forge(t1, func(d map[string]any) { d["signal_id"] = ghost }), "channels.signal-notices")
		// 复核候选 r1 写成跟进候选：真实事件已登记（同一信号与目标去重，不新增）；再模拟伪造事件先于真实事件到达
		forgedR1 := forge(t1, func(d map[string]any) { d["signal_id"] = r1 })
		n0 := count(t, chnOwner, `SELECT count(*) FROM chn_notices`)
		consume(t, forgedR1, "channels.signal-notices")
		if count(t, chnOwner, `SELECT count(*) FROM chn_notices`) != n0 {
			t.Fatal("同一信号与目标的事件去重")
		}
		if _, err := chnOwner.Exec(ctx, `DELETE FROM chn_notices WHERE signal_id = $1`, r1); err != nil {
			t.Fatal(err)
		}
		consume(t, forgedR1, "channels.signal-notices")
		fakeObj := ids.New("obj")
		fresh := newSignal(t, at(19, 10, 10), reviewBid) // 真实信号、伪造目标
		// 同样模拟伪造事件先于真实事件到达（真实事件已登记时同一信号去重）
		if _, err := chnOwner.Exec(ctx, `DELETE FROM chn_notices WHERE signal_id = $1`, fresh); err != nil {
			t.Fatal(err)
		}
		consume(t, forge(t1, func(d map[string]any) { d["signal_id"], d["objective_id"] = fresh, fakeObj }), "channels.signal-notices")
		deliverN(t, at(19, 10, 11), 2)
		if st, reason := noticeOf(t, ghost, dm); st != "cancelled" || reason != "signal_not_visible" || tokensFor(t, ghost, dm) != 0 {
			t.Fatalf("不存在的信号：%s %s", st, reason)
		}
		if st, reason := noticeOf(t, r1, dm); st != "cancelled" || reason != "not_immediate" || count(t, chnOwner,
			`SELECT count(*) FROM chn_notices WHERE signal_id = $1 AND recipient = $2 AND message IS NOT NULL`, r1, dm) != 0 {
			t.Fatalf("伪造的处置：%s %s", st, reason)
		}
		if count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE signal_id = $1 AND objective_id = $2 AND state = 'expanded' AND expanded_recipients = 0`, fresh, fakeObj) != 1 {
			t.Fatal("伪造的目标不得展开收件人")
		}
		if sent(dm) != before {
			t.Fatal("伪造的事件不得发送任何卡片")
		}
		checkWhitelist(t)
		// 投递统计：只给本租户管理员，按种类、状态与白名单原因计数，不含收件人与信号
		stats := obj(t, expect(t, admin, false, "GET", "/v1/channels/delivery-stats?since=2026-11-01T00:00:00Z", "", 200))
		got := map[string]float64{}
		for _, n := range stats["notices"].([]any) {
			m := n.(map[string]any)
			if m["kind"] != "signal_immediate" {
				continue
			}
			reason, _ := m["reason"].(string)
			got[m["state"].(string)+"/"+reason] += m["count"].(float64)
			for k := range m {
				if !slices.Contains([]string{"kind", "state", "reason", "count"}, k) {
					t.Fatalf("统计含白名单外的字段 %s", k)
				}
			}
		}
		for _, k := range []string{"delivered/", "cancelled/throttled", "cancelled/merged_into_briefing", "cancelled/objective_not_visible", "cancelled/signal_not_visible",
			"cancelled/not_immediate"} {
			if got[k] == 0 {
				t.Fatalf("统计缺 %s：%+v", k, got)
			}
		}
		expect(t, bd, false, "GET", "/v1/channels/delivery-stats", "", 403, "forbidden")
		other2 := obj(t, expectAs(t, admin2, t2, false, "GET", "/v1/channels/delivery-stats?since=2026-11-01T00:00:00Z", "", 200))
		for _, n := range other2["notices"].([]any) {
			if n.(map[string]any)["kind"] == "signal_immediate" {
				t.Fatalf("他租户的统计不得含本租户的即时通知：%+v", other2)
			}
		}
		if audits(t, "channels.notice.expanded", count2id(t, chnOwner, ctx, fresh)) != 1 {
			t.Fatal("展开应写审计")
		}
	})

	run("C-08 越权与反例测试 偏好关闭：成员关闭即时通知后立即生效、待发的取消（preference_off），不提供开关时保持当前值，重新开启后恢复；目标策略把跟进候选改为 digest 后，已展开、未发送的取消（not_immediate），新信号不再展开", func(t *testing.T) {
		clock.set(at(20, 9, 0), 0)
		prefs := obj(t, expect(t, dm, false, "GET", "/v1/channels/briefing-preferences", "", 200))
		if prefs["instant_notify"] != true {
			t.Fatalf("缺省接收即时通知：%+v", prefs)
		}
		body := func(version float64, instant string) string {
			return fmt.Sprintf(`{"expected_version":%d,"send_time":"07:30","timezone":"Asia/Shanghai","send_days":[1,2,3,4,5],"quiet_start":"21:00","quiet_end":"07:00","push_empty":false,"section_limits":[]%s}`,
				int64(version), instant)
		}
		off := obj(t, expect(t, dm, false, "PUT", "/v1/channels/briefing-preferences", body(prefs["version"].(float64), `,"instant_notify":false`), 200))
		if off["instant_notify"] != false {
			t.Fatalf("%+v", off)
		}
		from := sent(dm)
		s := newSignal(t, at(20, 10, 0), pursueBid("夹具关闭"))
		deliverN(t, at(20, 10, 1), 2)
		if st, reason := noticeOf(t, s, dm); st != "cancelled" || reason != "preference_off" || sent(dm) != from || tokensFor(t, s, dm) != 0 {
			t.Fatalf("%s %s", st, reason)
		}
		// 不提供开关：保持关闭
		kept := obj(t, expect(t, dm, false, "PUT", "/v1/channels/briefing-preferences", body(off["version"].(float64), ""), 200))
		if kept["instant_notify"] != false {
			t.Fatalf("%+v", kept)
		}
		on := obj(t, expect(t, dm, false, "PUT", "/v1/channels/briefing-preferences", body(kept["version"].(float64), `,"instant_notify":true`), 200))
		s2 := newSignal(t, at(20, 11, 0), pursueBid("夹具重新开启"))
		deliverN(t, at(20, 11, 1), 2)
		if st, _ := noticeOf(t, s2, dm); st != "delivered" || on["instant_notify"] != true {
			t.Fatalf("重新开启后恢复：%s", st)
		}
		// 目标的策略偏好：已展开、未发送时改为 digest
		s3 := newSignal(t, at(20, 12, 0), pursueBid("夹具改为摘要"))
		deliverN(t, at(20, 12, 1), 1) // 只展开
		if st, _ := noticeOf(t, s3, dm); st != "pending" {
			t.Fatal(st)
		}
		activate(t, map[string]any{"audience": []string{"objective_owner"}, "mode": "digest"}, "跟进候选改为只进入简报")
		deliverN(t, at(20, 12, 5), 2)
		if st, reason := noticeOf(t, s3, dm); st != "cancelled" || reason != "not_immediate" || tokensFor(t, s3, dm) != 0 {
			t.Fatalf("%s %s", st, reason)
		}
		s4 := newSignal(t, at(20, 13, 0), pursueBid("夹具摘要"))
		deliverN(t, at(20, 13, 1), 2)
		if noticeCount(t, s4) != 1 || count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE signal_id = $1 AND state = 'expanded' AND expanded_recipients = 0`, s4) != 1 {
			t.Fatal("digest 不展开")
		}
		if audits(t, "channels.briefing_preferences.updated", dm) < 3 {
			t.Fatal("偏好变更写审计")
		}
	})

	// 简报中各信号条目（只取完整展示的）
	briefingItems := func(t *testing.T, subject, id string) map[string]map[string]any {
		t.Helper()
		out := map[string]map[string]any{}
		b := obj(t, expect(t, subject, false, "GET", "/v1/channels/briefings/"+id, "", 200))
		for _, s := range b["sections"].([]any) {
			for _, it := range s.(map[string]any)["items"].([]any) {
				m := it.(map[string]any)
				if m["kind"] == "signal" {
					out[m["signal_id"].(string)] = m
				}
			}
		}
		return out
	}
	// 某信号推送给收件人的信号卡片张数（即时卡片与简报卡片同一模板）
	cardCount := func(t *testing.T, subject, signal string, from int) int {
		t.Helper()
		n := 0
		for _, m := range wc.Messages(corp1, account[subject])[from:] {
			if m.Kind != "signal_card" || m.Card == nil {
				continue
			}
			if _, _, target := linkTarget(t, button(*m.Card, "view").URL); target == signal {
				n++
			}
		}
		return n
	}

	run("C-08 越权与反例测试 简报先于展开：收件人 '*' 的行尚未展开时组装的简报照常推送该信号，之后展开出的通知在发送闸门处发现本人已有含该信号的简报而并入（merged_into_briefing），不发即时卡片；先展开后组装的照常并入；组装前已过发送闸门（发送失败待重试）的标“已通知”、简报不推送其卡片", func(t *testing.T) {
		activate(t, map[string]any{"audience": []string{"objective_owner"}, "mode": "immediate"}, "恢复跟进候选即时通知")
		compose(t, dm, at(23, 7, 31)) // 前移简报窗口，下一期只含本用例的信号
		// 组装前已过发送闸门：第一次发送失败，待重试
		wc.SetSendFailure(true)
		attempted := newSignal(t, at(24, 7, 5), pursueBid("夹具闸门已过"))
		deliverN(t, at(24, 7, 6), 2)
		if st, _ := noticeOf(t, attempted, dm); st != "retrying" {
			t.Fatal(st)
		}
		// 先展开后组装：组装时已有本人的待发送行
		expandedFirst := newSignal(t, at(24, 7, 15), pursueBid("夹具先展开"))
		clock.set(at(24, 7, 16), 0)
		r := &scheduler.Runner{}
		if o := r.Once(ctx, scheduler.Bound{Job: noticeJob, DB: chns.env.DB}); o != scheduler.Ran { // 只展开（失败待重试的再次失败）
			t.Fatal(o)
		}
		pump(t)
		wc.SetSendFailure(false)
		if st, _ := noticeOf(t, expandedFirst, dm); st != "pending" {
			t.Fatal(st)
		}
		if st, _ := noticeOf(t, attempted, dm); st != "retrying" {
			t.Fatal(st)
		}
		// 简报先于展开：组装时只有收件人 '*' 的行
		composedFirst := newSignal(t, at(24, 7, 20), pursueBid("夹具简报先于展开"))
		if count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE signal_id = $1 AND recipient = '*' AND state = 'pending'`, composedFirst) != 1 {
			t.Fatal("组装前尚未展开")
		}
		from := sent(dm)
		res := compose(t, dm, at(24, 7, 31))
		items := briefingItems(t, dm, res.BriefingID)
		for _, sig := range []string{composedFirst, expandedFirst} {
			if items[sig] == nil || items[sig]["notified"] == true || cardCount(t, dm, sig, from) != 1 {
				t.Fatalf("简报照常列出并推送卡片：%s %+v", sig, items[sig])
			}
		}
		if items[attempted] == nil || items[attempted]["notified"] != true || cardCount(t, dm, attempted, from) != 0 {
			t.Fatalf("已过发送闸门的标“已通知”、不推送卡片：%+v", items[attempted])
		}
		if st, reason := noticeOf(t, expandedFirst, dm); st != "cancelled" || reason != "merged_into_briefing" {
			t.Fatalf("先展开后组装：%s %s", st, reason)
		}
		// 组装之后展开与发送：发送闸门发现本人已有含该信号的简报
		after := sent(dm)
		deliverN(t, at(24, 7, 40), 3)
		if st, reason := noticeOf(t, composedFirst, dm); st != "cancelled" || reason != "merged_into_briefing" || cardCount(t, dm, composedFirst, after) != 0 {
			t.Fatalf("简报先于展开：%s %s", st, reason)
		}
		if f := failedFor(composedFirst, dm); len(f) != 1 || f[0]["reason"] != "merged_into_briefing" || f[0]["final"] != true || f[0]["attempt"] != float64(0) {
			t.Fatalf("%+v", f)
		}
		if count(t, chnOwner, `SELECT count(*) FROM chn_notices WHERE signal_id = $1 AND recipient = $2 AND message IS NOT NULL`, composedFirst, dm) != 0 {
			t.Fatal("并入的通知不渲染、不签发令牌")
		}
		// 已过闸门的照常重试送达，仍只有一张卡片
		if st, _ := noticeOf(t, attempted, dm); st != "delivered" || cardCount(t, dm, attempted, from) != 1 {
			t.Fatal(st)
		}
		// 并发：组装与发送同时进行（周三至周五各一期）。两者在同一把收件人咨询锁下“检查后写入”：发送闸门先提交的，组装保存时发现状态变化
		// 而重做并标“已通知”；组装先提交的，发送闸门发现本人已有含该信号的简报而并入。无论谁先，同一信号只以一张卡片送达，且简报的“已通知”
		// 与通知状态一致
		for day := 25; day <= 27; day++ {
			concurrent := newSignal(t, at(day, 7, 20), pursueBid(fmt.Sprintf("夹具并发%d", day)))
			clock.set(at(day, 7, 21), 0)
			if o := r.Once(ctx, scheduler.Bound{Job: noticeJob, DB: chns.env.DB}); o != scheduler.Ran { // 只展开
				t.Fatal(o)
			}
			pump(t)
			from := sent(dm)
			clock.set(at(day, 7, 31), 0)
			var wg sync.WaitGroup
			var composed *chapi.ComposeResult
			var composeErr error
			var outcome scheduler.Outcome
			wg.Add(2)
			go func() {
				defer wg.Done()
				composed, composeErr = briefings.Compose(svc(t1), dm, at(day, 7, 31))
			}()
			go func() {
				defer wg.Done()
				outcome = (&scheduler.Runner{}).Once(ctx, scheduler.Bound{Job: noticeJob, DB: chns.env.DB})
			}()
			wg.Wait()
			if composeErr != nil || outcome != scheduler.Ran {
				t.Fatal(composeErr, outcome)
			}
			deliverN(t, at(day, 7, 33), 2)
			st, reason := noticeOf(t, concurrent, dm)
			items := briefingItems(t, dm, composed.BriefingID)
			if items[concurrent] == nil || cardCount(t, dm, concurrent, from) != 1 {
				t.Fatalf("并发时只送达一张卡片：%s %s %d", st, reason, cardCount(t, dm, concurrent, from))
			}
			switch {
			case st == "delivered" && items[concurrent]["notified"] == true:
			case st == "cancelled" && reason == "merged_into_briefing" && items[concurrent]["notified"] != true:
			default:
				t.Fatalf("简报标记与通知状态不一致：%s %s %+v", st, reason, items[concurrent])
			}
		}
	})
}

// count2id 返回信号的待展开行（收件人 '*'）的通知标识（展开审计的资源）。
func count2id(t *testing.T, owner *pgx.Conn, ctx context.Context, signal string) string {
	t.Helper()
	var id string
	if err := owner.QueryRow(ctx, `SELECT id FROM chn_notices WHERE signal_id = $1 AND recipient = '*'`, signal).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
