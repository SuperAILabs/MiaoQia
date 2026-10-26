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
	purcontract "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/authz"
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
	"github.com/superailabs/miaoqia/services/subsystems/detection/api/facttest"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api/datasettest"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit"
	pursuitapi "github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
	pursuitremote "github.com/superailabs/miaoqia/services/subsystems/pursuit/api/remote"
)

// unwiredPursuits 是未开启卡片“跟进”入口的用例中 pursuit 渠道内动作的占位（任何调用都是暂时失败）。
type unwiredPursuits struct{}

func (unwiredPursuits) CreatePursuit(context.Context, string, *pursuitapi.CreatePursuitRequest) (*pursuitapi.CommandResult, error) {
	return nil, binding.ErrTransient
}
func (unwiredPursuits) GetTask(context.Context, string) (*pursuitapi.TaskView, error) {
	return nil, binding.ErrTransient
}
func (unwiredPursuits) CompleteTask(context.Context, string, string, *pursuitapi.CompleteTaskRequest) (*pursuitapi.CommandResult, error) {
	return nil, binding.ErrTransient
}
func (unwiredPursuits) ReopenTask(context.Context, string, string, *pursuitapi.ReopenTaskRequest) (*pursuitapi.CommandResult, error) {
	return nil, binding.ErrTransient
}

// PRD 第6段服务端切片（交易型跟进）：真实 PostgreSQL（各子系统自己的库、行级安全、应用账号）+ 真实 identity 目录与 OpenFGA + 外部网关
// 令牌校验 + 生成的 HTTP 服务端。channels 经 mTLS 内部端口的远程绑定调用 pursuit 的渠道内动作（卡片“跟进”）；企业微信为内存实现
// （wecomtest）；定时任务未接入，测试以本地绑定 pursuit.Reminders 驱动提醒与授权同步重试，业务时钟由测试控制；identity 的参与关系
// 同步可注入不可用。用例编号对应 PRD 第6段第12节。
func TestPursuitFollowUp(t *testing.T) {
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
	if os.Getenv("PURSUIT_DEBUG") != "" {
		quiet = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	now := time.Now
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, shanghai) }
	clock := &testClock{}  // channels
	pclock := &testClock{} // pursuit
	pclock.set(at(21, 10, 0), 0)

	iam := testenv.NewDatabase(t)
	idModule := identity.ModuleWith(identity.Adapters{Now: now})
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", idModule.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2 := ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	user := func(role string) string { return role + "-" + suffix }
	admin, dm, dm2, bd, bd2, lead, expert, bd3, admin2 := user("admin"), user("dm"), user("dm2"), user("bd"), user("bd2"), user("lead"), user("expert"), user("bd3"), user("admin2")
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
 ($1,$8,'active',ARRAY['domain_expert'],1), ($1,$9,'active',ARRAY['business_developer'],1), ($10,$11,'active',ARRAY['admin','domain_expert','decision_maker','business_developer'],1)`,
			[]any{t1, admin, dm, dm2, bd, bd2, lead, expert, bd3, t2, admin2}},
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
		case "PURSUE_TENANTS":
			return " " + t1 + " ,"
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
	ccModule := customercontext.Module()
	ccs := start(ccModule, "api")
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
	facts := facttest.New(t1, t2)
	detModule := detection.ModuleWith(detection.Adapters{Facts: facts, Candidates: facts, Now: now})
	dets := start(detModule, "api")
	detModule.Routes(dets.env, public, internal)

	// pursuit：identity 目录经可注入失败的包装（授权同步）；加载于 api 角色（从信号创建需要 detection）
	localDirectory, _ := runtime.Lookup[idapi.Directory](bindings, idapi.DirectoryBinding)
	directory := &flakyDirectory{Directory: localDirectory}
	bindings.Provide(idapi.DirectoryBinding, idapi.Directory(directory))
	purModule := pursuit.ModuleWith(pursuit.Adapters{Now: pclock.Now})
	purs := start(purModule, "api")
	purModule.Routes(purs.env, public, internal)
	purOwner := purs.owner
	reminders, _ := runtime.Lookup[pursuitapi.Reminders](bindings, pursuitapi.RemindersBinding)
	if reminders == nil {
		t.Fatal("pursuit 未提供 Reminders")
	}
	bindings.Provide(idapi.DirectoryBinding, localDirectory)

	// mTLS 内部端口：channels 经远程绑定调用 pursuit 的渠道内动作（channel 为 wecom 只经内部端口接受）
	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	intern := httptest.NewUnstartedServer(internal)
	intern.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	intern.StartTLS()
	defer intern.Close()
	remoteActions, err := pursuitremote.NewActions(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	bindings.Provide(pursuitapi.ActionsBinding, remoteActions)

	wc := wecomtest.New()
	chnModule := channels.ModuleWith(channels.Adapters{WeCom: wc, Coverage: &fakeCoverage{interrupted: map[string]time.Time{}},
		Rights: &fakeRights{override: map[string]map[string]chapi.FieldRight{}}, Now: clock.Now})
	chns := start(chnModule, "api")
	chnModule.Routes(chns.env, public, internal)
	chnOwner := chns.owner
	briefings, _ := runtime.Lookup[chapi.Briefings](bindings, chapi.BriefingsBinding)
	callbacks, _ := runtime.Lookup[chapi.Callbacks](bindings, chapi.CallbacksBinding)

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

	// 发件箱泵：全部事件按契约校验后交给订阅它的消费者。消费者返回错误时事件留在待重试队列（与消息总线的退避重试相同：
	// 下一次泵先重试），不视为测试失败；drained 断言没有待重试的事件。
	type source struct {
		name  string
		owner *pgx.Conn
		last  int64
	}
	sources := []*source{{name: "identity", owner: iamOwner}, {name: "customer-context", owner: ccs.owner}, {name: "learning", owner: lrns.owner},
		{name: "detection", owner: dets.owner}, {name: "pursuit", owner: purOwner}, {name: "channels", owner: chnOwner}}
	type route struct {
		module   runtime.Module
		env      *runtime.Env
		consumer string
	}
	var routesMu sync.Mutex
	routes := map[string][]route{
		"identity.approval.requested.v1":        {{ccModule, ccs.env, "customer-context.policy-approvals"}},
		"identity.approval.decided.v1":          {{ccModule, ccs.env, "customer-context.policy-approvals"}},
		"identity.approval.closed.v1":           {{ccModule, ccs.env, "customer-context.policy-approvals"}},
		"customer_context.policy.drafted.v1":    {{lrnModule, lrns.env, "learning.policy-simulations"}},
		"learning.simulation.completed.v1":      {{ccModule, ccs.env, "customer-context.policy-simulations"}},
		"customer_context.objective.changed.v1": {{ccModule, ccs.env, "customer-context.objective-participants"}, {purModule, purs.env, "pursuit.objective-changes"}},
		"detection.signal.detected.v1":          {{lrnModule, lrns.env, "learning.signal-feedback"}},
		"detection.signal.triaged.v1":           {{lrnModule, lrns.env, "learning.signal-feedback"}},
		"identity.member.changed.v1":            {{chnModule, chns.env, "channels.member-changes"}, {purModule, purs.env, "pursuit.member-changes"}},
		"pursuit.pursuit.signal_linked.v1":      {{detModule, dets.env, "detection.pursuit-links"}},
		"pursuit.pursuit.signal_unlinked.v1":    {{detModule, dets.env, "detection.pursuit-links"}},
		"pursuit.outcome.recorded.v1":           {{lrnModule, lrns.env, "learning.pursuit-outcomes"}},
		"pursuit.team.changed.v1":               {{purModule, purs.env, "pursuit.authorization-sync"}},
	}
	type pendingEvent struct {
		r route
		e outbox.Envelope
	}
	var delivered []outbox.Envelope
	var pending []pendingEvent
	handle := func(t *testing.T, r route, e outbox.Envelope) error {
		t.Helper()
		for _, c := range r.module.Consumers(r.env) {
			if c.Name != r.consumer {
				continue
			}
			if !slices.Contains(c.Topics, outbox.Topic(e.Zone, "cell-01", strings.SplitN(e.Type, ".", 2)[0])) {
				t.Fatalf("%s 不订阅 %s 的主题", c.Name, e.Type)
			}
			ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Tenant: e.Tenant, Cell: e.Cell, Purpose: "event"}), c.Subsystem)
			return r.env.DB.Tx(ectx, func(tx pgx.Tx) error { return c.Handlers[e.Type](ectx, tx, &e) })
		}
		t.Fatalf("没有消费者 %s", r.consumer)
		return nil
	}
	pump := func(t *testing.T) {
		t.Helper()
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
						t.Fatalf("%s 的 %s 不符合契约：%v：%s", s.name, e.Type, err, e.Data)
					}
					delivered = append(delivered, e)
					routesMu.Lock()
					rs := slices.Clone(routes[e.Type])
					routesMu.Unlock()
					for _, r := range rs {
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
	eventsFor := func(typ, key, value string) []map[string]any {
		var out []map[string]any
		for _, d := range eventsOf(typ) {
			if d[key] == value {
				out = append(out, d)
			}
		}
		return out
	}
	audits := func(t *testing.T, owner *pgx.Conn, action, resourcePrefix string) int {
		t.Helper()
		return count(t, owner, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1' AND envelope->'data'->>'action' = $1
 AND envelope->'data'->>'resource' LIKE $2`, action, resourcePrefix+"%")
	}

	// 公共事实与匹配（同 TestBriefingQuickActions）
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
	signalsOf := func(t *testing.T, notice string) []string {
		t.Helper()
		rows, err := dets.owner.Query(ctx, `SELECT id FROM det_signals WHERE subject_id = $1 ORDER BY id`, notice)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	inP1 := at(19, 20, 0)
	var sigSeq atomic.Int64
	newSignal := func(t *testing.T, bids ...facttest.Bid) []string {
		t.Helper()
		n, e := fact(t, bids...)
		mustMatch(t, e)
		s := signalsOf(t, n)
		if len(s) != len(bids) {
			t.Fatalf("%v", s)
		}
		for _, id := range s {
			if _, err := dets.owner.Exec(ctx, `UPDATE det_signals SET created_at = $2 WHERE id = $1`, id, inP1.Add(time.Duration(sigSeq.Add(1))*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	pursueBid := func() facttest.Bid {
		return facttest.Bid{Amount: 6_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}, WinnerName: "夹具总包"}
	}
	signalRow := func(t *testing.T, who, id string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, "GET", "/v1/detection/signals/"+id, "", 200))["signal"].(map[string]any)
	}

	// 档案与 A02 目标：决策者 dm 启用；业务开拓者 bd、bd2，项目推进者 lead，业务专家 expert 与管理员 admin 参与；bd3 不参与
	content := fmt.Sprintf(`{"subject_type":"organization","entities":[{"org_id":"%s","qualifications_complete":true}],"value_chain_roles":["ROLE-MFR"],
"industry_packs":["IND-BUILDING-MATERIALS"],"offering_categories":["CAT-PIPE-PE"],"brands":["示例品牌"],"service_regions":["330000"],"qualifications":[]}`, org1)
	expect(t, expert, "PUT", "/v1/customer-context/profile", `{"expected_version":0,"content":`+content+`}`, 200)
	var objective cc.Objective
	decode(t, expect(t, dm, "POST", "/v1/customer-context/objectives",
		fmt.Sprintf(`{"template_id":"APP-TENDER.A02","org_unit_id":"root","participants":["%s","%s","%s","%s","%s"]}`, bd, bd2, lead, expert, admin), 200), &objective)
	objPath := "/v1/customer-context/objectives/" + objective.ID
	policyPath := objPath + "/policy"
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
		pump(t)
	}
	setParticipants := func(t *testing.T, participants ...string) {
		t.Helper()
		var o cc.Objective
		decode(t, expect(t, dm, "GET", objPath, "", 200), &o)
		params, _ := json.Marshal(o.Params)
		list, _ := json.Marshal(participants)
		expect(t, dm, "PUT", objPath, fmt.Sprintf(`{"expected_version":%d,"owner":"%s","participants":%s,"params":%s}`, o.Version, dm, list, params), 200)
		pump(t)
	}
	putMember := func(t *testing.T, subject, state, roles string) {
		t.Helper()
		var m struct {
			MemberVersion int64 `json:"member_version"`
		}
		decode(t, expectMFA(t, admin, "GET", "/v1/identity/members/"+subject, "", 200), &m)
		expectMFA(t, admin, "PUT", "/v1/identity/members/"+subject, fmt.Sprintf(`{"state":%q,"roles":%s,"expected_version":%d}`, state, roles, m.MemberVersion), 200)
		pump(t)
	}

	// 跟进接口的小工具
	var opSeq atomic.Int64
	opID := func(tag string) string { return fmt.Sprintf("op-%s-%s-%d", tag, suffix, opSeq.Add(1)) }
	fromSignal := func(t *testing.T, who, signal string, extra string) string {
		t.Helper()
		row := signalRow(t, who, signal)
		return fmt.Sprintf(`{"operation_id":%q,"objective_id":%q,"signal_id":%q,"seen_content_version":%d%s}`, opID("c"), objective.ID, signal, num(row, "content_version"), extra)
	}
	targetOf := func(t *testing.T, signal string) string {
		t.Helper()
		row := signalRow(t, dm, signal)
		sub := row["subject"].(map[string]any)
		tg := map[string]any{"fact_type": row["fact_type"], "subject_id": sub["id"]}
		if p, ok := sub["part_id"].(string); ok && p != "" {
			tg["part_id"] = p
		}
		if w, ok := row["winner"].(map[string]any); ok {
			if o, ok := w["org_id"].(string); ok {
				tg["winner_org_id"] = o
			}
		}
		raw, _ := json.Marshal(tg)
		return string(raw)
	}
	create := func(t *testing.T, who, body string, status int, code ...string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, "POST", "/v1/pursuit/pursuits", body, status, code...))
	}
	pursuitOf := func(res map[string]any) string { return res["pursuit"].(map[string]any)["id"].(string) }
	detail := func(t *testing.T, who, id string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, "GET", "/v1/pursuit/pursuits/"+id, "", 200))
	}
	versionOf := func(t *testing.T, id string) int64 {
		t.Helper()
		var v int64
		if err := purOwner.QueryRow(ctx, `SELECT version FROM pur_pursuits WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	assign := func(t *testing.T, who, id, owner string, collaborators []string, status int, code ...string) map[string]any {
		t.Helper()
		if collaborators == nil {
			collaborators = []string{}
		}
		list, _ := json.Marshal(collaborators)
		return obj(t, expect(t, who, "POST", "/v1/pursuit/pursuits/"+id+"/assign", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"owner":%q,"collaborators":%s}`,
			opID("a"), versionOf(t, id), owner, list), status, code...))
	}
	taskVersion := func(t *testing.T, task string) int64 {
		t.Helper()
		var v int64
		if err := purOwner.QueryRow(ctx, `SELECT version FROM pur_tasks WHERE id = $1`, task).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	addTask := func(t *testing.T, who, id, task string, status int, code ...string) map[string]any {
		t.Helper()
		return obj(t, expect(t, who, "POST", "/v1/pursuit/pursuits/"+id+"/tasks", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"task":%s}`,
			opID("t"), versionOf(t, id), task), status, code...))
	}
	allowedOn := func(t *testing.T, subject, relation, object string) bool {
		t.Helper()
		a, _ := runtime.Lookup[idapi.Authorizer](bindings, idapi.AuthorizerBinding)
		uctx := reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01", Principal: reqctx.Principal{ID: subject, Kind: "user"}})
		ok, err := a.Check(uctx, authz.Request{Subject: "user:" + subject, Relation: relation, Object: object})
		var denied *idapi.Error
		if errors.As(err, &denied) && denied.Status == 403 {
			return false // 已退出的成员：租户上下文无效，等同于没有关系
		}
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	svc := func(tenant string) context.Context {
		return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:pursuit-scheduler", Kind: "service"}})
	}
	runDue := func(t *testing.T, when time.Time) int {
		t.Helper()
		n, err := reminders.RunDue(svc(t1), when, 100)
		if err != nil {
			t.Fatal(err)
		}
		pump(t)
		return n
	}

	// 后续用例依赖前面的状态：任何一步失败即停止
	run := func(name string, f func(t *testing.T)) {
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	// 信号：s1–s8 为跟进候选；ex 为排除（中标方是本方）；lotA/lotB 为同一标包的两家中标方
	s1 := newSignal(t, pursueBid())[0]
	s2 := newSignal(t, pursueBid())[0]
	s3 := newSignal(t, pursueBid())[0]
	s4 := newSignal(t, pursueBid())[0]
	s5 := newSignal(t, pursueBid())[0]
	s6 := newSignal(t, pursueBid())[0]
	s7 := newSignal(t, pursueBid())[0]
	ex := newSignal(t, facttest.Bid{Winner: org1, Amount: 3_000_000})[0]
	lot := newSignal(t, facttest.Bid{Amount: 6_000_000, HistoryCategories: []string{"CAT-PIPE-PE"}, Lot: "L1"},
		facttest.Bid{Amount: 6_100_000, HistoryCategories: []string{"CAT-PIPE-PE"}, Lot: "L1"})
	drained(t)

	var p1, p1Task string
	run("越权与反例测试 用例6、48、5：无 can_assign 的业务专家 403、非参与人与他租户 404、被分派人无目标访问 422 且不写入", func(t *testing.T) {
		create(t, expert, fromSignal(t, dm, s1, ""), 403, "forbidden")
		create(t, bd3, fromSignal(t, dm, s1, ""), 404, "objective_not_found")
		expectAs(t, admin2, t2, "POST", "/v1/pursuit/pursuits", fromSignal(t, dm, s1, ""), "", 404, "objective_not_found")
		// 被分派人 bd3 不是目标参与人（看不到信号）；协作者 expert 能看信号（参与人）但 expert 作为负责人同样允许：这里只验证拒绝
		create(t, bd, fromSignal(t, bd, s1, fmt.Sprintf(`,"owner":%q`, bd3)), 422, "assignee_no_access")
		create(t, bd, fromSignal(t, bd, s1, fmt.Sprintf(`,"collaborators":[%q]`, bd3)), 422, "assignee_no_access")
		if n := count(t, purOwner, `SELECT count(*) FROM pur_pursuits`); n != 0 {
			t.Fatalf("拒绝的创建写入了 %d 个跟进", n)
		}
		if row := signalRow(t, dm, s1); row["status"] != "open" {
			t.Fatalf("%+v", row)
		}
		// 渠道创建只经内部端口：公开接口上带 channel=wecom 被拒绝
		create(t, bd, fromSignal(t, bd, s1, `,"channel":"wecom"`), 403, "forbidden")
	})

	run("用例7、8：旧内容版本 409 signal_updated（带当前版本）不创建；排除信号 422 not_applicable", func(t *testing.T) {
		row := signalRow(t, bd, s1)
		body := fmt.Sprintf(`{"operation_id":%q,"objective_id":%q,"signal_id":%q,"seen_content_version":%d}`, opID("c"), objective.ID, s1, num(row, "content_version")+7)
		res := create(t, bd, body, 409, "signal_updated")
		if cur := res["signal_current"].(map[string]any); num(cur, "content_version") != num(row, "content_version") {
			t.Fatalf("%+v", res)
		}
		create(t, bd, fromSignal(t, bd, ex, ""), 422, "not_applicable")
		if n := count(t, purOwner, `SELECT count(*) FROM pur_pursuits`); n != 0 {
			t.Fatalf("%d", n)
		}
	})

	run("用例1、55、15、16、49：从信号创建（负责人 lead、协作者 bd2、第一项任务只填日期）；identity 不可用时本地创建成功并返回“授权同步中”，新负责人同步前不能修改任务、不在待办、提醒延后；同步后一次分派通知", func(t *testing.T) {
		directory.fail.Store(true)
		body := fromSignal(t, bd, s1, fmt.Sprintf(`,"owner":%q,"collaborators":[%q],"first_task":{"title":"联系总包采购","due":{"value":"2026-10-25","precision":"day","timezone":"Asia/Shanghai"}}`, lead, bd2))
		key := "create-" + suffix + "-0001"
		res := obj(t, expectAs(t, bd, t1, "POST", "/v1/pursuit/pursuits", body, key, 200))
		if res["result"] != "created" || res["authorization"] != "syncing" {
			t.Fatalf("%+v", res)
		}
		p1 = pursuitOf(res)
		sum := res["pursuit"].(map[string]any)
		if sum["owner"] != lead || sum["state"] != "open" || sum["authorization"].(map[string]any)["state"] != "syncing" {
			t.Fatalf("%+v", sum)
		}
		// 同一幂等键重发返回同一结果；新键、同一操作标识返回已固定的“已创建”，只有一个跟进
		again := obj(t, expectAs(t, bd, t1, "POST", "/v1/pursuit/pursuits", body, key, 200))
		retry := obj(t, expectAs(t, bd, t1, "POST", "/v1/pursuit/pursuits", body, "", 200))
		if pursuitOf(again) != p1 || pursuitOf(retry) != p1 || retry["result"] != "created" {
			t.Fatalf("%+v %+v", again, retry)
		}
		if n := count(t, purOwner, `SELECT count(*) FROM pur_pursuits`); n != 1 {
			t.Fatalf("%d", n)
		}
		pump(t)
		if len(pending) == 0 {
			t.Fatal("授权同步应失败并等待重试")
		}
		if row := signalRow(t, dm, s1); row["status"] != "converted" {
			t.Fatalf("%+v", row)
		}
		if err := purOwner.QueryRow(ctx, `SELECT id FROM pur_tasks WHERE pursuit_id = $1`, p1).Scan(&p1Task); err != nil {
			t.Fatal(err)
		}
		// 新负责人同步前：不能修改任务（authorization_syncing，不固定在操作上）、待办中没有、提醒延后
		upd := fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"title":"联系总包采购部"}`, "op-upd-"+suffix, taskVersion(t, p1Task))
		expect(t, lead, "POST", "/v1/pursuit/tasks/"+p1Task+"/update", upd, 403, "authorization_syncing")
		if items := obj(t, expect(t, lead, "GET", "/v1/pursuit/my-tasks", "", 200))["items"].([]any); len(items) != 0 {
			t.Fatalf("%+v", items)
		}
		if n := runDue(t, at(24, 9, 1)); n != 0 {
			t.Fatalf("同步前发出了 %d 条提醒", n)
		}
		if len(eventsOf("pursuit.pursuit.assigned.v1")) != 0 {
			t.Fatal("同步前不得通知")
		}
		// 恢复：事件重试完成同步；分派通知只一次；之后同一操作标识的修改成功
		directory.fail.Store(false)
		drained(t)
		if a := eventsFor("pursuit.pursuit.assigned.v1", "pursuit_id", p1); len(a) != 1 || a[0]["assignee"] != lead || a[0]["kind"] != "created" || a[0]["assigned_by"] != bd {
			t.Fatalf("%+v", a)
		}
		if !allowedOn(t, lead, "can_edit", "pursuit:"+p1) || !allowedOn(t, bd2, "can_view", "pursuit:"+p1) {
			t.Fatal("参与关系未同步到 identity")
		}
		if d := detail(t, lead, p1); d["authorization"].(map[string]any)["state"] != "effective" || d["viewer_can_edit"] != true {
			t.Fatalf("%+v", d)
		}
		expect(t, lead, "POST", "/v1/pursuit/tasks/"+p1Task+"/update", upd, 200)
		if items := obj(t, expect(t, lead, "GET", "/v1/pursuit/my-tasks", "", 200))["items"].([]any); len(items) != 1 {
			t.Fatalf("%+v", items)
		}
		// 已到时的“前一日 09:00”提醒在同步后发出一次，收件人为任务负责人
		if n := runDue(t, at(24, 9, 2)); n != 1 {
			t.Fatalf("%d", n)
		}
		if n := runDue(t, at(24, 9, 3)); n != 0 {
			t.Fatalf("重复发送 %d", n)
		}
		due := eventsFor("pursuit.task.due.v1", "task_id", p1Task)
		if len(due) != 1 || due[0]["recipient"] != lead || due[0]["kind"] != "day_before_0900" || due[0]["escalated"] != false {
			t.Fatalf("%+v", due)
		}
		if created := eventsFor("pursuit.pursuit.created.v1", "pursuit_id", p1); len(created) != 1 || created[0]["channel"] != "web" || created[0]["signal_id"] != s1 {
			t.Fatalf("%+v", created)
		}
		if audits(t, purOwner, "pursuit.pursuit.create", p1) != 1 || audits(t, purOwner, "pursuit.task.update", p1Task) != 1 {
			t.Fatal("审计")
		}
	})

	run("用例9、50：同一信号再次创建为“已存在”（无改变），请求中的负责人与任务未写入并提示，不新增关联、不再发事件", func(t *testing.T) {
		linked := len(eventsOf("pursuit.pursuit.signal_linked.v1"))
		res := create(t, bd2, fromSignal(t, bd2, s1, fmt.Sprintf(`,"owner":%q,"first_task":{"title":"另一项"}`, bd2)), 200)
		if res["result"] != "existing" || pursuitOf(res) != p1 || res["pursuit"].(map[string]any)["owner"] != lead {
			t.Fatalf("%+v", res)
		}
		nw := res["not_written"].([]any)
		if !slices.Contains(nw, any("owner")) || !slices.Contains(nw, any("first_task")) {
			t.Fatalf("%+v", nw)
		}
		drained(t)
		if len(eventsOf("pursuit.pursuit.signal_linked.v1")) != linked || count(t, purOwner, `SELECT count(*) FROM pur_tasks WHERE pursuit_id = $1`, p1) != 1 {
			t.Fatal("已存在不得写入")
		}
	})

	var manual string
	run("用例3、13、52：手动跟进标包与中标方后信号到达并“跟进”：已存在 · 已关联，负责人与任务不变，只发 signal_linked，信号转为已转跟进；之后创建者撤销创建 CONFLICT", func(t *testing.T) {
		res := create(t, bd, fmt.Sprintf(`{"operation_id":%q,"objective_id":%q,"target":%s}`, opID("m"), objective.ID, targetOf(t, s2)), 200)
		manual = pursuitOf(res)
		if res["result"] != "created" {
			t.Fatalf("%+v", res)
		}
		drained(t)
		created := len(eventsOf("pursuit.pursuit.created.v1"))
		res = create(t, bd2, fromSignal(t, bd2, s2, ""), 200)
		if res["result"] != "existing_linked" || pursuitOf(res) != manual || res["pursuit"].(map[string]any)["owner"] != bd {
			t.Fatalf("%+v", res)
		}
		drained(t)
		if len(eventsOf("pursuit.pursuit.created.v1")) != created || len(eventsFor("pursuit.pursuit.signal_linked.v1", "pursuit_id", manual)) != 1 {
			t.Fatal("关联已有跟进只发 signal_linked")
		}
		if row := signalRow(t, dm, s2); row["status"] != "converted" {
			t.Fatalf("%+v", row)
		}
		d := detail(t, bd, manual)
		if d["source"].(map[string]any)["kind"] != "manual" || len(d["links"].([]any)) != 1 || d["viewer_can_void"] != false {
			t.Fatalf("%+v", d)
		}
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+manual+"/void", fmt.Sprintf(`{"operation_id":%q,"expected_version":1}`, opID("v")), 409, "conflict")
	})

	run("用例12：同一标包的两家中标方各一个跟进", func(t *testing.T) {
		a := create(t, bd, fromSignal(t, bd, lot[0], ""), 200)
		b := create(t, bd, fromSignal(t, bd, lot[1], ""), 200)
		if a["result"] != "created" || b["result"] != "created" || pursuitOf(a) == pursuitOf(b) {
			t.Fatalf("%+v %+v", a, b)
		}
		drained(t)
	})

	run("用例11：两人同时从同一信号创建，只有一个跟进；另一方得到“已存在”与胜出者；各自重试返回各自固定的结果", func(t *testing.T) {
		bodies := map[string]string{bd: fromSignal(t, bd, s7, ""), bd2: fromSignal(t, bd2, s7, "")}
		results := map[string]map[string]any{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for who, body := range bodies {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, raw := do(t, who, t1, false, "POST", "/v1/pursuit/pursuits", body, "")
				var m map[string]any
				_ = json.Unmarshal(raw, &m)
				mu.Lock()
				defer mu.Unlock()
				if code == 200 {
					results[who] = m
				}
			}()
		}
		wg.Wait()
		if len(results) != 2 || pursuitOf(results[bd]) != pursuitOf(results[bd2]) {
			t.Fatalf("%+v", results)
		}
		got := []string{results[bd]["result"].(string), results[bd2]["result"].(string)}
		slices.Sort(got)
		if strings.Join(got, ",") != "created,existing" {
			t.Fatalf("%v", got)
		}
		if n := count(t, purOwner, `SELECT count(*) FROM pur_pursuits WHERE source_signal_id = $1 OR (state = 'open' AND target_key IN (SELECT target_key FROM pur_pursuits WHERE source_signal_id = $1))`, s7); n != 1 {
			t.Fatalf("%d", n)
		}
		for who, body := range bodies {
			if again := create(t, who, body, 200); again["result"] != results[who]["result"] {
				t.Fatalf("%s 重试：%+v", who, again)
			}
		}
		drained(t)
	})

	run("用例19、20、23、57：已忽略的信号创建后转为已转跟进；非创建者撤销 403；identity 不可用时撤销返回“授权清理中”，任务与提醒取消、关联解除，信号回到已忽略；恢复后关系删除；超过 10 分钟不能撤销", func(t *testing.T) {
		row := signalRow(t, dm, s4)
		expect(t, dm, "POST", "/v1/detection/signals/"+s4+"/triage", fmt.Sprintf(`{"operation_id":%q,"action":"dismiss","seen_content_version":%d,"state_version":%d,"feedback_version":%d}`,
			opID("dismiss"), num(row, "content_version"), num(row, "state_version"), num(row, "feedback_version")), 200)
		pump(t)
		res := create(t, bd, fromSignal(t, bd, s4, `,"first_task":{"title":"电话","due":{"value":"2026-10-30T10:00","precision":"minute","timezone":"Asia/Shanghai"}}`), 200)
		p := pursuitOf(res)
		drained(t)
		if row := signalRow(t, dm, s4); row["status"] != "converted" {
			t.Fatalf("%+v", row)
		}
		if d := detail(t, bd, p); d["viewer_can_void"] != true {
			t.Fatalf("%+v", d)
		}
		// 非创建者
		expect(t, dm, "POST", "/v1/pursuit/pursuits/"+p+"/void", fmt.Sprintf(`{"operation_id":%q,"expected_version":1}`, opID("v")), 403, "forbidden")
		directory.fail.Store(true)
		pclock.set(at(21, 10, 8), 0)
		res = obj(t, expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p+"/void", fmt.Sprintf(`{"operation_id":%q,"expected_version":1}`, opID("v")), 200))
		if res["result"] != "succeeded" || res["authorization"] != "cleaning" || res["signal_restoration"] == nil {
			t.Fatalf("%+v", res)
		}
		// 撤销后立即不接受任何操作（其他成员看不到）
		expect(t, bd2, "GET", "/v1/pursuit/pursuits/"+p, "", 404, "pursuit_not_found")
		if n := count(t, purOwner, `SELECT count(*) FROM pur_reminders WHERE pursuit_id = $1 AND state = 'planned'`, p); n != 0 {
			t.Fatalf("%d", n)
		}
		pump(t)
		if row := signalRow(t, dm, s4); row["status"] != "dismissed" {
			t.Fatalf("撤销后应回到已忽略：%+v", row)
		}
		if !allowedOn(t, bd, "can_edit", "pursuit:"+p) {
			t.Fatal("同步前 identity 中的关系应仍在（残留）")
		}
		directory.fail.Store(false)
		drained(t)
		if allowedOn(t, bd, "can_edit", "pursuit:"+p) {
			t.Fatal("撤销后的关系未删除")
		}
		if v := eventsFor("pursuit.pursuit.voided.v1", "pursuit_id", p); len(v) != 1 || num(v[0], "unlinked") != 1 {
			t.Fatalf("%+v", v)
		}
		// 超过 10 分钟：没有“撤销创建”，直接调用 409
		late := create(t, bd, fromSignal(t, bd, s5, ""), 200)
		pclock.set(at(21, 10, 19), 0)
		if d := detail(t, bd, pursuitOf(late)); d["viewer_can_void"] != false {
			t.Fatalf("%+v", d)
		}
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+pursuitOf(late)+"/void", fmt.Sprintf(`{"operation_id":%q,"expected_version":1}`, opID("v")), 409, "void_window_expired")
		pclock.set(at(21, 10, 0), 0)
		drained(t)
	})

	run("越权与反例测试 用例16：确定失败固定在操作标识上（新键同一操作返回同一失败）、同键不同请求 409、他人使用同一操作标识 409", func(t *testing.T) {
		op := opID("fixed")
		body := fmt.Sprintf(`{"operation_id":%q,"objective_id":%q,"signal_id":%q,"seen_content_version":%d,"owner":%q}`, op, objective.ID, s6, num(signalRow(t, bd, s6), "content_version"), bd3)
		create(t, bd, body, 422, "assignee_no_access")
		// 新幂等键、同一操作标识：返回已固定的失败（不重新判断，不写入）
		create(t, bd, body, 422, "assignee_no_access")
		key := "fixed-key-" + suffix
		expectAs(t, bd, t1, "POST", "/v1/pursuit/pursuits", body, key, 422, "assignee_no_access")
		expectAs(t, bd, t1, "POST", "/v1/pursuit/pursuits", strings.Replace(body, bd3, bd2, 1), key, 409, "idempotency_conflict")
		create(t, bd2, body, 409, "operation_conflict")
		if n := count(t, purOwner, `SELECT count(*) FROM pur_operations WHERE operation_id = $1 AND outcome = 'failed' AND code = 'assignee_no_access'`, op); n != 1 {
			t.Fatalf("%d", n)
		}
	})

	run("用例26、25、56：并发改派后提交者 CONFLICT 带当前负责人；identity 不可用时改派：原负责人立即不能以负责人身份操作，新负责人同步前也不能；同步后新负责人可操作、原关系删除、通知一次；改派给当前负责人无变化", func(t *testing.T) {
		v := versionOf(t, p1)
		body := func(owner string) string {
			return fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"owner":%q,"collaborators":[%q]}`, opID("a"), v, owner, bd2)
		}
		directory.fail.Store(true)
		res := obj(t, expect(t, dm, "POST", "/v1/pursuit/pursuits/"+p1+"/assign", body(bd), 200))
		if res["result"] != "succeeded" || res["authorization"] != "syncing" {
			t.Fatalf("%+v", res)
		}
		conflict := obj(t, expect(t, dm2, "POST", "/v1/pursuit/pursuits/"+p1+"/assign", body(bd2), 409, "conflict"))
		if conflict["current"].(map[string]any)["owner"] != bd {
			t.Fatalf("%+v", conflict)
		}
		// 原负责人 lead：identity 中仍有残留关系，但 pursuit 本地已不是负责人
		if !allowedOn(t, lead, "can_edit", "pursuit:"+p1) {
			t.Fatal("同步前 identity 中原负责人的关系应仍在")
		}
		upd := func(who, title string, status int, code string) {
			t.Helper()
			expect(t, who, "POST", "/v1/pursuit/tasks/"+p1Task+"/update", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"title":%q}`, opID("u"), taskVersion(t, p1Task), title),
				status, code)
		}
		upd(lead, "原负责人改", 403, "forbidden")
		upd(bd, "新负责人改", 403, "authorization_syncing")
		directory.fail.Store(false)
		drained(t)
		if allowedOn(t, lead, "can_edit", "pursuit:"+p1) || !allowedOn(t, bd, "can_edit", "pursuit:"+p1) {
			t.Fatal("同步后：原负责人关系应删除、新负责人应生效")
		}
		var assignee string
		if err := purOwner.QueryRow(ctx, `SELECT assignee FROM pur_tasks WHERE id = $1`, p1Task).Scan(&assignee); err != nil || assignee != bd {
			t.Fatalf("原负责人的待办任务应转给新负责人：%s %v", assignee, err)
		}
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+p1Task+"/update", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"title":"新负责人改"}`, opID("u"), taskVersion(t, p1Task)), 200)
		if a := eventsFor("pursuit.pursuit.assigned.v1", "pursuit_id", p1); len(a) != 2 || a[1]["assignee"] != bd || a[1]["kind"] != "reassigned" {
			t.Fatalf("%+v", a)
		}
		// 原负责人失去“分派给我”视图；新负责人有
		mine := func(who string) []any {
			return obj(t, expect(t, who, "GET", "/v1/pursuit/pursuits?view=mine", "", 200))["items"].([]any)
		}
		has := func(items []any, id string) bool {
			for _, it := range items {
				if it.(map[string]any)["id"] == id {
					return true
				}
			}
			return false
		}
		if has(mine(lead), p1) || !has(mine(bd), p1) {
			t.Fatal("分派给我视图")
		}
		if res := assign(t, dm, p1, bd, []string{bd2}, 200); res["result"] != "no_change" {
			t.Fatalf("%+v", res)
		}
		drained(t)
		if a := eventsFor("pursuit.pursuit.assigned.v1", "pursuit_id", p1); len(a) != 2 {
			t.Fatal("无变化不通知")
		}
		if audits(t, purOwner, "pursuit.pursuit.assign", p1) != 1 {
			t.Fatal("改派审计")
		}
	})

	run("用例28、29、31、32：分钟精度截止的提醒各一次（重复执行与并发执行不重复）；完成取消提醒，撤销完成恢复；已完成再完成无变化；撤销窗口外与他人改动后 CONFLICT", func(t *testing.T) {
		res := addTask(t, bd, p1, `{"title":"送样","due":{"value":"2026-10-28T09:30","precision":"minute","timezone":"Asia/Shanghai"}}`, 200)
		task := res["task"].(map[string]any)["id"].(string)
		due := res["task"].(map[string]any)["due"].(map[string]any)
		if due["value"] != "2026-10-28T09:30" || due["timezone"] != "Asia/Shanghai" {
			t.Fatalf("%+v", due)
		}
		// 10-27 09:31：本任务的提前 24 小时；p1 的任务已过当日与逾期两条，只发最晚的一条（逾期）
		if n := runDue(t, at(27, 9, 31)); n != 2 {
			t.Fatalf("%d", n)
		}
		// 并发执行（同一时刻两次）不重复；同一任务同一截止版本已过的多条提醒只发最晚的一条
		dueBefore := len(eventsOf("pursuit.task.due.v1"))
		var wg sync.WaitGroup
		var total atomic.Int64
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := reminders.RunDue(svc(t1), at(28, 9, 31), 100)
				if err == nil {
					total.Add(int64(n))
				}
			}()
		}
		wg.Wait()
		pump(t)
		if total.Load() != int64(len(eventsOf("pursuit.task.due.v1"))-dueBefore) {
			t.Fatalf("并发执行发送 %d，事件 %d", total.Load(), len(eventsOf("pursuit.task.due.v1"))-dueBefore)
		}
		if k := eventsFor("pursuit.task.due.v1", "task_id", p1Task); len(k) != 2 || k[1]["kind"] != "overdue_next_day_0900" {
			t.Fatalf("已过的当日与逾期提醒只发最晚的一条：%+v", k)
		}
		kinds := []string{}
		for _, d := range eventsFor("pursuit.task.due.v1", "task_id", task) {
			kinds = append(kinds, d["kind"].(string))
		}
		if strings.Join(kinds, ",") != "before_24h,at_due" {
			t.Fatalf("%v", kinds)
		}
		// 完成：取消逾期提醒；撤销完成（Web 10 秒内）恢复
		pclock.set(at(28, 10, 0), 0)
		completeOp := opID("done")
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+task+"/complete", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d}`, completeOp, taskVersion(t, task)), 200)
		if n := count(t, purOwner, `SELECT count(*) FROM pur_reminders WHERE task_id = $1 AND state = 'planned'`, task); n != 0 {
			t.Fatalf("%d", n)
		}
		if r := obj(t, expect(t, bd, "POST", "/v1/pursuit/tasks/"+task+"/complete", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d}`, opID("done"), taskVersion(t, task)-1), 200)); r["result"] != "no_change" {
			t.Fatalf("%+v", r)
		}
		// 他人（bd2）不能撤销 bd 的完成
		expect(t, bd2, "POST", "/v1/pursuit/tasks/"+task+"/reopen", fmt.Sprintf(`{"operation_id":%q,"target_operation_id":%q}`, opID("re"), completeOp), 404, "operation_not_found")
		pclock.set(at(28, 10, 0).Add(5*time.Second), 0)
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+task+"/reopen", fmt.Sprintf(`{"operation_id":%q,"target_operation_id":%q}`, opID("re"), completeOp), 200)
		if n := count(t, purOwner, `SELECT count(*) FROM pur_reminders WHERE task_id = $1 AND state = 'planned' AND kind = 'overdue_next_day_0900'`, task); n != 1 {
			t.Fatalf("撤销完成应恢复提醒：%d", n)
		}
		// 再完成后超过撤销窗口
		op2 := opID("done")
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+task+"/complete", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d}`, op2, taskVersion(t, task)), 200)
		pclock.set(at(28, 10, 1), 0)
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+task+"/reopen", fmt.Sprintf(`{"operation_id":%q,"target_operation_id":%q}`, opID("re"), op2), 409, "undo_window_expired")
		// 进程重启：新的模块实例（同一数据库）只发尚未发送的提醒
		restarted := pursuit.ModuleWith(pursuit.Adapters{Now: pclock.Now})
		rb := runtime.NewBindings()
		for _, name := range []string{idapi.AuthorizerBinding, idapi.DirectoryBinding, idapi.SessionBinding} {
			v, _ := runtime.Lookup[any](bindings, name)
			rb.Provide(name, v)
		}
		scope, _ := runtime.Lookup[any](bindings, ccapi.ScopeBinding)
		rb.Provide(ccapi.ScopeBinding, scope)
		if err := restarted.Start(ctx, &runtime.Env{DB: purs.env.DB, Bindings: rb, Log: quiet, Cell: "cell-01", Config: config, Role: "workflow-worker"}); err != nil {
			t.Fatal(err)
		}
		r2, _ := runtime.Lookup[pursuitapi.Reminders](rb, pursuitapi.RemindersBinding)
		if n, err := r2.RunDue(svc(t1), at(29, 9, 1), 100); err != nil || n != 0 {
			t.Fatalf("重启后重复发送：%d %v", n, err)
		}
		pclock.set(at(21, 10, 0), 0)
		drained(t)
	})

	run("用例2、24、45，越权与反例测试：卡片“跟进”（发布开关已开启、有 can_assign 才显示；业务专家没有）：以点击者为负责人创建，结果不带撤销；非收件人点击被拒绝且不消耗令牌；再点返回已固定的结果", func(t *testing.T) {
		svcCh := func(tenant string) context.Context {
			return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:briefing-scheduler", Kind: "service"}})
		}
		corp := "corp-" + suffix
		wc.Corp(corp, "企业"+corp)
		inst := ids.New("chi")
		if _, err := chnOwner.Exec(ctx, `INSERT INTO chn_installations (id, tenant_id, channel, state, version, corp_id, corp_name, verified_at) VALUES ($1, $2, 'wecom', 'enabled', 3, $3, $4, now())`,
			inst, t1, corp, "企业"+corp); err != nil {
			t.Fatal(err)
		}
		if _, err := chnOwner.Exec(ctx, `INSERT INTO chn_corp_routes (channel, corp_id, tenant_id, cell, installation_id) VALUES ('wecom', $1, $2, 'cell-01', $3)`, corp, t1, inst); err != nil {
			t.Fatal(err)
		}
		account := map[string]string{bd2: "u-bd2", expert: "u-ex", bd: "u-bd"}
		for s, a := range account {
			if _, err := chnOwner.Exec(ctx, `INSERT INTO chn_bindings (id, tenant_id, installation_id, channel, corp_id, channel_user_id, display_name, subject, member_version, state, version, bound_at)
 VALUES ($1, $2, $3, 'wecom', $4, $5, $6, $7, 1, 'bound', 1, now())`, ids.New("chb"), t1, inst, corp, a, "成员"+a, s); err != nil {
				t.Fatal(err)
			}
		}
		end1 := at(20, 7, 30)
		clock.set(end1.Add(2*time.Minute), 0)
		for _, who := range []string{bd2, expert} {
			if _, err := briefings.Compose(svcCh(t1), who, end1.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		pump(t)
		type card struct {
			c      chapi.Card
			signal string
		}
		button := func(c chapi.Card, key string) chapi.Button {
			for _, b := range c.Buttons {
				if b.Key == key {
					return b
				}
			}
			return chapi.Button{}
		}
		signalOfCard := func(c chapi.Card) string {
			u, _ := url.Parse(button(c, "view").URL)
			sum := sha256.Sum256([]byte(u.Query().Get("link")))
			var target string
			_ = chnOwner.QueryRow(ctx, `SELECT target FROM chn_briefing_links WHERE token_digest = encode($1::bytea, 'hex')`, sum[:]).Scan(&target)
			return target
		}
		cardsOf := func(a string) []card {
			var out []card
			for _, m := range wc.Messages(corp, a) {
				if m.Kind == "signal_card" && m.Card != nil {
					out = append(out, card{*m.Card, signalOfCard(*m.Card)})
				}
			}
			return out
		}
		bdCards, exCards := cardsOf(account[bd2]), cardsOf(account[expert])
		if len(bdCards) == 0 || len(exCards) == 0 {
			t.Fatalf("%d %d", len(bdCards), len(exCards))
		}
		for _, c := range exCards {
			if button(c.c, "pursue").Key != "" {
				t.Fatalf("业务专家（无 can_assign）不应有“跟进”：%+v", c.c.Buttons)
			}
		}
		// bd2 的卡片：已转跟进的信号仍显示“跟进”（关联到已有跟进）；选一张尚未跟进的
		var target card
		for _, c := range bdCards {
			if button(c.c, "pursue").Token == "" {
				t.Fatalf("开启后跟进候选的卡片应有“跟进”：%+v", c.c.Buttons)
			}
			if c.signal == s3 {
				target = c
			}
		}
		if target.signal == "" {
			var got []string
			for _, c := range bdCards {
				got = append(got, c.signal)
			}
			t.Fatalf("简报前 3 条中没有 s3：%v", got)
		}
		tok := button(target.c, "pursue").Token
		var evSeq atomic.Int64
		click := func(t *testing.T, accountID, token string, want chapi.InboundOutcome) {
			t.Helper()
			ev := fmt.Sprintf("pur-%s-%d", suffix, evSeq.Add(1))
			gw := reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: "svc:gateway", Kind: "service"}})
			res, err := callbacks.ReceiveInbound(gw, "wecom", wc.Sign(&chapi.Callback{CorpID: corp, EventID: ev, Timestamp: clock.Now(), Nonce: "n",
				Body: wecomtest.ActionBody(accountID, target.c.CardID, token)}))
			if err != nil || res.Outcome != want {
				t.Fatalf("%+v %v", res, err)
			}
			if _, err := briefings.ProcessActions(svcCh(t1), 50); err != nil {
				t.Fatal(err)
			}
			pump(t)
		}
		tokenID := func(token string) string {
			raw, _ := base64.RawURLEncoding.DecodeString(token)
			var claims struct {
				ID string `json:"i"`
			}
			_ = json.Unmarshal(raw[:len(raw)-32], &claims)
			return claims.ID
		}
		// 非收件人（bd）点击 bd2 的卡片：拒绝，不执行、不消耗令牌
		click(t, account[bd], tok, chapi.InboundQueued)
		var state string
		var attempts int
		if err := chnOwner.QueryRow(ctx, `SELECT state, attempts FROM chn_action_tokens WHERE id = $1`, tokenID(tok)).Scan(&state, &attempts); err != nil || state != "open" || attempts != 0 {
			t.Fatalf("%s %d %v", state, attempts, err)
		}
		if n := count(t, purOwner, `SELECT count(*) FROM pur_pursuits WHERE source_signal_id = $1`, s3); n != 0 {
			t.Fatal("非收件人的点击不得创建")
		}
		// 收件人点击：以 bd2 为负责人创建，渠道为企业微信；结果不带“撤销”
		clock.set(end1.Add(10*time.Minute), 0)
		click(t, account[bd2], tok, chapi.InboundQueued)
		up := wc.Updates(corp, account[bd2])
		last := up[len(up)-1]
		if !strings.HasPrefix(last.Text, "已创建跟进") || last.Card == nil || button(*last.Card, "undo").Key != "" {
			t.Fatalf("%+v", last)
		}
		var owner, channel, id string
		if err := purOwner.QueryRow(ctx, `SELECT id, owner, channel FROM pur_pursuits WHERE source_signal_id = $1`, s3).Scan(&id, &owner, &channel); err != nil || owner != bd2 || channel != "wecom" {
			t.Fatalf("%s %s %v", owner, channel, err)
		}
		exec := eventsFor("channels.action.executed.v1", "operation_id", tokenID(tok))
		if len(exec) != 1 || exec[0]["action"] != "pursue" || exec[0]["result"] != "succeeded" || exec[0]["pursuit_id"] != id {
			t.Fatalf("%+v", exec)
		}
		if row := signalRow(t, dm, s3); row["status"] != "converted" {
			t.Fatalf("%+v", row)
		}
		// 再点：返回已固定的结果，不再执行
		click(t, account[bd2], tok, chapi.InboundQueued)
		up = wc.Updates(corp, account[bd2])
		if !strings.Contains(up[len(up)-1].Text, "（已于 ") || len(eventsFor("channels.action.executed.v1", "operation_id", tokenID(tok))) != 1 {
			t.Fatalf("%+v", up[len(up)-1])
		}
		// 卡片创建的跟进在 Web 中由创建者 10 分钟内可撤销（卡片上不提供）
		if d := detail(t, bd2, id); d["viewer_can_void"] != true || d["source"].(map[string]any)["channel"] != "wecom" {
			t.Fatalf("%+v", d)
		}
		drained(t)
	})

	run("用例27、58，越权与反例测试（成员退出与重新加入、目标参与人移除）：负责人退出后跟进标“无有效负责人”并置顶、提醒转给决策者；重新加入不恢复；移出目标参与人后协作者移出；同步超过 24 小时只允许决策者改派", func(t *testing.T) {
		// bd2 负责一个跟进（bd 为协作者），带一项到期任务
		res := create(t, bd, fromSignal(t, bd, s6, fmt.Sprintf(`,"owner":%q,"collaborators":[%q],"first_task":{"title":"拜访","due":{"value":"2026-10-23","precision":"day","timezone":"Asia/Shanghai"}}`, bd2, expert)), 200)
		p := pursuitOf(res)
		drained(t)
		putMember(t, bd2, "removed", `[]`)
		drained(t)
		d := detail(t, dm, p)
		if d["owner_valid"] != false || !strings.Contains(fmt.Sprint(d["timeline"]), "owner_invalid") {
			t.Fatalf("%+v", d)
		}
		team := obj(t, expect(t, dm, "GET", "/v1/pursuit/pursuits?view=team", "", 200))["items"].([]any)
		pinned := false
		for _, it := range team {
			m := it.(map[string]any)
			if m["owner_valid"] != false {
				break
			}
			if m["id"] == p {
				pinned = true
			}
		}
		if !pinned {
			t.Fatalf("无有效负责人应置顶：%+v", team)
		}
		if allowedOn(t, bd2, "can_view", "pursuit:"+p) {
			t.Fatal("退出成员的关系应撤回")
		}
		// 提醒转给决策者：没有收件人，escalated；决策者的待办中出现
		if n := runDue(t, at(22, 9, 1)); n != 1 {
			t.Fatalf("%d", n)
		}
		var task string
		if err := purOwner.QueryRow(ctx, `SELECT id FROM pur_tasks WHERE pursuit_id = $1`, p).Scan(&task); err != nil {
			t.Fatal(err)
		}
		due := eventsFor("pursuit.task.due.v1", "task_id", task)
		if len(due) != 1 || due[0]["escalated"] != true || due[0]["recipient"] != nil {
			t.Fatalf("%+v", due)
		}
		esc := obj(t, expect(t, dm, "GET", "/v1/pursuit/my-tasks", "", 200))["items"].([]any)
		found := false
		for _, it := range esc {
			m := it.(map[string]any)
			if m["task"].(map[string]any)["id"] == task && m["escalated"] == true {
				found = true
			}
		}
		if !found {
			t.Fatalf("%+v", esc)
		}
		// 重新加入：不自动恢复负责人
		putMember(t, bd2, "active", `["business_developer"]`)
		drained(t)
		if d := detail(t, dm, p); d["owner_valid"] != false {
			t.Fatalf("%+v", d)
		}
		// 目标参与人移除：expert 不再是参与人 → 协作者移出
		if !allowedOn(t, expert, "can_view", "pursuit:"+p) {
			t.Fatal("协作者关系应已同步")
		}
		setParticipants(t, bd, bd2, lead, admin)
		drained(t)
		d = detail(t, dm, p)
		if cs := d["collaborators"].([]any); len(cs) != 0 {
			t.Fatalf("移出参与人后协作者应移出：%+v", cs)
		}
		if allowedOn(t, expert, "collaborator", "pursuit:"+p) {
			t.Fatal("移出的协作者关系应撤回")
		}
		// 改派前被分派人须有访问：expert 已不是参与人 → 422，整个命令不写入
		v := versionOf(t, p)
		assign(t, dm, p, bd, []string{expert}, 422, "assignee_no_access")
		if versionOf(t, p) != v {
			t.Fatal("拒绝的改派写入了")
		}
		setParticipants(t, bd, bd2, lead, expert, admin)
		// 授权同步失败超过 24 小时：只允许决策者改派（已同步的负责人 lead 也不能）
		assign(t, dm, p, lead, nil, 200)
		drained(t)
		directory.fail.Store(true)
		assign(t, dm, p, lead, []string{bd}, 200)
		if _, err := purOwner.Exec(ctx, `UPDATE pur_pursuits SET sync_pending_since = $2 WHERE id = $1`, p, at(21, 10, 0).Add(-25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if d := detail(t, dm, p); d["authorization"].(map[string]any)["state"] != "failed" {
			t.Fatalf("%+v", d)
		}
		assign(t, lead, p, bd, nil, 403, "authorization_syncing")
		assign(t, dm, p, bd, nil, 200)
		directory.fail.Store(false)
		drained(t)
		if d := detail(t, dm, p); d["owner_valid"] != true || d["owner"] != bd || d["authorization"].(map[string]any)["state"] != "effective" {
			t.Fatalf("改派后恢复：%+v", d)
		}
	})

	run("用例58：同步前被分派人退出，identity 拒绝该成员：跟进标“无有效负责人”并记录拒绝", func(t *testing.T) {
		// s5 的跟进（bd 创建）改派给 lead，同步前 lead 退出
		var target string
		if err := purOwner.QueryRow(ctx, `SELECT id FROM pur_pursuits WHERE source_signal_id = $1 AND state = 'open'`, s5).Scan(&target); err != nil {
			t.Fatal(err)
		}
		drained(t)
		notified := len(eventsFor("pursuit.pursuit.assigned.v1", "assignee", lead))
		directory.fail.Store(true)
		assign(t, dm, target, lead, nil, 200)
		// 成员事件不经 pursuit 的成员变化消费者（只验证 identity 的拒绝路径）
		routesMu.Lock()
		saved := routes["identity.member.changed.v1"]
		routes["identity.member.changed.v1"] = saved[:1]
		routesMu.Unlock()
		putMember(t, lead, "removed", `[]`)
		routesMu.Lock()
		routes["identity.member.changed.v1"] = saved
		routesMu.Unlock()
		directory.fail.Store(false)
		drained(t)
		d := detail(t, dm, target)
		if d["owner_valid"] != false || !strings.Contains(fmt.Sprint(d["timeline"]), "authorization_rejected") {
			t.Fatalf("%+v", d)
		}
		if len(eventsFor("pursuit.pursuit.assigned.v1", "assignee", lead)) != notified {
			t.Fatal("被拒的负责人不得收到分派通知")
		}
		if audits(t, purOwner, "pursuit.authorization.rejected", target) != 1 {
			t.Fatal("拒绝审计")
		}
		putMember(t, lead, "active", `["pursuit_lead"]`)
		drained(t)
	})

	run("用例38、39、40、41、42、14：关闭校验；关闭取消提醒、不改信号；learning 计入时点评估；修订追加记录；关闭后可再建并引用上一个", func(t *testing.T) {
		v := versionOf(t, p1)
		closeBody := func(outcome string) string {
			return fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"outcome":%s}`, opID("close"), v, outcome)
		}
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p1+"/close", closeBody(`{"result":"success","reason_code":"PRICE_COMPETITIVE"}`), 422, "invalid_outcome")
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p1+"/close", closeBody(`{"result":"success","reason_code":"TOO_SMALL","known_at":{"value":"2026-11-03","precision":"day","timezone":"Asia/Shanghai"}}`), 422, "invalid_outcome")
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p1+"/close", closeBody(`{"result":"failure","reason_code":"OTHER","known_at":{"value":"2026-11-03","precision":"day","timezone":"Asia/Shanghai"}}`), 422, "invalid_outcome")
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p1+"/close", closeBody(`{"result":"success","reason_code":"PRICE_COMPETITIVE","known_at":{"value":"2026-11-03","precision":"day","timezone":"Asia/Atlantis"}}`), 422, "invalid_time")
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p1+"/close", closeBody(`{"result":"success","reason_code":"PRICE_COMPETITIVE","known_at":{"value":"2026-11-03","precision":"day","timezone":"Asia/Shanghai"},"amounts":[{"type":"bogus","currency":"CNY","value":"1200000","nature":"estimate","source":"verbal"}]}`), 422, "invalid_outcome")
		// 研究分析等无编辑权的成员不能关闭
		expect(t, expert, "POST", "/v1/pursuit/pursuits/"+p1+"/close", closeBody(`{"result":"abandoned","reason_code":"TOO_SMALL"}`), 403, "forbidden")
		res := obj(t, expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p1+"/close", closeBody(`{"result":"success","reason_code":"EXISTING_RELATIONSHIP","known_at":{"value":"2026-11-03","precision":"day","timezone":"Asia/Shanghai"},"known_channel":"buyer_or_prime_notice","amounts":[{"type":"own_supply_amount","currency":"CNY","value":"1200000","nature":"estimate","source":"verbal"}]}`), 200))
		out := res["outcome"].(map[string]any)
		if out["contribution"] != "first_alert" || res["pursuit"].(map[string]any)["state"] != "closed" {
			t.Fatalf("%+v", res)
		}
		if n := count(t, purOwner, `SELECT count(*) FROM pur_reminders WHERE pursuit_id = $1 AND state = 'planned'`, p1); n != 0 {
			t.Fatalf("%d", n)
		}
		drained(t)
		if row := signalRow(t, dm, s1); row["status"] != "converted" {
			t.Fatalf("关闭不改变信号：%+v", row)
		}
		var basis, result string
		if err := lrns.owner.QueryRow(ctx, `SELECT basis, result FROM lrn_outcomes WHERE outcome_id = $1`, out["id"]).Scan(&basis, &result); err != nil || basis != "point_in_time" || result != "success" {
			t.Fatalf("%s %s %v", basis, result, err)
		}
		// 修订：估计 → 已发生，追加记录并引用旧记录
		rev := obj(t, expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p1+"/outcomes", fmt.Sprintf(`{"operation_id":%q,"supersedes":%q,"reason":"合同已签","outcome":{"result":"success","reason_code":"EXISTING_RELATIONSHIP","known_at":{"value":"2026-11-03","precision":"day","timezone":"Asia/Shanghai"},"amounts":[{"type":"own_supply_amount","currency":"CNY","value":"1180000","nature":"actual","source":"customer_document"}]}}`,
			opID("rev"), out["id"]), 200))
		if rev["outcome"].(map[string]any)["supersedes"] != out["id"] {
			t.Fatalf("%+v", rev)
		}
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p1+"/outcomes", fmt.Sprintf(`{"operation_id":%q,"supersedes":%q,"reason":"重复","outcome":{"result":"abandoned","reason_code":"TOO_SMALL"}}`,
			opID("rev"), out["id"]), 409, "conflict")
		drained(t)
		if d := detail(t, bd, p1); len(d["outcomes"].([]any)) != 2 {
			t.Fatalf("%+v", d["outcomes"])
		}
		if n := count(t, lrns.owner, `SELECT count(*) FROM lrn_outcomes WHERE pursuit_id = $1`, p1); n != 2 {
			t.Fatalf("%d", n)
		}
		// 获知时间缺失（放弃）：只进入事后统计
		ab := obj(t, expect(t, bd, "POST", "/v1/pursuit/pursuits/"+manual+"/close", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"outcome":{"result":"abandoned","reason_code":"TOO_SMALL"}}`, opID("close"), versionOf(t, manual)), 200))
		drained(t)
		if err := lrns.owner.QueryRow(ctx, `SELECT basis FROM lrn_outcomes WHERE outcome_id = $1`, ab["outcome"].(map[string]any)["id"]).Scan(&basis); err != nil || basis != "post_hoc" {
			t.Fatalf("%s %v", basis, err)
		}
		// 关闭后再建：引用上一个
		again := create(t, bd, fmt.Sprintf(`{"operation_id":%q,"objective_id":%q,"target":%s}`, opID("m"), objective.ID, targetOf(t, s2)), 200)
		if again["result"] != "created" {
			t.Fatalf("%+v", again)
		}
		if d := detail(t, bd, pursuitOf(again)); d["previous_pursuit_id"] != manual {
			t.Fatalf("%+v", d)
		}
		closed := obj(t, expect(t, bd, "GET", "/v1/pursuit/pursuits?view=closed", "", 200))["items"].([]any)
		if len(closed) != 2 {
			t.Fatalf("%+v", closed)
		}
		drained(t)
	})

	run("越权与反例测试 用例48、列表：他租户的跟进与任务标识查看、改派、完成、关闭都与不存在相同；篡改与他人的翻页游标 400", func(t *testing.T) {
		expectAs(t, admin2, t2, "GET", "/v1/pursuit/pursuits/"+p1, "", "", 404, "pursuit_not_found")
		expectAs(t, admin2, t2, "GET", "/v1/pursuit/tasks/"+p1Task, "", "", 404, "task_not_found")
		expectAs(t, admin2, t2, "POST", "/v1/pursuit/pursuits/"+p1+"/assign", fmt.Sprintf(`{"operation_id":%q,"expected_version":1,"owner":%q,"collaborators":[]}`, opID("x"), admin2), "", 404, "pursuit_not_found")
		expectAs(t, admin2, t2, "POST", "/v1/pursuit/tasks/"+p1Task+"/complete", fmt.Sprintf(`{"operation_id":%q,"expected_version":1}`, opID("x")), "", 404, "task_not_found")
		expectAs(t, admin2, t2, "POST", "/v1/pursuit/pursuits/"+p1+"/close", fmt.Sprintf(`{"operation_id":%q,"expected_version":1,"outcome":{"result":"abandoned","reason_code":"TOO_SMALL"}}`, opID("x")), "", 404, "pursuit_not_found")
		if items := obj(t, expectAs(t, admin2, t2, "GET", "/v1/pursuit/pursuits?view=team", "", "", 200))["items"].([]any); len(items) != 0 {
			t.Fatalf("他租户看到 %d 个跟进", len(items))
		}
		// 翻页游标：limit=1 取得下一页游标；篡改、换人使用、换视图都 400
		page := obj(t, expect(t, dm, "GET", "/v1/pursuit/pursuits?view=team&limit=1", "", 200))
		cur, _ := page["next_cursor"].(string)
		if cur == "" || len(page["items"].([]any)) != 1 {
			t.Fatalf("%+v", page)
		}
		next := obj(t, expect(t, dm, "GET", "/v1/pursuit/pursuits?view=team&limit=1&cursor="+url.QueryEscape(cur), "", 200))
		if next["items"].([]any)[0].(map[string]any)["id"] == page["items"].([]any)[0].(map[string]any)["id"] {
			t.Fatal("下一页重复")
		}
		raw, _ := base64.RawURLEncoding.DecodeString(cur)
		raw[2] ^= 1
		expect(t, dm, "GET", "/v1/pursuit/pursuits?view=team&limit=1&cursor="+base64.RawURLEncoding.EncodeToString(raw), "", 400, "invalid_cursor")
		expect(t, dm2, "GET", "/v1/pursuit/pursuits?view=team&limit=1&cursor="+url.QueryEscape(cur), "", 400, "invalid_cursor")
		expect(t, dm, "GET", "/v1/pursuit/pursuits?view=mine&limit=1&cursor="+url.QueryEscape(cur), "", 400, "invalid_cursor")
		expect(t, dm, "GET", "/v1/pursuit/my-tasks?cursor="+url.QueryEscape(cur), "", 400, "invalid_cursor")
		// 服务主体不能调用用户接口；本地绑定的 Reminders 不接受用户主体
		uctx := reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01", Principal: reqctx.Principal{ID: bd, Kind: "user"}})
		if _, err := reminders.RunDue(uctx, at(30, 0, 0), 10); err == nil {
			t.Fatal("用户主体执行了提醒")
		}
		var pe *pursuitapi.Error
		req := &pursuitapi.CreatePursuitRequest{OperationID: purcontract.OperationID("op-svc-" + suffix), ObjectiveID: purcontract.ObjectiveID(objective.ID),
			Collaborators: []purcontract.Subject{}}
		if _, err := remoteActions.CreatePursuit(svc(t1), "svc-attempt-"+suffix, req); !errors.As(err, &pe) || pe.Status != 403 {
			t.Fatalf("服务主体经内部端口创建：%v", err)
		}
	})

	run("越权与反例测试（复审）：负责人 A→B→A 改派且同步失败时 identity 中仍是旧版本的 A 关系，A 在新参与版本同步前不能写入（决策者可以），同步后同一操作成功", func(t *testing.T) {
		sig := newSignal(t, pursueBid())[0]
		p := pursuitOf(create(t, bd, fromSignal(t, bd, sig, fmt.Sprintf(`,"owner":%q`, bd)), 200))
		drained(t)
		directory.fail.Store(true)
		assign(t, dm, p, bd2, nil, 200)
		assign(t, dm, p, bd, nil, 200)
		pump(t)
		var applied, desired int64
		if err := purOwner.QueryRow(ctx, `SELECT applied_version, participation_version FROM pur_pursuits WHERE id = $1`, p).Scan(&applied, &desired); err != nil {
			t.Fatal(err)
		}
		if applied != 1 || desired != 3 || !allowedOn(t, bd, "can_edit", "pursuit:"+p) {
			t.Fatalf("前提：identity 中仍是第1版的负责人关系（applied %d desired %d）", applied, desired)
		}
		// 决策者（单元关系）不受同步状态影响
		addTask(t, dm, p, `{"title":"决策者安排"}`, 200)
		v := versionOf(t, p)
		op := opID("aba")
		body := fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"task":{"title":"A 同步前"}}`, op, v)
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p+"/tasks", body, 403, "authorization_syncing")
		assign(t, bd, p, bd, []string{expert}, 403, "authorization_syncing")
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p+"/close", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"outcome":{"result":"abandoned","reason_code":"TOO_SMALL"}}`, opID("aba"), v),
			403, "authorization_syncing")
		if versionOf(t, p) != v || count(t, purOwner, `SELECT count(*) FROM pur_operations WHERE operation_id = $1`, op) != 0 {
			t.Fatal("同步前的写入不得生效，也不得固定在操作上")
		}
		directory.fail.Store(false)
		drained(t)
		if res := obj(t, expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p+"/tasks", body, 200)); res["result"] != "succeeded" {
			t.Fatalf("%+v", res)
		}
	})

	run("越权与反例测试（复审）：授权同步失败超过 24 小时，只凭参与关系的负责人不能新增、修改、完成任务或撤销完成（决策者可以）；同步成功后恢复", func(t *testing.T) {
		sig := newSignal(t, pursueBid())[0]
		res := create(t, bd, fromSignal(t, bd, sig, fmt.Sprintf(`,"owner":%q,"first_task":{"title":"第一项"}`, bd)), 200)
		p := pursuitOf(res)
		drained(t)
		var first string
		if err := purOwner.QueryRow(ctx, `SELECT id FROM pur_tasks WHERE pursuit_id = $1`, p).Scan(&first); err != nil {
			t.Fatal(err)
		}
		completeOp := opID("done")
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+first+"/complete", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d}`, completeOp, taskVersion(t, first)), 200)
		second := addTask(t, bd, p, `{"title":"第二项"}`, 200)["task"].(map[string]any)["id"].(string)
		directory.fail.Store(true)
		assign(t, dm, p, bd, []string{expert}, 200)
		pump(t)
		// 24 小时内：负责人本人的关系已同步，仍可写入（只有新加入的协作者等待同步）
		if d := detail(t, dm, p); d["authorization"].(map[string]any)["state"] != "syncing" {
			t.Fatalf("%+v", d)
		}
		if _, err := purOwner.Exec(ctx, `UPDATE pur_pursuits SET sync_pending_since = $2 WHERE id = $1`, p, pclock.Now().Add(-25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if d := detail(t, dm, p); d["authorization"].(map[string]any)["state"] != "failed" {
			t.Fatalf("%+v", d)
		}
		v, tv := versionOf(t, p), taskVersion(t, second)
		addTask(t, bd, p, `{"title":"失败后"}`, 403, "authorization_syncing")
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+second+"/update", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"title":"改名"}`, opID("u"), tv), 403, "authorization_syncing")
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+second+"/complete", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d}`, opID("done"), tv), 403, "authorization_syncing")
		expect(t, bd, "POST", "/v1/pursuit/tasks/"+first+"/reopen", fmt.Sprintf(`{"operation_id":%q,"target_operation_id":%q}`, opID("re"), completeOp), 403, "authorization_syncing")
		expect(t, bd, "POST", "/v1/pursuit/pursuits/"+p+"/close", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"outcome":{"result":"abandoned","reason_code":"TOO_SMALL"}}`, opID("cl"), v),
			403, "authorization_syncing")
		if versionOf(t, p) != v || taskVersion(t, second) != tv {
			t.Fatal("被拒的写入生效了")
		}
		addTask(t, dm, p, `{"title":"决策者安排"}`, 200)
		directory.fail.Store(false)
		drained(t)
		addTask(t, bd, p, `{"title":"恢复后"}`, 200)
	})

	run("越权与反例测试（复审）：迟到或重放的成员退出事件（早于当前分派）不使重新加入后被重新分派的负责人失效；授予之后的退出仍判无效", func(t *testing.T) {
		sig := newSignal(t, pursueBid())[0]
		p := pursuitOf(create(t, bd, fromSignal(t, bd, sig, fmt.Sprintf(`,"owner":%q`, bd)), 200))
		drained(t)
		// 扣下 bd2 的退出事件（pursuit 的成员变化消费者不收到）
		routesMu.Lock()
		saved := routes["identity.member.changed.v1"]
		routes["identity.member.changed.v1"] = saved[:1]
		routesMu.Unlock()
		putMember(t, bd2, "removed", `[]`)
		routesMu.Lock()
		routes["identity.member.changed.v1"] = saved
		routesMu.Unlock()
		var held outbox.Envelope
		for _, e := range delivered {
			var d struct{ Subject, State string }
			if e.Type == "identity.member.changed.v1" && json.Unmarshal(e.Data, &d) == nil && d.Subject == bd2 && d.State == "removed" {
				held = e
			}
		}
		if held.ID == "" {
			t.Fatal("没有扣下退出事件")
		}
		putMember(t, bd2, "active", `["business_developer"]`)
		// 恢复目标访问（退出时 identity 撤回了其关系；参与人列表先去掉再加回）
		setParticipants(t, bd, lead, expert, admin)
		setParticipants(t, bd, bd2, lead, expert, admin)
		drained(t)
		signalRow(t, bd2, sig)
		// 重新加入并恢复目标访问后，决策者改派给 bd2、改走再改回（有效分派）
		assign(t, dm, p, bd2, nil, 200)
		assign(t, dm, p, bd, nil, 200)
		assign(t, dm, p, bd2, nil, 200)
		drained(t)
		v := versionOf(t, p)
		purRoute := saved[len(saved)-1]
		if err := handle(t, purRoute, held); err != nil {
			t.Fatal(err)
		}
		drained(t)
		if d := detail(t, dm, p); d["owner"] != bd2 || d["owner_valid"] != true || versionOf(t, p) != v {
			t.Fatalf("旧的退出事件使当前负责人失效：%+v", d)
		}
		if !allowedOn(t, bd2, "can_edit", "pursuit:"+p) {
			t.Fatal("旧的退出事件撤回了当前负责人的关系")
		}
		addTask(t, bd2, p, `{"title":"仍是负责人"}`, 200)
		// 授予之后的退出仍判无效，重新加入不恢复
		putMember(t, bd2, "removed", `[]`)
		drained(t)
		putMember(t, bd2, "active", `["business_developer"]`)
		drained(t)
		if d := detail(t, dm, p); d["owner_valid"] != false {
			t.Fatalf("授予之后的退出应判无效：%+v", d)
		}
	})

	run("越权与反例测试（复审二）：成员退出、目标变化事件迟到期间重新加入，决策者把负责人改为协作者（及协作者改为负责人）时重新判断并记下此刻的成员版本，之后送达的旧事件不撤销这次分派；带团队的“已存在”、关闭与修订不改授予", func(t *testing.T) {
		grantsOf := func(t *testing.T, id string) string {
			t.Helper()
			var g string
			if err := purOwner.QueryRow(ctx, `SELECT grants::text FROM pur_pursuits WHERE id = $1`, id).Scan(&g); err != nil {
				t.Fatal(err)
			}
			return g
		}
		// 上一用例中 bd2 退出后重新加入：先恢复其目标访问
		setParticipants(t, bd, lead, expert, admin)
		setParticipants(t, bd, bd2, lead, expert, admin)
		drained(t)
		sigP, sigQ := newSignal(t, pursueBid())[0], newSignal(t, pursueBid())[0]
		p := pursuitOf(create(t, bd, fromSignal(t, bd, sigP, fmt.Sprintf(`,"owner":%q`, bd2)), 200))                          // bd2 负责人 → 改为协作者
		q := pursuitOf(create(t, bd, fromSignal(t, bd, sigQ, fmt.Sprintf(`,"owner":%q,"collaborators":[%q]`, bd, bd2)), 200)) // bd2 协作者 → 改为负责人
		drained(t)
		// 带负责人与协作者的“已存在”不改团队与授予
		before := grantsOf(t, p)
		if res := create(t, bd, fromSignal(t, bd, sigP, fmt.Sprintf(`,"owner":%q,"collaborators":[%q]`, bd, expert)), 200); res["result"] != "existing" {
			t.Fatalf("%+v", res)
		}
		if grantsOf(t, p) != before {
			t.Fatal("“已存在”改了授予")
		}
		// pursuit 暂不消费成员与目标事件：bd2 退出、重新加入、恢复目标访问
		start := len(delivered)
		routesMu.Lock()
		savedM, savedO := routes["identity.member.changed.v1"], routes["customer_context.objective.changed.v1"]
		routes["identity.member.changed.v1"], routes["customer_context.objective.changed.v1"] = savedM[:1], savedO[:1]
		routesMu.Unlock()
		putMember(t, bd2, "removed", `[]`)
		putMember(t, bd2, "active", `["business_developer"]`)
		setParticipants(t, bd, lead, expert, admin)
		setParticipants(t, bd, bd2, lead, expert, admin)
		drained(t)
		routesMu.Lock()
		routes["identity.member.changed.v1"], routes["customer_context.objective.changed.v1"] = savedM, savedO
		routesMu.Unlock()
		var held []outbox.Envelope
		for _, e := range delivered[start:] {
			if e.Type == "identity.member.changed.v1" || e.Type == "customer_context.objective.changed.v1" {
				held = append(held, e)
			}
		}
		if len(held) < 3 {
			t.Fatalf("扣下的事件 %d", len(held))
		}
		// 决策者明确改派：p 的负责人改为 bd、bd2 为协作者；q 的负责人改为 bd2、bd 为协作者
		assign(t, dm, p, bd, []string{bd2}, 200)
		assign(t, dm, q, bd2, []string{bd}, 200)
		drained(t)
		vp, vq := versionOf(t, p), versionOf(t, q)
		// 迟到的事件按原顺序送达 pursuit
		for _, e := range held {
			r := savedM[len(savedM)-1]
			if e.Type == "customer_context.objective.changed.v1" {
				r = savedO[len(savedO)-1]
			}
			if err := handle(t, r, e); err != nil {
				t.Fatal(err)
			}
		}
		drained(t)
		if d := detail(t, dm, p); fmt.Sprint(d["collaborators"]) != fmt.Sprint([]any{bd2}) || versionOf(t, p) != vp {
			t.Fatalf("旧事件撤销了改为协作者的分派：%+v", d["collaborators"])
		}
		if d := detail(t, dm, q); d["owner"] != bd2 || d["owner_valid"] != true || versionOf(t, q) != vq {
			t.Fatalf("旧事件撤销了改为负责人的分派：%+v", d)
		}
		if !allowedOn(t, bd2, "collaborator", "pursuit:"+p) || !allowedOn(t, bd2, "can_edit", "pursuit:"+q) {
			t.Fatal("旧事件撤回了新分派的关系")
		}
		addTask(t, bd2, q, `{"title":"新负责人"}`, 200)
		// 关闭与修订结果不改授予
		before = grantsOf(t, q)
		res := obj(t, expect(t, bd2, "POST", "/v1/pursuit/pursuits/"+q+"/close", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"outcome":{"result":"abandoned","reason_code":"TOO_SMALL"}}`, opID("cl"), versionOf(t, q)), 200))
		outcome := res["outcome"].(map[string]any)["id"].(string)
		expect(t, bd2, "POST", "/v1/pursuit/pursuits/"+q+"/outcomes", fmt.Sprintf(`{"operation_id":%q,"supersedes":%q,"outcome":{"result":"abandoned","reason_code":"TOO_SMALL"},"reason":"复核"}`, opID("rv"), outcome), 200)
		drained(t)
		if grantsOf(t, q) != before {
			t.Fatal("关闭或修订改了授予")
		}
		// 授予之后的退出仍判无效（两处都移出 / 无效）
		putMember(t, bd2, "removed", `[]`)
		drained(t)
		if d := detail(t, dm, p); len(d["collaborators"].([]any)) != 0 {
			t.Fatalf("授予之后的退出应移出协作者：%+v", d["collaborators"])
		}
		putMember(t, bd2, "active", `["business_developer"]`)
		setParticipants(t, bd, lead, expert, admin)
		setParticipants(t, bd, bd2, lead, expert, admin)
		drained(t)
	})

	if len(pending) != 0 {
		t.Fatalf("结束时仍有 %d 个事件待重试", len(pending))
	}
}
