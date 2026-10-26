package integration_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
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
	detapi "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/channels"
	chapi "github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api/wecomtest"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	ccapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
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

// flakyBriefing 是 channels 看到的 detection 简报接口（经 mTLS 内部端口的远程绑定）：可注入取数失败与“已提交而响应丢失”。
type flakyBriefing struct {
	detectionapi.Briefing
	failSignals atomic.Bool
	loseTriage  atomic.Int64 // 之后的这么多次处理在 detection 执行后向调用方报告暂时失败
}

func (f *flakyBriefing) BriefingSignals(ctx context.Context, objective string, from, to time.Time) (*detectionapi.BriefingSignalPage, error) {
	if f.failSignals.Load() {
		return nil, binding.ErrTransient
	}
	return f.Briefing.BriefingSignals(ctx, objective, from, to)
}

func (f *flakyBriefing) Triage(ctx context.Context, signal, key string, req *detectionapi.TriageRequest) (*detectionapi.TriageResult, error) {
	res, err := f.Briefing.Triage(ctx, signal, key, req)
	if err == nil && f.loseTriage.Load() > 0 {
		f.loseTriage.Add(-1)
		return nil, binding.ErrTransient
	}
	return res, err
}

// countedAccess 在“先放过 skip 次、再失败 fail 次”的位置注入目标访问判断的失败（channels 与 detection 共用同一绑定）。
type countedAccess struct {
	ccapi.ObjectiveAccess
	mu         sync.Mutex
	skip, fail int
}

func (f *countedAccess) arm(skip, fail int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skip, f.fail = skip, fail
}

func (f *countedAccess) Access(ctx context.Context, objective string, relations []string) ([]string, error) {
	f.mu.Lock()
	inject := false
	switch {
	case f.skip > 0:
		f.skip--
	case f.fail > 0:
		f.fail--
		inject = true
	}
	f.mu.Unlock()
	if inject {
		return nil, errors.New("注入：目标访问判断不可用")
	}
	return f.ObjectiveAccess.Access(ctx, objective, relations)
}

// fakeCoverage 是标明为夹具的覆盖端口（domain-intelligence GetCoverage 未接入）。
type fakeCoverage struct {
	mu          sync.Mutex
	interrupted map[string]time.Time
}

func (f *fakeCoverage) Coverage(_ context.Context, domains []string) ([]chapi.DomainCoverage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []chapi.DomainCoverage
	for _, d := range domains {
		if at, ok := f.interrupted[d]; ok {
			out = append(out, chapi.DomainCoverage{Domain: d, Interrupted: true, AsOf: at})
			continue
		}
		out = append(out, chapi.DomainCoverage{Domain: d, AsOf: time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)})
	}
	return out, nil
}

// fakeRights 是标明为夹具的来源权利端口：缺省全部 full_text，个别信号的个别字段另行指定。
type fakeRights struct {
	mu       sync.Mutex
	override map[string]map[string]chapi.FieldRight
	fail     atomic.Bool
}

func (f *fakeRights) set(signal, field string, r chapi.FieldRight) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.override[signal] == nil {
		f.override[signal] = map[string]chapi.FieldRight{}
	}
	f.override[signal][field] = r
}

func (f *fakeRights) FieldRights(_ context.Context, signals []string) (map[string]map[string]chapi.FieldRight, error) {
	if f.fail.Load() {
		return nil, errors.New("注入：来源权利不可得")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]chapi.FieldRight{}
	for _, s := range signals {
		out[s] = map[string]chapi.FieldRight{}
		for _, field := range []string{"project", "winner", "amount", "region", "categories"} {
			out[s][field] = chapi.FieldRight{Levels: []string{"full_text"}}
		}
		for field, r := range f.override[s] {
			out[s][field] = r
		}
	}
	return out, nil
}

// PRD 第5段服务端切片（简报与卡片快捷动作）：真实 PostgreSQL（各子系统自己的库、行级安全、应用账号）+ 真实 identity 目录与 OpenFGA +
// 外部网关令牌校验 + 生成的 HTTP 服务端。channels 读取 detection 的简报取数与卡片、identity 的审批经 mTLS 内部端口的远程绑定；
// 企业微信为内存实现（wecomtest），覆盖与来源权利为标明为夹具的端口；定时任务与工作进程未接入，测试以本地绑定 Briefings 直接驱动
// 组装、投递与动作执行，业务时钟由测试控制。用例编号对应 PRD 第5段第12节。
func TestBriefingQuickActions(t *testing.T) {
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
	if os.Getenv("BRIEFING_DEBUG") != "" {
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
			return base64.StdEncoding.EncodeToString([]byte("briefing-test-signing-key-0123456789abcdef"))
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
	localAccess, _ := runtime.Lookup[ccapi.ObjectiveAccess](bindings, ccapi.AccessBinding)
	access := &countedAccess{ObjectiveAccess: localAccess}
	bindings.Provide(ccapi.AccessBinding, ccapi.ObjectiveAccess(access))

	today := now().UTC().Truncate(24 * time.Hour)
	const org1 = "org_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	fixture := datasettest.New(today.AddDate(0, 0, -365))
	fixture.Add(datasettest.A02(today, org1)...)
	lrnModule := learning.ModuleWith(learning.Adapters{Dataset: fixture, Domains: fixture, Now: now})
	lrns := start(lrnModule)
	lrnModule.Routes(lrns.env, public, internal)
	facts := facttest.New(t1, t2)
	detModule := detection.ModuleWith(detection.Adapters{Facts: facts, Candidates: facts, Now: now})
	dets := start(detModule)
	detModule.Routes(dets.env, public, internal)

	// mTLS 内部端口：channels 经远程绑定读取 detection 的简报取数与卡片、identity 的审批
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
	flaky := &flakyBriefing{Briefing: remoteBriefing}
	bindings.Provide(detectionapi.BriefingBinding, detectionapi.Briefing(flaky))
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
	coverage := &fakeCoverage{interrupted: map[string]time.Time{}}
	rights := &fakeRights{override: map[string]map[string]chapi.FieldRight{}}
	// 第6段：channels 依赖 pursuit 的渠道内动作；本用例不开启卡片“跟进”入口，以未接入的实现占位
	bindings.Provide(pursuitapi.ActionsBinding, pursuitapi.Actions(unwiredPursuits{}))
	chnModule := channels.ModuleWith(channels.Adapters{WeCom: wc, Coverage: coverage, Rights: rights, Now: clock.Now})
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
	expectAs := func(t *testing.T, subject, tenant string, mfa bool, method, path, body, key string, status int, code ...string) []byte {
		t.Helper()
		got, raw := do(t, subject, tenant, mfa, method, path, body, key)
		if got != status || (len(code) > 0 && codeOf(raw) != code[0]) {
			t.Fatalf("%s %s %s：得到 %d，期望 %d %v：%s", subject, method, path, got, status, code, raw)
		}
		return raw
	}
	expect := func(t *testing.T, subject string, mfa bool, method, path, body string, status int, code ...string) []byte {
		t.Helper()
		return expectAs(t, subject, t1, mfa, method, path, body, "", status, code...)
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
	num := func(m map[string]any, k string) int64 {
		v, _ := m[k].(float64)
		return int64(v)
	}
	count := func(t *testing.T, owner *pgx.Conn, sql string, args ...any) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 发件箱泵：全部事件按契约校验；detection 的信号事件交给 learning，identity 的成员事件交给 channels
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
		"detection.signal.detected.v1":          {{lrnModule, lrns.env, "learning.signal-feedback"}},
		"detection.signal.triaged.v1":           {{lrnModule, lrns.env, "learning.signal-feedback"}},
		"identity.member.changed.v1":            {{chnModule, chns.env, "channels.member-changes"}},
	}
	var delivered []outbox.Envelope
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
					delivered = append(delivered, e)
					for _, r := range routes[e.Type] {
						for _, c := range r.module.Consumers(r.env) {
							if c.Name != r.consumer {
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
	audits := func(t *testing.T, action, resource string) int {
		t.Helper()
		q := `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1' AND envelope->'data'->>'action' = $1`
		args := []any{action}
		if resource != "" {
			q += ` AND envelope->'data'->>'resource' = $2`
			args = append(args, resource)
		}
		return count(t, chnOwner, q, args...)
	}

	// 公共事实与匹配（同 TestSignalInboxFeedback）
	fact := func(t *testing.T, bids ...facttest.Bid) (string, outbox.Envelope) {
		t.Helper()
		notice := ids.New("ntc")
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
			facts.Put(notice, 1, facttest.BidResult(bids[i]))
		}
		data, _ := json.Marshal(map[string]any{"info_domain": "INF-PROC", "fact_id": notice, "changed_fields": []string{"/results"}, "rights_tags": []string{"fixture"},
			"snapshot_ref": ids.New("snp"), "revision_ref": ids.New("exr"), "object_type": "tendering.notice", "fact_version": 1, "schema_version": "1.0.0"})
		return notice, outbox.Envelope{SpecVersion: "1.0", ID: ids.New("evt"), Source: "miaoqia/domain-intelligence", Type: "domain.fact.upserted.v1",
			Time: now().UTC().Add(-time.Minute).Truncate(time.Millisecond), DataContentType: "application/json", Zone: reqctx.ZonePublic, Data: data}
	}
	mustMatch := func(t *testing.T, envelope outbox.Envelope) {
		t.Helper()
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
	}
	sig := func(t *testing.T, notice string) []string {
		t.Helper()
		rows, err := dets.owner.Query(ctx, `SELECT id FROM det_signals WHERE subject_id = $1 ORDER BY part_id`, notice)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	newSignal := func(t *testing.T, at time.Time, bid facttest.Bid) string {
		t.Helper()
		n, e := fact(t, bid)
		mustMatch(t, e)
		s := sig(t, n)
		if len(s) != 1 {
			t.Fatalf("%v", s)
		}
		if _, err := dets.owner.Exec(ctx, `UPDATE det_signals SET created_at = $2 WHERE id = $1`, s[0], at); err != nil {
			t.Fatal(err)
		}
		return s[0]
	}

	// 档案与 A02 目标：决策者 dm 启用；业务开拓者 bd、lv，管理员 admin 与业务专家 expert 参与；bd2 不参与
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
	// requestActivation 提交一次激活请求（必要时从生效表单起草并等待仿真），返回待批准的审批
	requestActivation := func(t *testing.T, note string) string {
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
		pump(t)
		return r.ApprovalID
	}
	approve := func(t *testing.T, approvalID string) {
		t.Helper()
		var a struct {
			Version int64 `json:"version"`
		}
		decode(t, expect(t, dm2, false, "GET", "/v1/identity/approvals/"+approvalID, "", 200), &a)
		expect(t, dm2, true, "POST", "/v1/identity/approvals/"+approvalID+"/decide", fmt.Sprintf(`{"expected_version":%d,"decision":"approve"}`, a.Version), 200)
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
	approve(t, requestActivation(t, "首次激活"))

	// 周期（Asia/Shanghai，工作日 07:30）：P1 [周一 10-19, 周二 10-20)、P2 至周三、P3 至周四、P4 至周五、P5 至下周一 10-26
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, shanghai) }
	end1, end2, end3, end4, end5 := at(20, 7, 30), at(21, 7, 30), at(22, 7, 30), at(23, 7, 30), at(26, 7, 30)
	start1 := at(19, 7, 30)

	// P1 的信号：跟进候选 pursue、复核 review、观察 watch、两家同标包 lotA/lotB、排除 excluded；handled 组装前已被处理；backfill 为回填
	inP1 := at(19, 20, 0)
	pursue := newSignal(t, inP1, facttest.Bid{Amount: 6_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}, WinnerName: "夹具总包甲"})
	review := newSignal(t, inP1.Add(time.Minute), facttest.Bid{Amount: 1_000_000, CategoryConfidence: 0.5, WinnerConfidence: 0.5})
	watch := newSignal(t, inP1.Add(2*time.Minute), facttest.Bid{Candidate: true, Amount: 800_000})
	excluded := newSignal(t, inP1.Add(3*time.Minute), facttest.Bid{Winner: org1, Amount: 3_000_000})
	handled := newSignal(t, inP1.Add(4*time.Minute), facttest.Bid{Amount: 2_500_000, HistoryCategories: []string{"CAT-PIPE-PE"}})
	backfill := newSignal(t, inP1.Add(5*time.Minute), facttest.Bid{Amount: 2_600_000, HistoryCategories: []string{"CAT-PIPE-PE"}})
	if _, err := dets.owner.Exec(ctx, `UPDATE det_signals SET backfill = true WHERE id = $1`, backfill); err != nil {
		t.Fatal(err)
	}
	// 本期之外的信号不列入
	before := newSignal(t, start1.Add(-time.Hour), facttest.Bid{Amount: 4_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}})
	// pursue 的中标方来源只允许引用与链接（用例14）
	rights.override[pursue] = map[string]chapi.FieldRight{"winner": {Levels: []string{"full_text", "citation_only"}, SourceURL: "https://example.com/notice/1"}}

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
	bindIDs := map[string]string{}
	bind := func(tenant, inst, corp, subject, account string) {
		id := ids.New("chb")
		bindIDs[subject] = id
		if _, err := chnOwner.Exec(ctx, `INSERT INTO chn_bindings (id, tenant_id, installation_id, channel, corp_id, channel_user_id, display_name, subject, member_version, state, version, bound_at)
 VALUES ($1, $2, $3, 'wecom', $4, $5, $6, $7, 1, 'bound', 1, now())`, id, tenant, inst, corp, account, "成员"+account, subject); err != nil {
			t.Fatal(err)
		}
	}
	account := map[string]string{bd: "u-bd", dm: "u-dm", expert: "u-ex", lv: "u-lv", bd2: "u-bd2", admin2: "u-a2"}
	for _, s := range []string{bd, dm, expert, lv, bd2} {
		bind(t1, inst1, corp1, s, account[s])
	}
	bind(t2, inst2, corp2, admin2, account[admin2])

	svc := func(tenant string) context.Context {
		return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:briefing-scheduler", Kind: "service"}})
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
	deliverDue := func(t *testing.T, when time.Time) {
		t.Helper()
		clock.set(when, 0)
		if _, err := briefings.DeliverDue(svc(t1), 100); err != nil {
			t.Fatal(err)
		}
		pump(t)
	}
	process := func(t *testing.T, tenant string) {
		t.Helper()
		if _, err := briefings.ProcessActions(svc(tenant), 50); err != nil {
			t.Fatal(err)
		}
		pump(t)
	}
	var evSeq atomic.Int64
	callback := func(corp, eventID, body string) (*chapi.InboundResult, error) {
		return callbacks.ReceiveInbound(gw, "wecom", wc.Sign(&chapi.Callback{CorpID: corp, EventID: eventID, Timestamp: clock.Now(), Nonce: "n", Body: body}))
	}
	// click 让渠道账号点击卡片上的按钮（新的渠道事件），返回回调结果与事件标识
	click := func(t *testing.T, corp, accountID, cardID, token string, want chapi.InboundOutcome) string {
		t.Helper()
		ev := fmt.Sprintf("act-%s-%d", suffix, evSeq.Add(1))
		res, err := callback(corp, ev, wecomtest.ActionBody(accountID, cardID, token))
		if err != nil || res.Outcome != want {
			t.Fatalf("%+v %v，期望 %s", res, err, want)
		}
		return ev
	}
	type sentCard struct {
		card   chapi.Card
		signal string
		msg    chapi.Message
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
	// cards 返回发给账号的消息中，自第 from 条起的信号卡片（按“查看”链接解析信号）
	cards := func(t *testing.T, corp, accountID string, from int) []sentCard {
		t.Helper()
		var out []sentCard
		for _, m := range wc.Messages(corp, accountID)[from:] {
			if m.Kind != "signal_card" || m.Card == nil {
				continue
			}
			_, kind, target := linkTarget(t, button(*m.Card, "view").URL)
			if kind != "signal" {
				t.Fatalf("%+v", m.Card)
			}
			out = append(out, sentCard{card: *m.Card, signal: target, msg: m})
		}
		return out
	}
	cardFor := func(t *testing.T, list []sentCard, signal string) sentCard {
		t.Helper()
		for _, c := range list {
			if c.signal == signal {
				return c
			}
		}
		t.Fatalf("没有 %s 的卡片：%d 张", signal, len(list))
		return sentCard{}
	}
	lastUpdate := func(t *testing.T, corp, accountID string) chapi.Message {
		t.Helper()
		u := wc.Updates(corp, accountID)
		if len(u) == 0 {
			t.Fatal("没有卡片更新")
		}
		return u[len(u)-1]
	}
	signalRow := func(t *testing.T, who, id string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, false, "GET", "/v1/detection/signals/"+id, "", 200))["signal"].(map[string]any)
	}
	tokenState := func(t *testing.T, token string) (state string, attempts int) {
		t.Helper()
		raw, _ := base64.RawURLEncoding.DecodeString(token)
		var claims struct {
			ID string `json:"i"`
		}
		if err := json.Unmarshal(raw[:len(raw)-32], &claims); err != nil {
			t.Fatal(err)
		}
		if err := chnOwner.QueryRow(ctx, `SELECT state, attempts FROM chn_action_tokens WHERE id = $1`, claims.ID).Scan(&state, &attempts); err != nil {
			t.Fatal(err)
		}
		return
	}
	tokenID := func(token string) string {
		raw, _ := base64.RawURLEncoding.DecodeString(token)
		var claims struct {
			ID string `json:"i"`
		}
		_ = json.Unmarshal(raw[:len(raw)-32], &claims)
		return claims.ID
	}
	opsFor := func(t *testing.T, operation string) int {
		t.Helper()
		return count(t, dets.owner, `SELECT count(*) FROM det_triage_ops WHERE operation_id = $1`, operation)
	}
	getBriefing := func(t *testing.T, who, id string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, false, "GET", "/v1/channels/briefings/"+id, "", 200))
	}
	sectionsOf := func(b map[string]any) []map[string]any {
		var out []map[string]any
		for _, s := range b["sections"].([]any) {
			out = append(out, s.(map[string]any))
		}
		return out
	}
	objectiveSection := func(t *testing.T, b map[string]any) map[string]any {
		t.Helper()
		for _, s := range sectionsOf(b) {
			if s["kind"] == "objective" {
				return s
			}
		}
		t.Fatalf("没有目标区：%+v", b)
		return nil
	}
	itemIDs := func(sec map[string]any) []string {
		var out []string
		for _, it := range sec["items"].([]any) {
			m := it.(map[string]any)
			if id, ok := m["signal_id"].(string); ok {
				out = append(out, id)
			}
		}
		return out
	}

	// 后续用例依赖前面的状态：任何一步失败即停止
	run := func(name string, f func(t *testing.T)) {
		if !t.Run(name, f) {
			t.FailNow()
		}
	}
	var bdBriefing1, dmBriefing1, exBriefing1 string
	var bdCards, dmCards, exCards, lvCards []sentCard
	run("用例2、3、9、14、15：每期一份（重复组装返回同一份）、周期按成员时区、已处理与回填只计数、展示级别只显示“见原文”、无反馈权限的卡片只有“查看”", func(t *testing.T) {
		// 组装前同事已忽略 handled
		r := signalRow(t, dm, handled)
		expect(t, dm, false, "POST", "/v1/detection/signals/"+handled+"/triage", fmt.Sprintf(`{"operation_id":"op-%s-h","action":"dismiss","seen_content_version":%d,"state_version":%d,"feedback_version":%d}`,
			suffix, num(r, "content_version"), num(r, "state_version"), num(r, "feedback_version")), 200)
		pump(t)
		res := compose(t, bd, end1.Add(2*time.Minute))
		if res.Outcome != "composed" || res.BriefingID == "" || res.DeliveryID == "" {
			t.Fatalf("%+v", res)
		}
		bdBriefing1 = res.BriefingID
		// 定时任务重复触发与组装重试：同一期只有一份
		for _, when := range []time.Time{end1.Add(3 * time.Minute), end1.Add(2 * time.Hour)} {
			if again := compose(t, bd, when); again.Outcome != "existing" || again.BriefingID != bdBriefing1 {
				t.Fatalf("%+v", again)
			}
		}
		if n := count(t, chnOwner, `SELECT count(*) FROM chn_briefings WHERE subject = $1`, bd); n != 1 {
			t.Fatalf("%d 份", n)
		}
		var ps, pe time.Time
		if err := chnOwner.QueryRow(ctx, `SELECT period_start, period_end FROM chn_briefings WHERE id = $1`, bdBriefing1).Scan(&ps, &pe); err != nil {
			t.Fatal(err)
		}
		if !ps.Equal(start1) || !pe.Equal(end1) {
			t.Fatalf("%v %v", ps, pe)
		}
		b := getBriefing(t, bd, bdBriefing1)
		sec := objectiveSection(t, b)
		got := itemIDs(sec)
		for _, id := range []string{pursue, review, watch} {
			if !slices.Contains(got, id) {
				t.Fatalf("缺少 %s：%v", id, got)
			}
		}
		for _, id := range []string{excluded, handled, backfill, before} {
			if slices.Contains(got, id) {
				t.Fatalf("不应列出 %s：%v", id, got)
			}
		}
		if num(sec, "handled_count") != 1 || num(sec, "backfill_count") != 1 || num(sec, "new_count") != 4 || sec["state"] != "normal" {
			t.Fatalf("%+v", sec)
		}
		if b["empty"] != false || b["partial"] != false || b["delayed"] != false {
			t.Fatalf("%+v", b)
		}
		// 区内按排序分降序
		var scores []int64
		for _, it := range sec["items"].([]any) {
			scores = append(scores, num(it.(map[string]any), "score"))
		}
		if !slices.IsSortedFunc(scores, func(a, b int64) int { return int(b - a) }) {
			t.Fatalf("%v", scores)
		}
		// 展示级别：pursue 的中标方只有原文链接
		for _, it := range sec["items"].([]any) {
			m := it.(map[string]any)
			if m["signal_id"] == pursue {
				w := m["winner"].(map[string]any)
				if w["level"] != "citation_only" || w["value"] != nil || w["source_url"] != "https://example.com/notice/1" {
					t.Fatalf("%+v", w)
				}
			}
		}
		// 推送：总览卡 + 前 3 条信号卡片；卡片带“相关”“不相关 · 前 5 个原因”“不相关 · 其他”与“查看”
		msgs := wc.Messages(corp1, account[bd])
		if len(msgs) != 4 || msgs[0].Kind != "briefing_overview" {
			t.Fatalf("%d：%+v", len(msgs), msgs)
		}
		bdCards = cards(t, corp1, account[bd], 0)
		if len(bdCards) != 3 {
			t.Fatalf("%d", len(bdCards))
		}
		pc := cardFor(t, bdCards, pursue)
		if len(pc.card.Buttons) != 8 || button(pc.card, "relevant").Token == "" || button(pc.card, "irrelevant:CATEGORY_MISMATCH").Label != "不相关 · 品类不符" ||
			button(pc.card, "irrelevant:other").URL == "" || button(pc.card, "follow").Key != "" {
			t.Fatalf("%+v", pc.card.Buttons)
		}
		text := strings.Join(pc.card.Lines, "\n")
		if strings.Contains(text, "夹具总包甲") || !strings.Contains(text, "见原文 https://example.com/notice/1") || !strings.Contains(text, "北京时间") {
			t.Fatalf("%s", text)
		}
		// 业务专家（参与人，只读）：同一期的卡片只有“查看”
		res = compose(t, expert, end1.Add(2*time.Minute))
		exBriefing1 = res.BriefingID
		exCards = cards(t, corp1, account[expert], 0)
		if len(exCards) != 3 {
			t.Fatalf("%d", len(exCards))
		}
		for _, c := range exCards {
			if len(c.card.Buttons) != 1 || c.card.Buttons[0].Key != "view" {
				t.Fatalf("%+v", c.card.Buttons)
			}
		}
		dmBriefing1 = compose(t, dm, end1.Add(2*time.Minute)).BriefingID
		dmCards = cards(t, corp1, account[dm], 0)
		compose(t, lv, end1.Add(2*time.Minute))
		lvCards = cards(t, corp1, account[lv], 0)
		if len(dmCards) != 3 || len(lvCards) != 3 {
			t.Fatalf("%d %d", len(dmCards), len(lvCards))
		}
		// 投递事件按契约发布
		sent := eventsOf("channels.delivery.sent.v1")
		if len(sent) != 4 || sent[0]["kind"] != "briefing" || num(sent[0], "messages") != 4 {
			t.Fatalf("%+v", sent)
		}
		// 非参与人（无可看目标、无待处理审批）不是受众；管理员参与但未绑定：只在 Web 与站内
		if res := compose(t, bd2, end1.Add(2*time.Minute)); res.Outcome != "skipped" {
			t.Fatalf("%+v", res)
		}
		res = compose(t, admin, end1.Add(2*time.Minute))
		var push, reason string
		if err := chnOwner.QueryRow(ctx, `SELECT push_state, not_pushed_reason FROM chn_briefings WHERE id = $1`, res.BriefingID).Scan(&push, &reason); err != nil ||
			push != "not_pushed" || reason != "not_bound" || res.DeliveryID != "" {
			t.Fatalf("%s %s %v", push, reason, err)
		}
		if audits(t, "channels.briefing.composed", bdBriefing1) != 1 {
			t.Fatal("组装审计")
		}
	})

	run("用例16、17、25、24：卡片“相关”与“不相关 · 品类不符”写入 detection（渠道为企业微信），结果按部分显示并带“撤销”；10 分钟内撤销恢复；10 分钟后撤销过期；再点返回已固定的结果", func(t *testing.T) {
		clock.set(end1.Add(10*time.Minute), 0)
		pc := cardFor(t, bdCards, pursue)
		click(t, corp1, account[bd], pc.card.CardID, button(pc.card, "relevant").Token, chapi.InboundQueued)
		process(t, t1)
		up := lastUpdate(t, corp1, account[bd])
		if up.Card == nil || up.Card.CardID != pc.card.CardID || up.Text != "已标为相关" || button(*up.Card, "undo").Token == "" {
			t.Fatalf("%+v", up)
		}
		if f, _ := signalRow(t, dm, pursue)["status"].(string); f != "open" {
			t.Fatalf("%s", f)
		}
		my := signalRow(t, bd, pursue)["my_feedback"].(map[string]any)
		if my["result"] != "relevant" {
			t.Fatalf("%+v", my)
		}
		relevantOp := tokenID(button(pc.card, "relevant").Token)
		if opsFor(t, relevantOp) != 1 {
			t.Fatal("处理记录")
		}
		var triaged map[string]any
		for _, d := range eventsOf("detection.signal.triaged.v1") {
			if d["operation_id"] == relevantOp {
				triaged = d
			}
		}
		if triaged == nil || triaged["channel"] != "wecom" {
			t.Fatalf("%+v", triaged)
		}
		exec := eventsOf("channels.action.executed.v1")
		if len(exec) != 1 || exec[0]["operation_id"] != relevantOp || exec[0]["result"] != "succeeded" || exec[0]["recipient"] != bd {
			t.Fatalf("%+v", exec)
		}
		// 再点同一按钮：返回已固定的结果并注明执行时间，不再执行
		click(t, corp1, account[bd], pc.card.CardID, button(pc.card, "relevant").Token, chapi.InboundQueued)
		process(t, t1)
		if up := lastUpdate(t, corp1, account[bd]); !strings.HasPrefix(up.Text, "已标为相关（已于 ") || button(*up.Card, "undo").Key != "" {
			t.Fatalf("%+v", up)
		}
		if opsFor(t, relevantOp) != 1 || len(eventsOf("channels.action.executed.v1")) != 1 || audits(t, "channels.action.replayed", relevantOp) != 1 {
			t.Fatal("重复点击不得再次执行")
		}

		// “不相关 · 品类不符”：写入反馈并忽略；10 分钟内撤销
		rc := cardFor(t, bdCards, review)
		click(t, corp1, account[bd], rc.card.CardID, button(rc.card, "irrelevant:CATEGORY_MISMATCH").Token, chapi.InboundQueued)
		process(t, t1)
		up = lastUpdate(t, corp1, account[bd])
		if up.Text != "已标为不相关（品类不符），已移出待处理" || button(*up.Card, "undo").Token == "" {
			t.Fatalf("%+v", up)
		}
		if row := signalRow(t, bd, review); row["status"] != "dismissed" || row["my_feedback"].(map[string]any)["result"] != "irrelevant" {
			t.Fatalf("%+v", row)
		}
		clock.set(end1.Add(15*time.Minute), 0)
		undoTok := button(*up.Card, "undo").Token
		click(t, corp1, account[bd], rc.card.CardID, undoTok, chapi.InboundQueued)
		process(t, t1)
		if up := lastUpdate(t, corp1, account[bd]); up.Text != "已撤销" {
			t.Fatalf("%+v", up)
		}
		if row := signalRow(t, bd, review); row["status"] != "open" || row["my_feedback"] != nil {
			t.Fatalf("%+v", row)
		}
		if exec := eventsOf("channels.action.executed.v1"); exec[len(exec)-1]["action"] != "undo" || exec[len(exec)-1]["result"] != "succeeded" {
			t.Fatalf("%+v", exec)
		}
		// 10 分钟后：撤销已过期，不执行
		click(t, corp1, account[bd], pc.card.CardID, button(pc.card, "irrelevant:TOO_SMALL").Token, chapi.InboundQueued)
		process(t, t1)
		late := button(*lastUpdate(t, corp1, account[bd]).Card, "undo").Token
		if late == "" {
			t.Fatal("没有撤销")
		}
		clock.set(end1.Add(26*time.Minute), 0)
		click(t, corp1, account[bd], pc.card.CardID, late, chapi.InboundQueued)
		process(t, t1)
		if up := lastUpdate(t, corp1, account[bd]); up.Text != "卡片已过期，请在 Web 中处理" || button(*up.Card, "view").URL == "" {
			t.Fatalf("%+v", up)
		}
		if row := signalRow(t, bd, pursue); row["status"] != "dismissed" {
			t.Fatalf("%+v", row)
		}
		if st, _ := tokenState(t, late); st != "open" || audits(t, "channels.action.rejected", tokenID(late)) != 1 {
			t.Fatalf("过期的点击不固定结果：%s", st)
		}
	})

	run("越权与反例测试 用例29、45、46、21：转发的卡片、跨租户回调、篡改与伪造的令牌、卡片不符、回调重放都在执行前被拒绝，不消耗令牌；随后收件人本人点击成功", func(t *testing.T) {
		clock.set(end1.Add(30*time.Minute), 0)
		wc_ := cardFor(t, bdCards, watch)
		tok := button(wc_.card, "relevant").Token
		bd2Before := len(wc.Messages(corp1, account[bd2]))
		// 转发：同租户的 bd2 点击 bd 的卡片
		click(t, corp1, account[bd2], wc_.card.CardID, tok, chapi.InboundQueued)
		process(t, t1)
		msgs := wc.Messages(corp1, account[bd2])
		if len(msgs) != bd2Before+1 || msgs[len(msgs)-1].Text != "此卡片发给其他成员，不能由你操作" || msgs[len(msgs)-1].Card != nil {
			t.Fatalf("%+v", msgs[bd2Before:])
		}
		for _, m := range msgs[bd2Before:] {
			raw, _ := json.Marshal(m)
			if strings.Contains(string(raw), watch) || strings.Contains(string(raw), "夹具") {
				t.Fatalf("泄露信号内容：%s", raw)
			}
		}
		// 跨租户：t2 企业的回调携带 t1 成员的令牌
		click(t, corp2, account[admin2], wc_.card.CardID, tok, chapi.InboundRejected)
		// 篡改（改动签名中的一个字符）、伪造（自签载荷）、卡片不符
		raw, _ := base64.RawURLEncoding.DecodeString(tok)
		raw[len(raw)-1] ^= 0x01
		click(t, corp1, account[bd], wc_.card.CardID, base64.RawURLEncoding.EncodeToString(raw), chapi.InboundRejected)
		payload, _ := json.Marshal(map[string]any{"i": tokenID(tok), "t": t1, "r": bd2, "s": watch, "a": "relevant", "v": 0, "k": wc_.card.CardID, "e": clock.Now().Add(time.Hour)})
		click(t, corp1, account[bd2], wc_.card.CardID, base64.RawURLEncoding.EncodeToString(append(payload, make([]byte, 32)...)), chapi.InboundRejected)
		click(t, corp1, account[bd], cardFor(t, bdCards, pursue).card.CardID, tok, chapi.InboundRejected)
		// 签名不符的回调
		if _, err := callbacks.ReceiveInbound(gw, "wecom", &chapi.Callback{CorpID: corp1, EventID: "forged-" + suffix, Timestamp: clock.Now(), Nonce: "n",
			Body: wecomtest.ActionBody(account[bd], wc_.card.CardID, tok), Signature: "00"}); err == nil {
			t.Fatal("验签失败的回调被接受")
		}
		process(t, t1)
		process(t, t2)
		if st, n := tokenState(t, tok); st != "open" || n != 0 {
			t.Fatalf("令牌被消耗：%s %d", st, n)
		}
		if opsFor(t, tokenID(tok)) != 0 || audits(t, "channels.action.rejected", tokenID(tok)) != 1 {
			t.Fatalf("拒绝审计：%d", audits(t, "channels.action.rejected", tokenID(tok)))
		}
		if n := count(t, chnOwner, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1' AND envelope->'data'->>'action' = 'channels.action.rejected'
 AND envelope->'data'->>'basis' IN ('wecom;cross_tenant', 'wecom;token_invalid', 'wecom;card_mismatch')`); n != 4 {
			t.Fatalf("回调拒绝审计 %d", n)
		}
		// 收件人本人点击成功；同一回调重放三次不再执行、计入渠道健康
		before := count(t, chnOwner, `SELECT rejected_callbacks FROM chn_corp_routes WHERE corp_id = $1`, corp1)
		ev := click(t, corp1, account[bd], wc_.card.CardID, tok, chapi.InboundQueued)
		for range 3 {
			if res, err := callback(corp1, ev, wecomtest.ActionBody(account[bd], wc_.card.CardID, tok)); err != nil || res.Outcome != chapi.InboundDuplicate {
				t.Fatalf("%+v %v", res, err)
			}
		}
		process(t, t1)
		if lastUpdate(t, corp1, account[bd]).Text != "已标为相关" || opsFor(t, tokenID(tok)) != 1 {
			t.Fatal("收件人点击未成功")
		}
		if after := count(t, chnOwner, `SELECT rejected_callbacks FROM chn_corp_routes WHERE corp_id = $1`, corp1); after != before+3 {
			t.Fatalf("%d → %d", before, after)
		}
		// 之后 bd2 再点仍被拒绝，看不到 bd 的结果
		click(t, corp1, account[bd2], wc_.card.CardID, tok, chapi.InboundQueued)
		process(t, t1)
		if m := wc.Messages(corp1, account[bd2]); m[len(m)-1].Text != "此卡片发给其他成员，不能由你操作" {
			t.Fatalf("%+v", m[len(m)-1])
		}
		if audits(t, "channels.action.rejected", tokenID(tok)) != 2 {
			t.Fatal("非收件人的拒绝审计")
		}
	})

	run("用例15（反例）：只读参与人的点击即使带有效令牌形状也不能执行；直接调用 detection 处理被拒绝", func(t *testing.T) {
		ec := exCards[0]
		// 业务专家的卡片没有动作令牌：拿 bd 的令牌点击视为转发
		click(t, corp1, account[expert], ec.card.CardID, button(cardFor(t, bdCards, review).card, "relevant").Token, chapi.InboundRejected)
		r := signalRow(t, expert, ec.signal)
		req := &detectionapi.TriageRequest{OperationID: "op-ex-" + suffix, Action: detapi.TriageRequestActionFeedback, SeenContentVersion: num(r, "content_version"),
			StateVersion: num(r, "state_version"), FeedbackVersion: num(r, "feedback_version"),
			Feedback: detapi.NewOptFeedbackInput(detapi.FeedbackInput{Result: detapi.FeedbackInputResultRelevant, Reasons: []string{}})}
		uctx := reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01", Principal: reqctx.Principal{ID: expert, Kind: "user"}})
		var de *detectionapi.Error
		if _, err := remoteBriefing.Triage(uctx, ec.signal, "k-ex-"+suffix, req); !errors.As(err, &de) || de.Code != "forbidden" {
			t.Fatalf("%v", err)
		}
	})

	run("用例22、23：暂时失败自动以新幂等键、同一操作重试，原幂等键重发仍返回那次失败；已提交而响应丢失时新尝试返回已提交的结果，只有一条记录", func(t *testing.T) {
		clock.set(end1.Add(40*time.Minute), 0)
		c1 := cardFor(t, dmCards, review)
		tok := button(c1.card, "relevant").Token
		// 第一次访问判断（读取卡片当前版本）放过，第二次（detection 执行第一次尝试）失败
		access.arm(1, 1)
		click(t, corp1, account[dm], c1.card.CardID, tok, chapi.InboundQueued)
		process(t, t1)
		access.arm(0, 0)
		if up := lastUpdate(t, corp1, account[dm]); up.Text != "已标为相关" {
			t.Fatalf("%+v", up)
		}
		op := tokenID(tok)
		if st, n := tokenState(t, tok); st != "settled" || n != 2 || opsFor(t, op) != 1 {
			t.Fatalf("%s %d %d", st, n, opsFor(t, op))
		}
		var status int
		if err := dets.owner.QueryRow(ctx, `SELECT status FROM det_triage_attempts WHERE idem_key = $1`, op+"-1").Scan(&status); err != nil || status != 503 {
			t.Fatalf("%d %v", status, err)
		}
		var fixed []byte
		if err := chnOwner.QueryRow(ctx, `SELECT request FROM chn_action_tokens WHERE id = $1`, op).Scan(&fixed); err != nil {
			t.Fatal(err)
		}
		var req detectionapi.TriageRequest
		decode(t, fixed, &req)
		uctx := reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01", Principal: reqctx.Principal{ID: dm, Kind: "user"}})
		if _, err := remoteBriefing.Triage(uctx, review, op+"-1", &req); !errors.Is(err, binding.ErrTransient) || !strings.Contains(err.Error(), "customer_context_unavailable") {
			t.Fatalf("原幂等键重发：%v", err)
		}
		if res, err := remoteBriefing.Triage(uctx, review, op+"-2", &req); err != nil || res.OperationID != op {
			t.Fatalf("%+v %v", res, err)
		}

		// 已提交而响应丢失
		c2 := cardFor(t, dmCards, watch)
		tok2 := button(c2.card, "relevant").Token
		flaky.loseTriage.Store(1)
		click(t, corp1, account[dm], c2.card.CardID, tok2, chapi.InboundQueued)
		process(t, t1)
		if up := lastUpdate(t, corp1, account[dm]); up.Text != "已标为相关" {
			t.Fatalf("%+v", up)
		}
		if st, n := tokenState(t, tok2); st != "settled" || n != 2 || opsFor(t, tokenID(tok2)) != 1 {
			t.Fatalf("%s %d", st, n)
		}
	})

	run("用例20：令牌所见内容版本不是当前版本时整个动作 SIGNAL_UPDATED 不执行，附新卡片，在新卡片上再点成功", func(t *testing.T) {
		clock.set(end1.Add(45*time.Minute), 0)
		c := cardFor(t, dmCards, pursue)
		if _, err := dets.owner.Exec(ctx, `UPDATE det_signals SET content_version = content_version + 1, material_updated_at = now() WHERE id = $1`, pursue); err != nil {
			t.Fatal(err)
		}
		if _, err := dets.owner.Exec(ctx, `INSERT INTO det_signal_updates (tenant_id, signal_id, content_version, trigger, changed_fields, disposition_before, disposition_after, material, recorded_at)
 VALUES ($1, $2, 1, 'tendering.result.corrected', '{amount}', 'pursue_candidate', 'pursue_candidate', true, $3)`, t1, pursue, end1.Add(44*time.Minute)); err != nil {
			t.Fatal(err)
		}
		from := len(wc.Messages(corp1, account[dm]))
		tok := button(c.card, "relevant").Token
		click(t, corp1, account[dm], c.card.CardID, tok, chapi.InboundQueued)
		process(t, t1)
		if up := lastUpdate(t, corp1, account[dm]); up.Text != "这条信号已更新，请查看新卡片后再操作" {
			t.Fatalf("%+v", up)
		}
		if n := count(t, dets.owner, `SELECT count(*) FROM det_triage_ops WHERE operation_id = $1 AND outcome <> 'failed'`, tokenID(tok)); n != 0 {
			t.Fatal("SIGNAL_UPDATED 不得写入")
		}
		fresh := cards(t, corp1, account[dm], from)
		if len(fresh) != 1 || fresh[0].signal != pursue || !strings.Contains(fresh[0].msg.Text, "内容版本 0 → 1") {
			t.Fatalf("%+v", fresh)
		}
		click(t, corp1, account[dm], fresh[0].card.CardID, button(fresh[0].card, "relevant").Token, chapi.InboundQueued)
		process(t, t1)
		if up := lastUpdate(t, corp1, account[dm]); up.Text != "已标为相关" {
			t.Fatalf("%+v", up)
		}
	})

	run("用例19：信号已关闭时“不相关”只写入反馈，处理状态不适用", func(t *testing.T) {
		clock.set(end1.Add(50*time.Minute), 0)
		if _, err := dets.owner.Exec(ctx, `UPDATE det_signals SET status = 'closed', state_version = state_version + 1 WHERE id = $1`, watch); err != nil {
			t.Fatal(err)
		}
		c := cardFor(t, lvCards, watch)
		click(t, corp1, account[lv], c.card.CardID, button(c.card, "irrelevant:TOO_SMALL").Token, chapi.InboundQueued)
		process(t, t1)
		up := lastUpdate(t, corp1, account[lv])
		if up.Text != "已记录你的反馈；该信号已关闭，处理状态不适用" || !slices.Contains(up.Card.Lines, "当前：已关闭") {
			t.Fatalf("%+v", up)
		}
	})

	run("用例39：完整简报的条目为组装时快照，处理状态按当前叠加并注明处理人", func(t *testing.T) {
		clock.set(end1.Add(55*time.Minute), 0)
		b := getBriefing(t, expert, exBriefing1)
		if b["status_as_of"] == nil {
			t.Fatalf("%+v", b)
		}
		for _, it := range objectiveSection(t, b)["items"].([]any) {
			m := it.(map[string]any)
			cur := m["current"].(map[string]any)
			switch m["signal_id"] {
			case pursue:
				// bd 在卡片上以“规模太小”忽略；dm 的卡片之后被更新（内容版本 0 → 1）
				if cur["status"] != "dismissed" || cur["handled_by"] != bd || cur["content_updated"] != true || num(m, "content_version") != 0 {
					t.Fatalf("%+v", m)
				}
			case watch:
				if cur["status"] != "closed" {
					t.Fatalf("%+v", m)
				}
			}
		}
	})

	run("越权与反例测试 用例30、40：简报只属本人；他人、其他租户与不存在同为 404；游标签名绑定本人与租户；投递统计只给管理员且不含条目与收件人", func(t *testing.T) {
		for _, path := range []string{"/v1/channels/briefings/" + bdBriefing1, "/v1/channels/briefings/" + ids.New("brf")} {
			expect(t, dm, false, "GET", path, "", 404, "briefing_not_found")
			expect(t, admin, false, "GET", path, "", 404, "briefing_not_found")
			expectAs(t, admin2, t2, false, "GET", path, "", "", 404, "briefing_not_found")
			expect(t, dm, false, "POST", path+"/resend", "", 404, "briefing_not_found")
			expectAs(t, admin2, t2, false, "POST", path+"/resend", "", "", 404, "briefing_not_found")
		}
		list := obj(t, expect(t, dm, false, "GET", "/v1/channels/briefings", "", 200))
		for _, b := range list["briefings"].([]any) {
			if b.(map[string]any)["id"] != dmBriefing1 {
				t.Fatalf("%+v", b)
			}
		}
		if l := obj(t, expectAs(t, admin2, t2, false, "GET", "/v1/channels/briefings", "", "", 200)); len(l["briefings"].([]any)) != 0 {
			t.Fatalf("%+v", l)
		}
		// 游标：再组装一期使 bd 有两期，limit=1 得到游标
		compose(t, bd, end2.Add(time.Minute))
		page := obj(t, expect(t, bd, false, "GET", "/v1/channels/briefings?limit=1", "", 200))
		cursor, _ := page["next_cursor"].(string)
		if cursor == "" || len(page["briefings"].([]any)) != 1 {
			t.Fatalf("%+v", page)
		}
		next := obj(t, expect(t, bd, false, "GET", "/v1/channels/briefings?limit=1&cursor="+url.QueryEscape(cursor), "", 200))
		if bs := next["briefings"].([]any); len(bs) != 1 || bs[0].(map[string]any)["id"] != bdBriefing1 {
			t.Fatalf("%+v", next)
		}
		raw, _ := base64.RawURLEncoding.DecodeString(cursor)
		raw[0] ^= 0x01
		tampered := base64.RawURLEncoding.EncodeToString(raw)
		forged, _ := json.Marshal(map[string]any{"t": t1, "u": dm, "e": end5, "i": ids.New("brf")})
		for _, c := range []string{tampered, base64.RawURLEncoding.EncodeToString(append(forged, make([]byte, 32)...)), "not-a-cursor"} {
			expect(t, bd, false, "GET", "/v1/channels/briefings?cursor="+url.QueryEscape(c), "", 400, "invalid_cursor")
		}
		// 签发给 bd 的游标不能被他人或其他租户使用
		expect(t, dm, false, "GET", "/v1/channels/briefings?cursor="+url.QueryEscape(cursor), "", 400, "invalid_cursor")
		expectAs(t, admin2, t2, false, "GET", "/v1/channels/briefings?cursor="+url.QueryEscape(cursor), "", "", 400, "invalid_cursor")
		// 投递统计
		expect(t, bd, false, "GET", "/v1/channels/delivery-stats", "", 403, "forbidden")
		expect(t, expert, false, "GET", "/v1/channels/delivery-stats", "", 403, "forbidden")
		stats := expect(t, admin, false, "GET", "/v1/channels/delivery-stats?since="+url.QueryEscape(start1.Format(time.RFC3339)), "", 200)
		if s := string(stats); strings.Contains(s, bd) || strings.Contains(s, pursue) || strings.Contains(s, "夹具") || !strings.Contains(s, `"delivered"`) {
			t.Fatalf("%s", stats)
		}
		if s := obj(t, expectAs(t, admin2, t2, false, "GET", "/v1/channels/delivery-stats?since="+url.QueryEscape(start1.Format(time.RFC3339)), "", "", 200)); len(s["deliveries"].([]any)) != 0 {
			t.Fatalf("其他租户的统计：%+v", s)
		}
	})

	run("越权与反例测试 用例37：链接只指向对象；他人打开为账号不符；其他租户、伪造与不存在同为 404；7 天后过期", func(t *testing.T) {
		clock.set(end1.Add(time.Hour), 0)
		view := button(cardFor(t, bdCards, review).card, "view").URL
		tok, _, _ := linkTarget(t, view)
		body := fmt.Sprintf(`{"token":%q}`, tok)
		if r := obj(t, expect(t, bd, false, "POST", "/v1/channels/briefing-links:open", body, 200)); r["kind"] != "signal" || r["id"] != review || r["from_signal"] != true {
			t.Fatalf("%+v", r)
		}
		overview := wc.Messages(corp1, account[bd])[0]
		btok, _, _ := linkTarget(t, button(*overview.Card, "open").URL)
		if r := obj(t, expect(t, bd, false, "POST", "/v1/channels/briefing-links:open", fmt.Sprintf(`{"token":%q}`, btok), 200)); r["kind"] != "briefing" || r["id"] != bdBriefing1 {
			t.Fatalf("%+v", r)
		}
		expect(t, dm, false, "POST", "/v1/channels/briefing-links:open", body, 409, "account_mismatch")
		expectAs(t, admin2, t2, false, "POST", "/v1/channels/briefing-links:open", body, "", 404, "content_unavailable")
		fake := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
		expect(t, bd, false, "POST", "/v1/channels/briefing-links:open", fmt.Sprintf(`{"token":%q}`, fake), 404, "content_unavailable")
		clock.set(end1.Add(7*24*time.Hour+3*time.Minute), 0)
		expect(t, bd, false, "POST", "/v1/channels/briefing-links:open", body, 410, "card_link_expired")
		clock.set(end1.Add(time.Hour), 0)
	})

	run("用例31、33：推送失败退避重试只送达一次；渠道超时后的重试使同一张卡片送达两次，两张卡片的令牌相同，同一按钮只执行一次", func(t *testing.T) {
		newSignal(t, at(20, 20, 0), facttest.Bid{Amount: 5_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}})
		from := len(wc.Messages(corp1, account[dm]))
		// 总览卡正常，第一张信号卡片已送达但确认丢失
		wc.LoseAcksAfter(1, 1)
		res := compose(t, dm, end2.Add(time.Minute))
		var state string
		var attempts int
		if err := chnOwner.QueryRow(ctx, `SELECT state, attempts FROM chn_deliveries WHERE id = $1`, res.DeliveryID).Scan(&state, &attempts); err != nil || state != "retrying" || attempts != 1 {
			t.Fatalf("%s %d %v", state, attempts, err)
		}
		if f := eventsOf("channels.delivery.failed.v1"); len(f) == 0 || f[len(f)-1]["final"] != false || f[len(f)-1]["reason"] != "channel_error" {
			t.Fatalf("%+v", f)
		}
		deliverDue(t, end2.Add(5*time.Minute))
		if err := chnOwner.QueryRow(ctx, `SELECT state, attempts FROM chn_deliveries WHERE id = $1`, res.DeliveryID).Scan(&state, &attempts); err != nil || state != "delivered" || attempts != 2 {
			t.Fatalf("%s %d %v", state, attempts, err)
		}
		msgs := wc.Messages(corp1, account[dm])[from:]
		overviews := 0
		for _, m := range msgs {
			if m.Kind == "briefing_overview" {
				overviews++
			}
		}
		got := cards(t, corp1, account[dm], from)
		if overviews != 1 || len(got) < 2 || got[0].card.CardID != got[1].card.CardID {
			t.Fatalf("%d %+v", overviews, got)
		}
		first, second := got[0], got[1]
		if b1, b2 := button(first.card, "relevant").Token, button(second.card, "relevant").Token; b1 == "" || b1 != b2 {
			t.Fatal("重复送达的卡片令牌必须相同")
		}
		tok := button(second.card, "irrelevant:TOO_SMALL").Token
		clock.set(end2.Add(10*time.Minute), 0)
		click(t, corp1, account[dm], second.card.CardID, tok, chapi.InboundQueued)
		process(t, t1)
		click(t, corp1, account[dm], first.card.CardID, tok, chapi.InboundQueued)
		process(t, t1)
		if opsFor(t, tokenID(tok)) != 1 || !strings.Contains(lastUpdate(t, corp1, account[dm]).Text, "（已于 ") {
			t.Fatal("同一按钮只执行一次")
		}
	})

	run("用例43：卡片上成功、随后在 Web 撤销，再点重复卡片返回原结果与执行时间并显示当前值，不再次执行", func(t *testing.T) {
		clock.set(end2.Add(12*time.Minute), 0)
		var tokRow struct{ id, signal string }
		if err := chnOwner.QueryRow(ctx, `SELECT id, signal_id FROM chn_action_tokens WHERE recipient = $1 AND action = 'irrelevant' AND state = 'settled' ORDER BY executed_at DESC LIMIT 1`, dm).
			Scan(&tokRow.id, &tokRow.signal); err != nil {
			t.Fatal(err)
		}
		expect(t, dm, false, "POST", "/v1/detection/triage-undos", fmt.Sprintf(`{"operation_id":"op-undo-%s","target_operation_id":%q}`, suffix, tokRow.id), 200)
		pump(t)
		var card string
		if err := chnOwner.QueryRow(ctx, `SELECT card_id FROM chn_action_tokens WHERE id = $1`, tokRow.id).Scan(&card); err != nil {
			t.Fatal(err)
		}
		var sealed string
		for _, c := range cards(t, corp1, account[dm], 0) {
			if c.card.CardID == card {
				sealed = button(c.card, "irrelevant:TOO_SMALL").Token
			}
		}
		click(t, corp1, account[dm], card, sealed, chapi.InboundQueued)
		process(t, t1)
		up := lastUpdate(t, corp1, account[dm])
		if !strings.HasPrefix(up.Text, "已标为不相关（规模太小），已移出待处理（已于 ") || !slices.Contains(up.Card.Lines, "当前：待处理") {
			t.Fatalf("%+v", up)
		}
		if opsFor(t, tokRow.id) != 1 {
			t.Fatal("不得再次执行")
		}
	})

	run("用例27、31：推送后被移出目标：点击卡片得到无权限且不显示信号；重试推送前失去访问的区被删除；下一期不再含该目标区；完整简报该区无权限", func(t *testing.T) {
		clock.set(end2.Add(20*time.Minute), 0)
		// bd 的 P3 推送失败、待重试
		newSignal(t, at(21, 20, 0), facttest.Bid{Amount: 5_500_000, HistoryCategories: []string{"CAT-PIPE-PE"}})
		from := len(wc.Messages(corp1, account[bd]))
		wc.FailNextSends(1)
		res := compose(t, bd, end3.Add(time.Minute))
		setParticipants(t, lv, admin, expert)
		deliverDue(t, end3.Add(5*time.Minute))
		var state string
		var dropped int
		if err := chnOwner.QueryRow(ctx, `SELECT state, dropped_sections FROM chn_deliveries WHERE id = $1`, res.DeliveryID).Scan(&state, &dropped); err != nil || state != "delivered" || dropped != 1 {
			t.Fatalf("%s %d %v", state, dropped, err)
		}
		msgs := wc.Messages(corp1, account[bd])[from:]
		if len(msgs) != 1 || msgs[0].Kind != "briefing_overview" || !slices.Contains(msgs[0].Card.Lines, "今日没有可显示的目标区") {
			t.Fatalf("%+v", msgs)
		}
		// P1 卡片上的未用按钮（令牌仍在有效期内）：无权限，不显示信号
		clock.set(end1.Add(2*time.Hour), 0)
		c := cardFor(t, bdCards, review)
		click(t, corp1, account[bd], c.card.CardID, button(c.card, "irrelevant:OUT_OF_REGION").Token, chapi.InboundQueued)
		process(t, t1)
		up := lastUpdate(t, corp1, account[bd])
		if up.Text != "你已没有该目标的反馈权限" || len(up.Card.Lines) != 0 {
			t.Fatalf("%+v", up)
		}
		if exec := eventsOf("channels.action.executed.v1"); exec[len(exec)-1]["code"] != "forbidden" {
			t.Fatalf("%+v", exec[len(exec)-1])
		}
		// 链接同样不可用
		tok, _, _ := linkTarget(t, button(c.card, "view").URL)
		expect(t, bd, false, "POST", "/v1/channels/briefing-links:open", fmt.Sprintf(`{"token":%q}`, tok), 404, "content_unavailable")
		// 完整简报：该区只剩种类、顺序与 no_permission
		sec := sectionsOf(getBriefing(t, bd, bdBriefing1))
		for _, s := range sec {
			if s["kind"] == "objective" && (s["state"] != "no_permission" || len(s["items"].([]any)) != 0 || s["objective_id"] != nil || s["title"] != nil) {
				t.Fatalf("%+v", s)
			}
		}
		// 下一期：不再是受众
		if r := compose(t, bd, end4.Add(time.Minute)); r.Outcome != "skipped" {
			t.Fatalf("%+v", r)
		}
	})

	run("用例28：成员退出（并重新加入）后，排队中的推送在发送前取消；点击旧卡片只收到绑定引导，不执行", func(t *testing.T) {
		clock.set(end3.Add(20*time.Minute), 0)
		wc.FailNextSends(1)
		res := compose(t, lv, end3.Add(time.Minute))
		putMember := func(state, roles string) {
			var m struct {
				MemberVersion int64 `json:"member_version"`
			}
			decode(t, expect(t, admin, true, "GET", "/v1/identity/members/"+lv, "", 200), &m)
			expect(t, admin, true, "PUT", "/v1/identity/members/"+lv, fmt.Sprintf(`{"state":%q,"roles":%s,"expected_version":%d}`, state, roles, m.MemberVersion), 200)
			pump(t)
		}
		putMember("removed", `[]`)
		putMember("active", `["business_developer"]`)
		deliverDue(t, end3.Add(10*time.Minute))
		var state, reason string
		if err := chnOwner.QueryRow(ctx, `SELECT state, reason FROM chn_deliveries WHERE id = $1`, res.DeliveryID).Scan(&state, &reason); err != nil || state != "cancelled" ||
			(reason != "binding_inactive" && reason != "member_inactive") {
			t.Fatalf("%s %s %v", state, reason, err)
		}
		if f := eventsOf("channels.delivery.failed.v1"); f[len(f)-1]["final"] != true {
			t.Fatalf("%+v", f[len(f)-1])
		}
		from := len(wc.Messages(corp1, account[lv]))
		c := cardFor(t, lvCards, review)
		tok := button(c.card, "relevant").Token
		click(t, corp1, account[lv], c.card.CardID, tok, chapi.InboundQueued)
		process(t, t1)
		msgs := wc.Messages(corp1, account[lv])[from:]
		if len(msgs) != 1 || msgs[0].Kind != "binding_guide" {
			t.Fatalf("%+v", msgs)
		}
		if st, _ := tokenState(t, tok); st != "open" || opsFor(t, tokenID(tok)) != 0 {
			t.Fatal("退出成员的点击不得执行")
		}
	})

	run("用例32、44：推送截止后停止重试并留在站内；本人重新推送产生新的投递与新令牌，不重新组装；重放返回同一投递；非当日与未绑定拒绝", func(t *testing.T) {
		clock.set(end3.Add(30*time.Minute), 0)
		wc.SetSendFailure(true)
		res := compose(t, expert, end3.Add(time.Minute))
		deliverDue(t, at(22, 18, 0))
		wc.SetSendFailure(false)
		var state, reason, push string
		if err := chnOwner.QueryRow(ctx, `SELECT d.state, d.reason, b.push_state FROM chn_deliveries d JOIN chn_briefings b ON b.id = d.briefing_id WHERE d.id = $1`, res.DeliveryID).
			Scan(&state, &reason, &push); err != nil || state != "in_app" || reason != "cutoff" || push != "in_app" {
			t.Fatalf("%s %s %s %v", state, reason, push, err)
		}
		before := count(t, chnOwner, `SELECT count(*) FROM chn_briefings WHERE subject = $1`, expert)
		key := "resend-" + suffix + "-0001"
		var d1, d2 map[string]any
		d1 = obj(t, expectAs(t, expert, t1, false, "POST", "/v1/channels/briefings/"+res.BriefingID+"/resend", "", key, 200))
		d2 = obj(t, expectAs(t, expert, t1, false, "POST", "/v1/channels/briefings/"+res.BriefingID+"/resend", "", key, 200))
		if d1["id"] != d2["id"] || d1["kind"] != "resend" || count(t, chnOwner, `SELECT count(*) FROM chn_briefings WHERE subject = $1`, expert) != before {
			t.Fatalf("%+v %+v", d1, d2)
		}
		if n := count(t, chnOwner, `SELECT count(*) FROM chn_deliveries WHERE briefing_id = $1 AND kind = 'resend' AND state = 'delivered'`, res.BriefingID); n != 1 {
			t.Fatalf("%d", n)
		}
		// 新卡片的链接与旧投递不同
		var oldMsgs, newMsgs string
		if err := chnOwner.QueryRow(ctx, `SELECT d1.messages::text, d2.messages::text FROM chn_deliveries d1, chn_deliveries d2 WHERE d1.id = $1 AND d2.id = $2`, res.DeliveryID, d1["id"]).
			Scan(&oldMsgs, &newMsgs); err != nil || oldMsgs == newMsgs {
			t.Fatalf("%v", err)
		}
		expect(t, expert, false, "POST", "/v1/channels/briefings/"+exBriefing1+"/resend", "", 409, "resend_not_allowed")
		admBriefing := ""
		if err := chnOwner.QueryRow(ctx, `SELECT id FROM chn_briefings WHERE subject = $1 ORDER BY period_end DESC LIMIT 1`, admin).Scan(&admBriefing); err != nil {
			t.Fatal(err)
		}
		clock.set(end1.Add(time.Hour), 0)
		expect(t, admin, false, "POST", "/v1/channels/briefings/"+admBriefing+"/resend", "", 409, "not_bound")
		if audits(t, "channels.briefing.resent", d1["id"].(string)) != 1 {
			t.Fatal("重新推送审计")
		}
		if b := getBriefing(t, expert, res.BriefingID); len(b["deliveries"].([]any)) != 2 {
			t.Fatalf("%+v", b["deliveries"])
		}
	})

	run("用例34、8、49、7：组装延迟标“延迟发送”；12:00 后不推送且窗口不前移；一区取数失败标暂不可用、窗口顺延，下一期标“含上一期”且不遗漏不重复；来源中断显示截至时间", func(t *testing.T) {
		// P4（[10-22, 10-23)）的一条信号
		p4sig := newSignal(t, at(22, 20, 0), facttest.Bid{Amount: 7_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}})
		// 专家：P4 在 12:30 才组装 → 不推送，窗口不前移
		res := compose(t, expert, at(23, 12, 30))
		if res.Outcome != "late" {
			t.Fatalf("%+v", res)
		}
		var push, reason string
		if err := chnOwner.QueryRow(ctx, `SELECT push_state, not_pushed_reason FROM chn_briefings WHERE id = $1`, res.BriefingID).Scan(&push, &reason); err != nil || push != "not_pushed" || reason != "late" {
			t.Fatalf("%s %s", push, reason)
		}
		// P5：一区取数失败
		flaky.failSignals.Store(true)
		res = compose(t, expert, end5.Add(time.Minute))
		flaky.failSignals.Store(false)
		b := getBriefing(t, expert, res.BriefingID)
		if sec := objectiveSection(t, b); sec["state"] != "unavailable" || b["partial"] != true || b["empty"] != false {
			t.Fatalf("%+v", b)
		}
		var fetched time.Time
		if err := chnOwner.QueryRow(ctx, `SELECT fetched_to FROM chn_briefing_windows WHERE subject = $1`, expert).Scan(&fetched); err != nil || !fetched.Equal(end3) {
			t.Fatalf("窗口不得前移：%v %v", fetched, err)
		}
		// P6（下周二）组装延迟 20 分钟：标“延迟发送”；窗口从上一次成功取数的终点（P3 的终点）起，含上一期
		end6 := at(27, 7, 30)
		from := len(wc.Messages(corp1, account[expert]))
		res = compose(t, expert, end6.Add(20*time.Minute))
		b = getBriefing(t, expert, res.BriefingID)
		sec := objectiveSection(t, b)
		ws, _ := time.Parse(time.RFC3339, sec["window_start"].(string))
		if b["delayed"] != true || sec["includes_previous"] != true || !ws.Equal(end3) {
			t.Fatalf("%+v", b)
		}
		ids := itemIDs(sec)
		n := 0
		for _, id := range ids {
			if id == p4sig {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("P4 的信号应恰好出现一次：%v", ids)
		}
		ov := wc.Messages(corp1, account[expert])[from]
		if ov.Kind != "briefing_overview" || !strings.Contains(ov.Card.Title, "延迟发送，原定 07:30") || !slices.Contains(ov.Card.Lines, "  含上一期未能生成的部分") {
			t.Fatalf("%+v", ov.Card)
		}
		// 来源中断（P7）：区显示中断领域与截至时间，简报不算空且为部分
		coverage.mu.Lock()
		coverage.interrupted["INF-PROC"] = at(27, 3, 0)
		coverage.mu.Unlock()
		res = compose(t, expert, at(28, 7, 31))
		coverage.mu.Lock()
		delete(coverage.interrupted, "INF-PROC")
		coverage.mu.Unlock()
		b = getBriefing(t, expert, res.BriefingID)
		sec = objectiveSection(t, b)
		if sec["state"] != "source_interrupted" || b["partial"] != true || b["empty"] != false {
			t.Fatalf("%+v", b)
		}
		interrupted := false
		for _, d := range sec["data_as_of"].([]any) {
			m := d.(map[string]any)
			if m["domain"] == "INF-PROC" && m["state"] == "interrupted" && m["as_of"] != nil {
				interrupted = true
			}
		}
		if !interrupted {
			t.Fatalf("%+v", sec["data_as_of"])
		}
	})

	run("用例6、3：空简报保存并写明原因、默认不推送；偏好修改从下一期生效、版本冲突与非法值被拒绝；开启后推送空简报总览卡", func(t *testing.T) {
		clock.set(at(28, 9, 0), 0)
		prefs := obj(t, expect(t, expert, false, "GET", "/v1/channels/briefing-preferences", "", 200))
		if num(prefs, "version") != 0 || prefs["send_time"] != "07:30" || prefs["timezone"] != "Asia/Shanghai" || prefs["push_empty"] != false {
			t.Fatalf("%+v", prefs)
		}
		body := func(version int64, sendTime string, pushEmpty bool) string {
			return fmt.Sprintf(`{"expected_version":%d,"send_time":%q,"timezone":"Asia/Shanghai","send_days":[1,2,3,4,5],"quiet_start":"21:00","quiet_end":"07:00","push_empty":%t,"section_limits":[]}`,
				version, sendTime, pushEmpty)
		}
		expect(t, expert, false, "PUT", "/v1/channels/briefing-preferences", body(0, "08:15", true), 422, "invalid_preferences")
		expect(t, expert, false, "PUT", "/v1/channels/briefing-preferences", body(3, "07:30", true), 409, "version_conflict")
		updated := obj(t, expect(t, expert, false, "PUT", "/v1/channels/briefing-preferences", body(0, "07:30", true), 200))
		eff, _ := time.Parse(time.RFC3339, updated["effective_from"].(string))
		if num(updated, "version") != 1 || !eff.Equal(at(29, 7, 30)) {
			t.Fatalf("%+v", updated)
		}
		if audits(t, "channels.briefing_preferences.updated", "") != 1 {
			t.Fatal("偏好审计")
		}
		// P8（10-29 07:30）：仍用旧版本（不推送空简报）
		res := compose(t, expert, at(29, 7, 31))
		var empty bool
		var push, reason string
		var version int64
		if err := chnOwner.QueryRow(ctx, `SELECT empty, push_state, coalesce(not_pushed_reason, ''), preference_version FROM chn_briefings WHERE id = $1`, res.BriefingID).
			Scan(&empty, &push, &reason, &version); err != nil || !empty || push != "not_pushed" || reason != "empty" || version != 0 {
			t.Fatalf("%v %s %s %d %v", empty, push, reason, version, err)
		}
		if sec := objectiveSection(t, getBriefing(t, expert, res.BriefingID)); sec["state"] != "empty" {
			t.Fatalf("%+v", sec)
		}
		// P9：新版本生效，推送空简报总览卡
		from := len(wc.Messages(corp1, account[expert]))
		res = compose(t, expert, at(30, 7, 31))
		msgs := wc.Messages(corp1, account[expert])[from:]
		if res.DeliveryID == "" || len(msgs) != 1 || msgs[0].Kind != "briefing_overview" {
			t.Fatalf("%+v %+v", res, msgs)
		}
	})

	run("用例36（服务端部分）：待本人批准的审批进入“待我处理”区并排在首位，经 identity 内部端口以本人权限读取", func(t *testing.T) {
		approval := requestActivation(t, "再次激活")
		res := compose(t, dm2, at(30, 7, 31))
		if res.Outcome != "composed" {
			t.Fatalf("%+v", res)
		}
		b := getBriefing(t, dm2, res.BriefingID)
		secs := sectionsOf(b)
		if secs[0]["kind"] != "pending" || len(secs[0]["items"].([]any)) != 1 || secs[0]["items"].([]any)[0].(map[string]any)["approval_id"] != approval {
			t.Fatalf("%+v", secs)
		}
		approve(t, approval)
	})

	run("用例42：卡片动作（含被拒绝的）、撤销、重新推送、偏好变更都有审计，含渠道、操作标识与结果", func(t *testing.T) {
		for _, a := range []string{"channels.action.executed", "channels.action.rejected", "channels.action.replayed", "channels.briefing.resent",
			"channels.briefing_preferences.updated", "channels.briefing.composed", "channels.delivery.delivered", "channels.delivery.in_app"} {
			if audits(t, a, "") == 0 {
				t.Fatalf("没有 %s 审计", a)
			}
		}
		var basis string
		if err := chnOwner.QueryRow(ctx, `SELECT envelope->'data'->>'basis' FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1'
 AND envelope->'data'->>'action' = 'channels.action.executed' LIMIT 1`).Scan(&basis); err != nil || !strings.HasPrefix(basis, "wecom;operation=atk_") || !strings.Contains(basis, ";result=") {
			t.Fatalf("%s %v", basis, err)
		}
	})

	run("越权与反例测试 来源权利收紧：快照不固定展示权限——完整简报、重新推送与排队重试的发送都按当前权利重新裁剪（full_text→none、full_text→citation_only、权利不可得）", func(t *testing.T) {
		// dm 的一期推送失败、待重试；组装时全部字段为 full_text
		wc.FailNextSends(1)
		res := compose(t, dm, at(30, 7, 31))
		if res.DeliveryID == "" {
			t.Fatalf("%+v", res)
		}
		var target string
		var amount, project string
		for _, it := range objectiveSection(t, getBriefing(t, dm, res.BriefingID))["items"].([]any) {
			m := it.(map[string]any)
			a, _ := m["amount"].(map[string]any)
			if m["kind"] == "signal" && a != nil && a["level"] == "full_text" && a["value"] != nil {
				target, amount = m["signal_id"].(string), a["value"].(string)
				project = m["project"].(map[string]any)["value"].(string)
				break
			}
		}
		if target == "" {
			t.Fatal("没有 full_text 的金额")
		}
		// 组装之后权利收紧：金额 none，项目只允许引用与链接
		rights.set(target, "amount", chapi.FieldRight{Levels: []string{"none"}})
		rights.set(target, "project", chapi.FieldRight{Levels: []string{"citation_only"}, SourceURL: "https://example.com/p/" + target})
		cardText := func(c sentCard) string { return strings.Join(c.card.Lines, "\n") }
		// 排队重试的发送：按当前权利重新渲染，按钮与令牌不变
		var before []byte
		if err := chnOwner.QueryRow(ctx, `SELECT messages FROM chn_deliveries WHERE id = $1`, res.DeliveryID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		from := len(wc.Messages(corp1, account[dm]))
		deliverDue(t, at(30, 7, 40))
		c := cardFor(t, cards(t, corp1, account[dm], from), target)
		if txt := cardText(c); strings.Contains(txt, amount) || strings.Contains(txt, project) || !strings.Contains(txt, "见原文 https://example.com/p/") {
			t.Fatalf("重试发送泄露：%s", txt)
		}
		if !strings.Contains(string(before), button(c.card, "relevant").Token) {
			t.Fatal("重新渲染不得更换令牌")
		}
		ov := wc.Messages(corp1, account[dm])[from]
		if ov.Kind != "briefing_overview" || !strings.Contains(strings.Join(ov.Card.Lines, "\n"), "见原文 https://example.com/p/") {
			t.Fatalf("%+v", ov.Card)
		}
		// 完整简报：快照内容不变，展示按当前权利
		check := func(t *testing.T, b map[string]any, wantProject string) {
			t.Helper()
			for _, it := range objectiveSection(t, b)["items"].([]any) {
				m := it.(map[string]any)
				if m["signal_id"] != target {
					continue
				}
				a, p := m["amount"].(map[string]any), m["project"].(map[string]any)
				if a["level"] != "none" || a["value"] != nil || a["source_url"] != nil || p["level"] != wantProject || p["value"] != nil {
					t.Fatalf("%+v", m)
				}
			}
		}
		check(t, getBriefing(t, dm, res.BriefingID), "citation_only")
		// 本人重新推送：新卡片同样按当前权利
		from = len(wc.Messages(corp1, account[dm]))
		expect(t, dm, false, "POST", "/v1/channels/briefings/"+res.BriefingID+"/resend", "", 200)
		if txt := cardText(cardFor(t, cards(t, corp1, account[dm], from), target)); strings.Contains(txt, amount) || strings.Contains(txt, project) {
			t.Fatalf("重新推送泄露：%s", txt)
		}
		// 权利服务不可得：失败关闭为 none（全部条目），重新推送的卡片不含任何来源值
		rights.fail.Store(true)
		b := getBriefing(t, dm, res.BriefingID)
		check(t, b, "none")
		for _, it := range objectiveSection(t, b)["items"].([]any) {
			for _, f := range []string{"project", "winner", "amount", "region", "categories"} {
				if v, _ := it.(map[string]any)[f].(map[string]any); v != nil && (v["level"] != "none" || v["value"] != nil) {
					t.Fatalf("%s：%+v", f, v)
				}
			}
		}
		from = len(wc.Messages(corp1, account[dm]))
		expect(t, dm, false, "POST", "/v1/channels/briefings/"+res.BriefingID+"/resend", "", 200)
		for _, c := range cards(t, corp1, account[dm], from) {
			if txt := cardText(c); strings.Contains(txt, " 万") || strings.Contains(txt, "夹具") {
				t.Fatalf("权利不可得时泄露：%s", txt)
			}
		}
		rights.fail.Store(false)
	})

}
