package integration_test

import (
	"context"
	"crypto/sha256"
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
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	lrn "github.com/superailabs/miaoqia/services/contracts/apis/learning"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api/datasettest"
)

// flakyApprovals 在 customer-context 与 identity 审批之间注入故障（identity 不可用）。
type flakyApprovals struct {
	idapi.Approvals
	failRequest, failClose atomic.Bool
}

func (f *flakyApprovals) RequestApproval(ctx context.Context, req *idapi.RequestApprovalRequest, key string) (*idapi.ApprovalRequest, error) {
	if f.failRequest.Load() {
		return nil, errors.New("注入：identity 不可用")
	}
	return f.Approvals.RequestApproval(ctx, req, key)
}

func (f *flakyApprovals) CloseApproval(ctx context.Context, id string, req *idapi.CloseApprovalRequest) (*idapi.ApprovalRequest, error) {
	if f.failClose.Load() {
		return nil, errors.New("注入：identity 不可用")
	}
	return f.Approvals.CloseApproval(ctx, id, req)
}

// flakyAuthz 注入授权决策失败（OpenFGA 不可用）。
type flakyAuthz struct {
	idapi.Authorizer
	fail atomic.Bool
}

func (f *flakyAuthz) Check(ctx context.Context, req authz.Request) (bool, error) {
	if f.fail.Load() {
		return false, errors.New("注入：授权决策不可用")
	}
	return f.Authorizer.Check(ctx, req)
}

// PRD 第7段服务端切片：真实 PostgreSQL（三个子系统各自的库、行级安全、应用账号）+ 真实 identity 目录与 OpenFGA +
// 外部网关令牌校验 + 生成的 HTTP 服务端；learning 使用标明为夹具的内存数据集（datasettest）。发件箱事件按契约校验后
// 直接交给消费处理（消费者与 Kafka 的投递由 kernel/consumer 自己的测试覆盖）。用例编号对应 PRD 第12节。
func TestPolicySimulationApproval(t *testing.T) {
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
	// 业务时钟（identity、customer-context、learning 共用）：可前移以验证审批时限
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }

	iam := testenv.NewDatabase(t)
	idModule := identity.ModuleWith(identity.Adapters{Now: now})
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", idModule.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2 := ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	user := func(role string) string { return role + "-" + suffix }
	admin, expert, dm, dm2, dm3, dm4, bd, bd2, ra, admin2 := user("admin"), user("expert"), user("dm"), user("dm2"), user("dm3"), user("dm4"), user("bd"),
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
		{`INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['domain_expert'],1), ($1,$4,'active',ARRAY['decision_maker'],1),
 ($1,$5,'active',ARRAY['decision_maker'],1), ($1,$6,'active',ARRAY['decision_maker'],1), ($1,$7,'active',ARRAY['decision_maker'],1),
 ($1,$8,'active',ARRAY['business_developer'],1), ($1,$9,'active',ARRAY['business_developer'],1), ($1,$10,'active',ARRAY['research_analyst'],1),
 ($11,$12,'active',ARRAY['admin','domain_expert','decision_maker'],1)`,
			[]any{t1, admin, expert, dm, dm2, dm3, dm4, bd, bd2, ra, t2, admin2}},
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
	localApprovals, _ := runtime.Lookup[idapi.Approvals](bindings, idapi.ApprovalsBinding)
	approvals := &flakyApprovals{Approvals: localApprovals}
	bindings.Provide(idapi.ApprovalsBinding, idapi.Approvals(approvals))
	localAuthz, _ := runtime.Lookup[idapi.Authorizer](bindings, idapi.AuthorizerBinding)
	decider := &flakyAuthz{Authorizer: localAuthz}
	bindings.Provide(idapi.AuthorizerBinding, idapi.Authorizer(decider))

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
	today := now().UTC().Truncate(24 * time.Hour)
	const org1 = "org_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	fixture := datasettest.New(today.AddDate(0, 0, -365))
	fixture.Add(datasettest.A02(today, org1)...)
	lrnModule := learning.ModuleWith(learning.Adapters{Dataset: fixture, Domains: fixture, Now: now})
	lrnEnv, lrnOwner := start(lrnModule)

	public, internal := http.NewServeMux(), http.NewServeMux()
	idModule.Routes(idEnv, public, internal)
	ccModule.Routes(ccEnv, public, internal)
	lrnModule.Routes(lrnEnv, public, internal)
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
	expectKey := func(t *testing.T, subject, tenant string, mfa bool, method, path, body, key string, status int, code ...string) []byte {
		t.Helper()
		got, raw := do(t, subject, tenant, mfa, method, path, body, key)
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

	// 发件箱泵：按各库的发件箱顺序读取新事件，按契约校验后交给订阅它的消费处理，直到没有新事件。
	type source struct {
		name  string
		owner *pgx.Conn
		last  int64
	}
	sources := []*source{{name: "identity", owner: iamOwner}, {name: "customer-context", owner: ccOwner}, {name: "learning", owner: lrnOwner}}
	type route struct {
		module   runtime.Module
		env      *runtime.Env
		consumer string
	}
	routes := map[string][]route{
		"identity.approval.requested.v1":     {{ccModule, ccEnv, "customer-context.policy-approvals"}},
		"identity.approval.decided.v1":       {{ccModule, ccEnv, "customer-context.policy-approvals"}},
		"identity.approval.closed.v1":        {{ccModule, ccEnv, "customer-context.policy-approvals"}},
		"customer_context.policy.drafted.v1": {{lrnModule, lrnEnv, "learning.policy-simulations"}},
		"learning.simulation.completed.v1":   {{ccModule, ccEnv, "customer-context.policy-simulations"}},
	}
	var delivered []outbox.Envelope // 已投递的事件（全部，按投递顺序）
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
	var pending []outbox.Envelope // 读出而暂不投递的事件（模拟延迟投递）
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
	data := func(e outbox.Envelope) map[string]any {
		var m map[string]any
		_ = json.Unmarshal(e.Data, &m)
		return m
	}
	lastEvent := func(t *testing.T, typ string) map[string]any {
		t.Helper()
		for i := len(delivered) - 1; i >= 0; i-- {
			if delivered[i].Type == typ {
				return data(delivered[i])
			}
		}
		t.Fatalf("没有事件 %s", typ)
		return nil
	}

	// 档案（我方主体 org1、品类与服务区域）与 A02 目标：决策者 dm 启用，业务开拓者 bd 参与
	content := fmt.Sprintf(`{"subject_type":"organization","entities":[{"org_id":"%s","qualifications_complete":true}],"value_chain_roles":["ROLE-MFR"],
"industry_packs":["IND-BUILDING-MATERIALS"],"offering_categories":["CAT-PIPE-PE"],"brands":["示例品牌"],"service_regions":["330000"],"qualifications":[]}`, org1)
	expect(t, expert, false, "PUT", "/v1/customer-context/profile", `{"expected_version":0,"content":`+content+`}`, 200)
	var objective cc.Objective
	decode(t, expect(t, dm, false, "POST", "/v1/customer-context/objectives", `{"template_id":"APP-TENDER.A02","org_unit_id":"root","participants":["`+bd+`"]}`, 200), &objective)
	objPath := "/v1/customer-context/objectives/" + objective.ID
	policyPath := objPath + "/policy"
	getPolicy := func(t *testing.T, who string) cc.Policy {
		t.Helper()
		var p cc.Policy
		decode(t, expect(t, who, false, "GET", policyPath, "", 200), &p)
		return p
	}
	getRun := func(t *testing.T, who, id string) lrn.SimulationRun {
		t.Helper()
		var r lrn.SimulationRun
		decode(t, expect(t, who, false, "GET", "/v1/learning/simulations/"+id, "", 200), &r)
		return r
	}
	formOf := func(t *testing.T, p cc.Policy) map[string]any {
		t.Helper()
		d, ok := p.Draft.Get()
		var raw []byte
		if ok {
			raw, _ = json.Marshal(&d.Form)
		} else {
			a, _ := p.Active.Get()
			raw, _ = json.Marshal(&a.Form)
		}
		var m map[string]any
		decode(t, raw, &m)
		return m
	}
	saveDraft := func(t *testing.T, who string, expected int64, form map[string]any, status int, code ...string) cc.Policy {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"expected_version": expected, "form": form})
		raw := expect(t, who, false, "PUT", policyPath+"/draft", string(body), status, code...)
		var p cc.Policy
		if status == 200 {
			decode(t, raw, &p)
		}
		return p
	}
	activation := func(p cc.Policy, note string, backfill ...int) string {
		d := p.Draft.Or(cc.PolicyDraft{})
		sim := d.Simulation.Or(cc.PolicySimulationRef{})
		m := map[string]any{"expected_version": p.DraftVersion, "revision": d.Revision, "simulation_id": sim.SimulationID, "note": note}
		if len(backfill) > 0 {
			m["backfill_days"] = backfill[0]
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	getApproval := func(t *testing.T, who, id string) identityapi.ApprovalRequest {
		t.Helper()
		var a identityapi.ApprovalRequest
		decode(t, expect(t, who, false, "GET", "/v1/identity/approvals/"+id, "", 200), &a)
		return a
	}
	decide := func(t *testing.T, who string, mfa bool, id, decision, reason string, status int, code ...string) []byte {
		t.Helper()
		a := getApproval(t, admin, id)
		body := fmt.Sprintf(`{"expected_version":%d,"decision":"%s"`, a.Version, decision)
		if reason != "" {
			body += fmt.Sprintf(`,"reason":"%s"`, reason)
		}
		return expect(t, who, mfa, "POST", "/v1/identity/approvals/"+id+"/decide", body+"}", status, code...)
	}
	request := func(t *testing.T, who string, body string, status int, code ...string) cc.PolicyRequest {
		t.Helper()
		raw := expect(t, who, true, "POST", policyPath+"/activation-requests", body, status, code...)
		var r cc.PolicyRequest
		if status == 200 {
			decode(t, raw, &r)
		}
		return r
	}
	member := func(t *testing.T, subject string) int64 {
		t.Helper()
		var s struct {
			MemberVersion int64 `json:"member_version"`
		}
		decode(t, expect(t, admin, true, "GET", "/v1/identity/members/"+subject, "", 200), &s)
		return s.MemberVersion
	}

	var firstRun lrn.SimulationRun
	t.Run("用例1、23：启用即有首版草案并自动仿真；首次激活前没有对比一侧，报告标明候选变更仿真与夹具数据", func(t *testing.T) {
		if objective.State != "inactive" {
			t.Fatalf("%+v", objective)
		}
		pump(t)
		p := getPolicy(t, dm)
		d, ok := p.Draft.Get()
		if !ok || p.Generation != 0 || p.ActiveVersion != 0 || p.DraftVersion != 1 || d.Revision != 1 || !d.Valid || len(d.ExampleMismatches) != 0 || d.SavedBy != cc.Subject(dm) {
			t.Fatalf("%+v", p)
		}
		sim, ok := d.Simulation.Get()
		if !ok || sim.Status != "completed" || sim.Stale || sim.CompareVersion != 0 || len(sim.EnabledDomains) != 4 {
			t.Fatalf("%+v", d.Simulation)
		}
		firstRun = getRun(t, dm, sim.SimulationID)
		in, _ := firstRun.Inputs.Get()
		rep, _ := firstRun.Report.Get()
		if in.Purpose != "counterfactual_candidate" || !in.DatasetFixture || in.WindowDays != 90 || in.CompareVersion != 0 || in.Revision != 1 ||
			!in.WindowEnd.Equal(today) || len(in.ExcludedDomains) != 0 || rep.CompareDistribution.Set || rep.Diff.Set {
			t.Fatalf("%+v", firstRun)
		}
		// 夹具预期（datasettest.A02）：窗口内 10 条事实（FX-A02-11 在窗口外）
		dist := rep.Distribution
		if rep.Evaluated != 10 || dist.PursueCandidate != 1 || dist.ReviewCandidate != 6 || dist.WatchSignal != 1 || dist.Excluded != 1 || dist.NoSignal != 1 {
			t.Fatalf("%+v", rep)
		}
		if len(rep.Samples) != 8 || rep.Samples[0].FactID != "FX-A02-01" || rep.Samples[0].Draft != "pursue_candidate" {
			t.Fatalf("%+v", rep.Samples)
		}
		hints := map[string]bool{}
		for _, h := range rep.Hints {
			hints[string(h.Code)] = true
		}
		if !hints["review_majority"] || hints["zero_results"] || hints["coverage_insufficient"] || hints["fact_types_not_connected"] {
			t.Fatalf("%+v", rep.Hints)
		}
		if objective, err := json.Marshal(&firstRun); err != nil || !strings.Contains(string(objective), `"weekly"`) {
			t.Fatal(err)
		}
	})

	t.Run("用例9：只用逻辑求值时间之前已可得的版本，之后才发布的更正不参与", func(t *testing.T) {
		rep, _ := firstRun.Report.Get()
		for _, s := range rep.Samples {
			if s.FactID == "FX-A02-08" {
				if s.AmountCny.Or(0) != 2_000_000 || s.Draft != "review_candidate" {
					t.Fatalf("%+v", s)
				}
				return
			}
		}
		t.Fatal("缺少 FX-A02-08")
	})

	t.Run("逐条规则：未知比例与原因（字段缺失、低于证据门槛）", func(t *testing.T) {
		rep, _ := firstRun.Report.Get()
		stats := map[string]lrn.RuleStats{}
		for _, r := range rep.Rules {
			stats[r.ID] = r
		}
		reasons := func(id string) map[string]int {
			out := map[string]int{}
			for _, u := range stats[id].UnknownReasons {
				out[string(u.Reason)] = u.Count
			}
			return out
		}
		if reasons("R-CAT")["field_missing"] != 1 || reasons("R-WINNER")["below_evidence_threshold"] != 1 || reasons("R-WINNER")["field_missing"] != 1 {
			t.Fatalf("%+v %+v", stats["R-CAT"], stats["R-WINNER"])
		}
		if len(rep.TopUnknownRules) != 5 || !slices.Contains(rep.TopUnknownRules, "R-WINNER") || !slices.Contains(rep.TopUnknownRules, "R-AMOUNT") {
			t.Fatalf("%v", rep.TopUnknownRules)
		}
		for _, r := range rep.Rules {
			if sum := r.TrueRatio + r.FalseRatio + r.UnknownRatio; sum < 0.999 || sum > 1.001 {
				t.Fatalf("%+v", r)
			}
		}
	})

	t.Run("用例10：相同修订与输入复用同一运行，结果确定", func(t *testing.T) {
		var again lrn.SimulationRun
		decode(t, expect(t, dm, false, "POST", "/v1/learning/simulations", fmt.Sprintf(`{"objective_id":"%s","revision":1}`, objective.ID), 200), &again)
		a, _ := json.Marshal(&firstRun)
		b, _ := json.Marshal(&again)
		if again.ID != firstRun.ID || string(a) != string(b) {
			t.Fatalf("%s\n%s", a, b)
		}
		var n int
		if err := lrnOwner.QueryRow(ctx, `SELECT count(*) FROM lrn_simulation_runs WHERE objective_id = $1 AND revision = 1`, objective.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%d %v", n, err)
		}
	})

	t.Run("报告与仿真的权限：查看为 can_view，发起为 can_draft_policy；无权与不存在同样 404", func(t *testing.T) {
		getRun(t, ra, string(firstRun.ID))
		getRun(t, bd, string(firstRun.ID))
		expect(t, bd2, false, "GET", "/v1/learning/simulations/"+string(firstRun.ID), "", 404, "simulation_not_found")
		expectKey(t, admin2, t2, false, "GET", "/v1/learning/simulations/"+string(firstRun.ID), "", "", 404, "simulation_not_found")
		expect(t, dm, false, "GET", "/v1/learning/simulations/sim_01JA8ZXXXXXXXXXXXXXXXXXXXX", "", 404, "simulation_not_found")
		body := fmt.Sprintf(`{"objective_id":"%s","revision":1}`, objective.ID)
		expect(t, ra, false, "POST", "/v1/learning/simulations", body, 403, "forbidden")
		expect(t, bd2, false, "POST", "/v1/learning/simulations", body, 404, "simulation_target_not_found")
		expect(t, dm, false, "POST", "/v1/learning/simulations", fmt.Sprintf(`{"objective_id":"%s","revision":99}`, objective.ID), 404, "simulation_target_not_found")
	})

	t.Run("用例3：草案只由决策者与参与目标的业务开拓者编辑；表单按模板约束在服务端校验", func(t *testing.T) {
		p := getPolicy(t, dm)
		form := formOf(t, p)
		saveDraft(t, bd2, p.DraftVersion, form, 404, "objective_not_found")
		saveDraft(t, ra, p.DraftVersion, form, 403, "forbidden")
		bad := func(rules []map[string]any, status int, code string) {
			t.Helper()
			f := formOf(t, p)
			f["rules"] = rules
			saveDraft(t, dm, p.DraftVersion, f, status, code)
		}
		bad([]map[string]any{{"id": "R-NOPE", "enabled": true}}, 422, "unknown_rule")
		bad([]map[string]any{{"id": "R-CAT", "enabled": true, "weight": 10}}, 422, "invalid_weight")
		bad([]map[string]any{{"id": "B-LARGE", "enabled": true, "weight": 51}}, 400, "invalid_request")
		f := formOf(t, p)
		f["notify"] = map[string]any{"pursue_candidate": map[string]any{"audience": []string{"role:admin"}, "mode": "immediate"}}
		saveDraft(t, dm, p.DraftVersion, f, 400, "invalid_request")
		if after := getPolicy(t, dm); after.DraftVersion != p.DraftVersion {
			t.Fatalf("被拒绝的保存改变了草案：%+v", after)
		}
	})

	t.Run("用例6：两人同时保存草案，后提交者版本冲突，不覆盖前者", func(t *testing.T) {
		p := getPolicy(t, dm)
		form := formOf(t, p)
		params := form["params"].(map[string]any)
		params["large_amount_cny"] = map[string]any{"source": "objective", "number": 10_000_000}
		form["rules"] = []map[string]any{{"id": "B-BUYS-CATEGORY", "enabled": true, "weight": 20}}
		other := formOf(t, p)
		other["thresholds"] = map[string]any{"base_score": 50, "pursue_at": 65, "drop_below": 20}
		gate := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, f := range []map[string]any{form, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body, _ := json.Marshal(map[string]any{"expected_version": p.DraftVersion, "form": f})
				who := []string{dm, bd}[i]
				<-gate
				codes[i], _ = do(t, who, t1, false, "PUT", policyPath+"/draft", string(body), "")
			}()
		}
		close(gate)
		wg.Wait()
		slices.Sort(codes)
		if codes[0] != 200 || codes[1] != 409 {
			t.Fatalf("%v", codes)
		}
		after := getPolicy(t, dm)
		if after.DraftVersion != p.DraftVersion+1 {
			t.Fatalf("%+v", after)
		}
		// 以确定的内容再保存一次，后续用例使用它
		saveDraft(t, dm, after.DraftVersion, form, 200)
		saveDraft(t, bd, after.DraftVersion, form, 409, "version_conflict")
	})

	t.Run("用例5：未通过校验的草案照常保存，不能仿真与申请", func(t *testing.T) {
		p := getPolicy(t, dm)
		form := formOf(t, p)
		form["thresholds"] = map[string]any{"base_score": 50, "pursue_at": 60, "drop_below": 70}
		saved := saveDraft(t, dm, p.DraftVersion, form, 200)
		d, _ := saved.Draft.Get()
		if d.Valid || len(d.Issues) == 0 {
			t.Fatalf("%+v", d)
		}
		pump(t)
		if d2, _ := getPolicy(t, dm).Draft.Get(); d2.Simulation.Set {
			t.Fatalf("未通过校验的修订被仿真：%+v", d2.Simulation)
		}
		expect(t, dm, false, "POST", "/v1/learning/simulations", fmt.Sprintf(`{"objective_id":"%s","revision":%d}`, objective.ID, d.Revision), 409, "policy_invalid")
		body := fmt.Sprintf(`{"expected_version":%d,"revision":%d,"simulation_id":"%s","note":"试"}`, saved.DraftVersion, d.Revision, firstRun.ID)
		request(t, dm, body, 409, "draft_invalid")
		form["thresholds"] = map[string]any{"base_score": 50, "pursue_at": 60, "drop_below": 20}
		saveDraft(t, dm, saved.DraftVersion, form, 200)
		pump(t)
	})

	var first cc.PolicyRequest
	var firstKey string
	t.Run("用例4、7、12：申请核对修订与报告，要求强认证；同一幂等键只产生一个请求", func(t *testing.T) {
		p := getPolicy(t, bd)
		d, _ := p.Draft.Get()
		sim, ok := d.Simulation.Get()
		if !ok || sim.Status != "completed" || d.Revision < 4 {
			t.Fatalf("%+v", d)
		}
		stale := fmt.Sprintf(`{"expected_version":%d,"revision":%d,"simulation_id":"%s","note":"试"}`, p.DraftVersion, d.Revision, firstRun.ID)
		request(t, bd, stale, 409, "simulation_not_ready")
		old := fmt.Sprintf(`{"expected_version":%d,"revision":1,"simulation_id":"%s","note":"试"}`, p.DraftVersion, firstRun.ID)
		request(t, bd, old, 409, "version_conflict")
		body := activation(p, "调整大额阈值与品类加分")
		expect(t, bd, false, "POST", policyPath+"/activation-requests", body, 403, "mfa_required")
		request(t, bd2, body, 404, "objective_not_found")
		expect(t, ra, true, "POST", policyPath+"/activation-requests", body, 403, "forbidden")
		request(t, bd, activation(p, "首次激活", 31), 400, "invalid_request")
		approvals.failRequest.Store(true)
		request(t, bd, body, 503, "identity_unavailable")
		approvals.failRequest.Store(false)
		if after := getPolicy(t, bd); after.Request.Set || after.Draft.Or(cc.PolicyDraft{}).LockedBy.Set {
			t.Fatalf("失败的申请留下了状态：%+v", after)
		}
		firstKey = "first-activation-key-0001"
		decode(t, expectKey(t, bd, t1, true, "POST", policyPath+"/activation-requests", body, firstKey, 200), &first)
		if first.State != "pending" || first.Kind != "activation" || first.BackfillDays.Or(-1) != 30 || first.ObjectVersion != d.Revision ||
			first.SimulationID != sim.SimulationID || first.BaselineVersion != 0 || first.RequestedBy != cc.Subject(bd) {
			t.Fatalf("%+v", first)
		}
		var again cc.PolicyRequest
		decode(t, expectKey(t, bd, t1, true, "POST", policyPath+"/activation-requests", body, firstKey, 200), &again)
		if again.ApprovalID != first.ApprovalID {
			t.Fatalf("%+v", again)
		}
		var list identityapi.ApprovalList
		decode(t, expect(t, dm, false, "GET", "/v1/identity/approvals", "", 200), &list)
		if len(list.Approvals) != 1 || string(list.Approvals[0].ID) != first.ApprovalID {
			t.Fatalf("%+v", list)
		}
		a := list.Approvals[0]
		if a.Binding.ObjectVersion != d.Revision || a.Binding.EvidenceRef != sim.SimulationID || a.Binding.BackfillDays.Or(-1) != 30 || a.Applicant != bd {
			t.Fatalf("%+v", a)
		}
		after := getPolicy(t, dm)
		if after.Draft.Or(cc.PolicyDraft{}).LockedBy.Or("") != first.ApprovalID || after.Request.Or(cc.PolicyRequest{}).ApprovalID != first.ApprovalID {
			t.Fatalf("%+v", after)
		}
		saveDraft(t, dm, after.DraftVersion, formOf(t, after), 409, "draft_locked")
		request(t, dm, activation(after, "再来"), 409, "draft_locked")
		expect(t, dm, false, "POST", policyPath+"/draft:discard", fmt.Sprintf(`{"expected_version":%d}`, after.DraftVersion), 409, "draft_locked")
		pump(t) // identity.approval.requested 的对账：已登记，不改变状态
		if again := getPolicy(t, dm); again.Request.Or(cc.PolicyRequest{}).State != "pending" {
			t.Fatalf("%+v", again)
		}
	})

	t.Run("用例2：申请人不能批准；无批准权的成员看不到请求、直接调用被拒绝；批准要求强认证", func(t *testing.T) {
		decide(t, bd, true, first.ApprovalID, "approve", "", 403, "self_approval_forbidden")
		decide(t, ra, true, first.ApprovalID, "approve", "", 404, "approval_not_found")
		decide(t, bd2, true, first.ApprovalID, "approve", "", 404, "approval_not_found")
		decide(t, dm, false, first.ApprovalID, "approve", "", 403, "mfa_required")
		var list identityapi.ApprovalList
		decode(t, expect(t, ra, false, "GET", "/v1/identity/approvals", "", 200), &list)
		if len(list.Approvals) != 0 {
			t.Fatalf("%+v", list)
		}
		decode(t, expect(t, bd, false, "GET", "/v1/identity/approvals?role=approver", "", 200), &list)
		if len(list.Approvals) != 0 {
			t.Fatalf("申请人在待我审批中看到自己的请求：%+v", list)
		}
		decode(t, expect(t, bd, false, "GET", "/v1/identity/approvals?role=applicant", "", 200), &list)
		if len(list.Approvals) != 1 {
			t.Fatalf("%+v", list)
		}
	})

	t.Run("用例1：批准后执行激活：新版本生效、代次加一、目标生效、草案清除；重复投递不重复执行", func(t *testing.T) {
		decide(t, admin, true, first.ApprovalID, "approve", "", 200)
		decide(t, dm, true, first.ApprovalID, "approve", "", 409, "approval_not_pending")
		pump(t)
		p := getPolicy(t, dm)
		v, ok := p.Active.Get()
		if !ok || p.Generation != 1 || p.ActiveVersion != 1 || p.Draft.Set || v.Version != 1 || v.State != "active" || v.ApprovalID != first.ApprovalID ||
			v.RequestedBy != cc.Subject(bd) || v.ApprovedBy != cc.Subject(admin) || v.SimulationID != first.SimulationID || len(v.EnabledDomains) != 4 {
			t.Fatalf("%+v", p)
		}
		if r := p.Request.Or(cc.PolicyRequest{}); r.State != "executed" || r.DecidedBy.Or("") != cc.Subject(admin) {
			t.Fatalf("%+v", r)
		}
		var o cc.Objective
		decode(t, expect(t, dm, false, "GET", objPath, "", 200), &o)
		if o.State != "active" {
			t.Fatalf("%+v", o)
		}
		ev := lastEvent(t, "customer_context.policy.activated.v1")
		if ev["version"].(float64) != 1 || ev["generation"].(float64) != 1 || ev["backfill_days"].(float64) != 30 || ev["previous_version"].(float64) != 0 {
			t.Fatalf("%+v", ev)
		}
		for _, e := range delivered {
			if e.Type == "identity.approval.decided.v1" {
				deliver(t, e)
			}
		}
		if again := getPolicy(t, dm); again.Generation != 1 || again.ActiveVersion != 1 {
			t.Fatalf("重复投递重复执行：%+v", again)
		}
	})

	var second cc.Policy
	t.Run("用例7、8、25：新草案的仿真对比生效版本；停用硬条件高亮；规则级差异", func(t *testing.T) {
		p := getPolicy(t, dm)
		form := formOf(t, p)
		form["rules"] = []map[string]any{{"id": "B-BUYS-CATEGORY", "enabled": true, "weight": 20}, {"id": "R-REGION", "enabled": false}}
		saveDraft(t, dm, p.DraftVersion, form, 200)
		pump(t)
		second = getPolicy(t, dm)
		d, _ := second.Draft.Get()
		sim, _ := d.Simulation.Get()
		if d.BaseVersion != 1 || sim.CompareVersion != 1 || sim.CompareGeneration != 1 || sim.Stale {
			t.Fatalf("%+v", d)
		}
		run := getRun(t, dm, sim.SimulationID)
		rep, _ := run.Report.Get()
		diff, ok := rep.Diff.Get()
		if !ok || !rep.CompareDistribution.Set || diff.Added != 1 || diff.AddedSamples[0].FactID != "FX-A02-05" || diff.AddedSamples[0].Active.Or("") != "no_signal" {
			t.Fatalf("%+v", rep)
		}
		found := false
		for _, h := range rep.Hints {
			found = found || h.Code == "hard_condition_disabled"
		}
		if !found {
			t.Fatalf("%+v", rep.Hints)
		}
		var pd cc.PolicyDiff
		decode(t, expect(t, ra, false, "GET", policyPath+"/diff?from=v1&to=draft", "", 200), &pd)
		highlighted := false
		for _, r := range pd.Rules {
			if r.ID == "R-REGION" && r.Change == "disabled" && r.Highlight {
				highlighted = true
			}
		}
		if !highlighted || pd.From != "v1" || pd.To != "draft" {
			t.Fatalf("%+v", pd)
		}
		expect(t, dm, false, "GET", policyPath+"/diff?from=v9&to=draft", "", 404, "policy_version_not_found")
		expect(t, bd2, false, "GET", policyPath+"/diff?from=v1&to=draft", "", 404, "objective_not_found")
		request(t, dm, activation(second, "首次之后不能回填", 10), 422, "backfill_not_allowed")
	})

	t.Run("用例13：撤回、驳回后草案回到可编辑；驳回必须写原因", func(t *testing.T) {
		r := request(t, dm, activation(second, "停用区域条件"), 200)
		a := getApproval(t, dm, r.ApprovalID)
		expect(t, dm2, true, "POST", "/v1/identity/approvals/"+r.ApprovalID+"/withdraw", fmt.Sprintf(`{"expected_version":%d}`, a.Version), 403, "forbidden")
		expect(t, dm, false, "POST", "/v1/identity/approvals/"+r.ApprovalID+"/withdraw", fmt.Sprintf(`{"expected_version":%d}`, a.Version), 200)
		pump(t)
		p := getPolicy(t, dm)
		if p.Request.Or(cc.PolicyRequest{}).State != "withdrawn" || p.Draft.Or(cc.PolicyDraft{}).LockedBy.Set {
			t.Fatalf("%+v", p)
		}
		r = request(t, dm, activation(p, "停用区域条件（再次）"), 200)
		decide(t, dm2, true, r.ApprovalID, "reject", "", 400, "reason_required")
		decide(t, dm2, true, r.ApprovalID, "reject", "区域条件不能停用", 200)
		pump(t)
		p = getPolicy(t, dm)
		if q := p.Request.Or(cc.PolicyRequest{}); q.State != "rejected" || q.Reason.Or("") != "区域条件不能停用" || p.Draft.Or(cc.PolicyDraft{}).LockedBy.Set || p.ActiveVersion != 1 {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("用例13：申请人退出参与（失去申请关系）、离开租户、失去角色时请求失效，草案解锁", func(t *testing.T) {
		// 业务开拓者 bd 不再参与目标
		p := getPolicy(t, bd)
		r := request(t, bd, activation(p, "参与人申请"), 200)
		var o cc.Objective
		decode(t, expect(t, dm, false, "GET", objPath, "", 200), &o)
		params, _ := json.Marshal(o.Params)
		expect(t, dm, false, "PUT", objPath, fmt.Sprintf(`{"expected_version":%d,"owner":"%s","participants":[],"params":%s}`, o.Version, dm, params), 200)
		pump(t)
		if a := getApproval(t, admin, r.ApprovalID); a.State != "invalidated" || a.ClosedReason.Or("") != "applicant_lost_permission" {
			t.Fatalf("%+v", a)
		}
		p = getPolicy(t, dm)
		if q := p.Request.Or(cc.PolicyRequest{}); q.State != "invalidated" || p.Draft.Or(cc.PolicyDraft{}).LockedBy.Set {
			t.Fatalf("%+v", p)
		}
		// 决策者 dm3 离开租户
		r = request(t, dm3, activation(p, "离开前提交"), 200)
		expect(t, admin, true, "PUT", "/v1/identity/members/"+dm3, fmt.Sprintf(`{"state":"removed","roles":[],"expected_version":%d}`, member(t, dm3)), 200)
		pump(t)
		if a := getApproval(t, admin, r.ApprovalID); a.State != "invalidated" || a.ClosedReason.Or("") != "applicant_left" {
			t.Fatalf("%+v", a)
		}
		// 决策者 dm4 改为研究分析者
		p = getPolicy(t, dm)
		r = request(t, dm4, activation(p, "改角色前提交"), 200)
		expect(t, admin, true, "PUT", "/v1/identity/members/"+dm4, fmt.Sprintf(`{"state":"active","roles":["research_analyst"],"expected_version":%d}`, member(t, dm4)), 200)
		pump(t)
		if a := getApproval(t, admin, r.ApprovalID); a.State != "invalidated" || a.ClosedReason.Or("") != "applicant_lost_permission" {
			t.Fatalf("%+v", a)
		}
		if p = getPolicy(t, dm); p.Request.Or(cc.PolicyRequest{}).State != "invalidated" || p.Draft.Or(cc.PolicyDraft{}).LockedBy.Set {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("用例13：超过7天未决定自动失效（业务时间）", func(t *testing.T) {
		p := getPolicy(t, dm)
		r := request(t, dm, activation(p, "会过期"), 200)
		offset.Store(int64(8 * 24 * time.Hour))
		var list identityapi.ApprovalList
		decode(t, expect(t, admin, false, "GET", "/v1/identity/approvals", "", 200), &list)
		offset.Store(0)
		if len(list.Approvals) != 0 {
			t.Fatalf("%+v", list)
		}
		pump(t)
		a := getApproval(t, dm, r.ApprovalID)
		if a.State != "expired" || !a.ClosedAt.Or(time.Time{}).Equal(a.ExpiresAt) {
			t.Fatalf("%+v", a)
		}
		if p = getPolicy(t, dm); p.Request.Or(cc.PolicyRequest{}).State != "expired" || p.Draft.Or(cc.PolicyDraft{}).LockedBy.Set {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("用例14：批准后执行前基准已变则不执行，记为失败并通知；原生效版本不变", func(t *testing.T) {
		p := getPolicy(t, dm)
		r := request(t, dm, activation(p, "执行前基准会变"), 200)
		hold["identity.approval.decided.v1"] = true
		decide(t, dm2, true, r.ApprovalID, "approve", "", 200)
		pump(t)
		// 模拟决定与执行之间生效版本代次的并发变化（正常流程中由另一次激活或回滚造成，此处直接改写以构造竞态）
		if _, err := ccOwner.Exec(ctx, `UPDATE ctx_policies SET generation = generation + 1 WHERE objective_id = $1`, objective.ID); err != nil {
			t.Fatal(err)
		}
		delete(hold, "identity.approval.decided.v1")
		for _, e := range pending {
			delivered = append(delivered, e)
			deliver(t, e)
		}
		pending = nil
		pump(t)
		if _, err := ccOwner.Exec(ctx, `UPDATE ctx_policies SET generation = generation - 1 WHERE objective_id = $1`, objective.ID); err != nil {
			t.Fatal(err)
		}
		p = getPolicy(t, dm)
		if q := p.Request.Or(cc.PolicyRequest{}); q.State != "failed" || q.Reason.Or("") != "baseline_changed" || p.ActiveVersion != 1 || p.Draft.Or(cc.PolicyDraft{}).LockedBy.Set {
			t.Fatalf("%+v", p)
		}
		ev := lastEvent(t, "customer_context.policy.execution_failed.v1")
		if ev["approval_id"] != r.ApprovalID || ev["reason"] != "baseline_changed" || ev["kind"] != "activation" {
			t.Fatalf("%+v", ev)
		}
	})

	t.Run("激活第二版；进行中的激活申请期间不能提交回滚", func(t *testing.T) {
		p := getPolicy(t, dm)
		r := request(t, dm, activation(p, "第二版"), 200)
		decide(t, dm2, true, r.ApprovalID, "approve", "", 200)
		pump(t)
		p = getPolicy(t, dm)
		if p.ActiveVersion != 2 || p.Generation != 2 {
			t.Fatalf("%+v", p)
		}
		var list cc.PolicyVersionList
		decode(t, expect(t, ra, false, "GET", policyPath+"/versions", "", 200), &list)
		if len(list.Versions) != 2 || list.Versions[0].Version != 2 || list.Versions[0].State != "active" || list.Versions[1].State != "superseded" || list.Truncated {
			t.Fatalf("%+v", list)
		}
		decode(t, expect(t, ra, false, "GET", policyPath+"/versions?limit=1", "", 200), &list)
		if len(list.Versions) != 1 || !list.Truncated {
			t.Fatalf("%+v", list)
		}
		decode(t, expect(t, ra, false, "GET", policyPath+"/versions?limit=1&cursor="+string(list.NextCursor.Or("")), "", 200), &list)
		if len(list.Versions) != 1 || list.Versions[0].Version != 1 || list.Truncated {
			t.Fatalf("%+v", list)
		}
	})

	t.Run("用例8、17、25：领域状态变化只影响新仿真；回滚恢复原文档与原启用领域集合，不锁定草案，草案报告随之过期", func(t *testing.T) {
		fixture.SetStatus("INF-CRED", "not_live")
		p := getPolicy(t, dm)
		form := formOf(t, p)
		saveDraft(t, dm, p.DraftVersion, form, 200)
		pump(t)
		p = getPolicy(t, dm)
		d, _ := p.Draft.Get()
		sim, _ := d.Simulation.Get()
		run := getRun(t, dm, sim.SimulationID)
		in, _ := run.Inputs.Get()
		if slices.Contains(in.EnabledDomains, "INF-CRED") || len(in.ExcludedDomains) != 1 || in.ExcludedDomains[0].Status != "not_live" {
			t.Fatalf("%+v", in)
		}
		rep, _ := run.Report.Get()
		for _, r := range rep.Rules {
			if r.ID == "P-CREDIT" && (len(r.UnknownReasons) == 0 || r.UnknownReasons[0].Reason != "data_not_connected") {
				t.Fatalf("%+v", r)
			}
		}
		// 草案已有进行中的激活申请时不能提交回滚（一个目标同时只有一个进行中的申请）
		pendingActivation := request(t, dm, activation(p, "进行中"), 200)
		expect(t, dm, true, "POST", policyPath+"/rollback-requests", `{"target_version":1,"note":"回滚"}`, 409, "approval_in_progress")
		a := getApproval(t, dm, pendingActivation.ApprovalID)
		expect(t, dm, false, "POST", "/v1/identity/approvals/"+pendingActivation.ApprovalID+"/withdraw", fmt.Sprintf(`{"expected_version":%d}`, a.Version), 200)
		pump(t)
		expect(t, dm, true, "POST", policyPath+"/rollback-requests", `{"target_version":2,"note":"回滚"}`, 409, "already_active")
		expect(t, dm, true, "POST", policyPath+"/rollback-requests", `{"target_version":9,"note":"回滚"}`, 404, "policy_version_not_found")
		expect(t, dm, false, "POST", policyPath+"/rollback-requests", `{"target_version":1,"note":"回滚"}`, 403, "mfa_required")
		var rb cc.PolicyRequest
		decode(t, expect(t, dm, true, "POST", policyPath+"/rollback-requests", `{"target_version":1,"note":"第二版误伤"}`, 200), &rb)
		v1 := getPolicyVersion(t, expect, ra, policyPath, 1)
		if rb.Kind != "rollback" || rb.ObjectVersion != 1 || rb.SimulationID != v1.SimulationID || rb.BaselineVersion != 2 || rb.BaselineGeneration != 2 || rb.BackfillDays.Set {
			t.Fatalf("%+v", rb)
		}
		p = getPolicy(t, dm)
		if p.Draft.Or(cc.PolicyDraft{}).LockedBy.Set {
			t.Fatal("回滚申请锁定了草案")
		}
		request(t, dm, activation(p, "并行"), 409, "approval_in_progress")
		decide(t, admin, true, rb.ApprovalID, "approve", "", 200)
		pump(t)
		p = getPolicy(t, dm)
		if p.ActiveVersion != 1 || p.Generation != 3 {
			t.Fatalf("%+v", p)
		}
		active, _ := p.Active.Get()
		if !slices.Contains(active.EnabledDomains, "INF-CRED") || len(active.EnabledDomains) != 4 {
			t.Fatalf("回滚按当前领域状态改变了集合：%+v", active.EnabledDomains)
		}
		if sim, _ := p.Draft.Or(cc.PolicyDraft{}).Simulation.Get(); !sim.Stale {
			t.Fatalf("回滚后草案报告未过期：%+v", sim)
		}
		stale := p
		request(t, dm, activation(stale, "过期报告"), 409, "simulation_stale")
		ev := lastEvent(t, "customer_context.policy.rolled_back.v1")
		if ev["version"].(float64) != 1 || ev["previous_version"].(float64) != 2 || ev["generation"].(float64) != 3 {
			t.Fatalf("%+v", ev)
		}
		fixture.SetStatus("INF-CRED", "live")
	})

	t.Run("用例26：回滚申请驳回不改变草案与生效版本；回滚到较新的曾生效版本", func(t *testing.T) {
		before := getPolicy(t, dm)
		var rb cc.PolicyRequest
		decode(t, expect(t, dm, true, "POST", policyPath+"/rollback-requests", `{"target_version":2,"note":"回到第二版"}`, 200), &rb)
		decide(t, dm2, true, rb.ApprovalID, "reject", "暂不回到第二版", 200)
		pump(t)
		after := getPolicy(t, dm)
		if after.ActiveVersion != before.ActiveVersion || after.Generation != before.Generation || after.DraftVersion != before.DraftVersion ||
			after.Request.Or(cc.PolicyRequest{}).State != "rejected" {
			t.Fatalf("%+v", after)
		}
	})

	t.Run("仿真失败：数据集不可用时运行失败，不能用于申请；恢复后可重试", func(t *testing.T) {
		fixture.FailLoad(true)
		p := getPolicy(t, dm)
		saveDraft(t, dm, p.DraftVersion, formOf(t, p), 200)
		pump(t)
		p = getPolicy(t, dm)
		d, _ := p.Draft.Get()
		sim, _ := d.Simulation.Get()
		if sim.Status != "failed" || sim.FailureReason.Or("") != "dataset_unavailable" {
			t.Fatalf("%+v", sim)
		}
		request(t, dm, activation(p, "失败的仿真"), 409, "simulation_not_ready")
		fixture.FailLoad(false)
		var run lrn.SimulationRun
		decode(t, expect(t, dm, false, "POST", "/v1/learning/simulations", fmt.Sprintf(`{"objective_id":"%s","revision":%d}`, objective.ID, d.Revision), 200), &run)
		if run.Status != "completed" {
			t.Fatalf("%+v", run)
		}
		pump(t)
		if sim, _ := getPolicy(t, dm).Draft.Or(cc.PolicyDraft{}).Simulation.Get(); sim.Status != "completed" || sim.SimulationID != string(run.ID) {
			t.Fatalf("%+v", sim)
		}
	})

	t.Run("目标停用：进行中的申请随即失效；失效未送达时批准后不执行（objective_disabled）", func(t *testing.T) {
		p := getPolicy(t, dm)
		r := request(t, dm, activation(p, "停用前提交"), 200)
		approvals.failClose.Store(true)
		var o cc.Objective
		decode(t, expect(t, dm, false, "GET", objPath, "", 200), &o)
		expect(t, dm, false, "PUT", objPath+"/state", fmt.Sprintf(`{"expected_version":%d,"state":"disabled"}`, o.Version), 200)
		approvals.failClose.Store(false)
		if a := getApproval(t, admin, r.ApprovalID); a.State != "pending" {
			t.Fatalf("%+v", a)
		}
		decide(t, admin, true, r.ApprovalID, "approve", "", 200)
		pump(t)
		p = getPolicy(t, dm)
		if q := p.Request.Or(cc.PolicyRequest{}); q.State != "failed" || q.Reason.Or("") != "objective_disabled" || p.ActiveVersion != 1 {
			t.Fatalf("%+v", p)
		}
		saveDraft(t, dm, p.DraftVersion, formOf(t, p), 409, "objective_disabled")
		decode(t, expect(t, dm, false, "GET", objPath, "", 200), &o)
		expect(t, dm, false, "PUT", objPath+"/state", fmt.Sprintf(`{"expected_version":%d,"state":"inactive"}`, o.Version), 200)
	})

	t.Run("授权决策不可用：返回 503，不当作无权限，也不改变状态", func(t *testing.T) {
		before := getPolicy(t, dm)
		decider.fail.Store(true)
		expect(t, dm, false, "GET", policyPath, "", 503, "identity_unavailable")
		expect(t, dm, false, "GET", "/v1/learning/simulations/"+string(firstRun.ID), "", 503, "identity_unavailable")
		body, _ := json.Marshal(map[string]any{"expected_version": before.DraftVersion, "form": formOf(t, before)})
		expect(t, dm, false, "PUT", policyPath+"/draft", string(body), 503, "identity_unavailable")
		decider.fail.Store(false)
		if after := getPolicy(t, dm); after.DraftVersion != before.DraftVersion {
			t.Fatalf("%+v", after)
		}
	})

	userCtx := func(subject string) context.Context {
		c := reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01", Principal: reqctx.Principal{ID: subject, Kind: "user"}})
		return reqctx.WithAssurance(c, reqctx.Assurance{MFA: true, AuthTime: time.Now(), Source: reqctx.SourceIdP})
	}
	direct := func(t *testing.T, who string, objectVersion int64, evidence string, p cc.Policy) *idapi.ApprovalRequest {
		t.Helper()
		a, err := localApprovals.RequestApproval(userCtx(who), &idapi.RequestApprovalRequest{Type: identityapi.ApprovalTypePolicyActivation,
			Resource: "objective:" + objective.ID, Note: "本地登记未完成", Binding: idapi.ApprovalBinding{ObjectVersion: objectVersion, EvidenceRef: evidence,
				BaselineVersion: p.ActiveVersion, BaselineGeneration: p.Generation}}, fmt.Sprintf("direct-approval-%04d-%06d", objectVersion, keys.Add(1)))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	freshDraft := func(t *testing.T) cc.Policy {
		t.Helper()
		p := getPolicy(t, dm)
		saveDraft(t, dm, p.DraftVersion, formOf(t, p), 200)
		pump(t)
		p = getPolicy(t, dm)
		if sim := p.Draft.Or(cc.PolicyDraft{}).Simulation.Or(cc.PolicySimulationRef{}); sim.Status != "completed" || sim.Stale {
			t.Fatalf("%+v", p)
		}
		return p
	}

	t.Run("对账：identity 已创建而本地未登记的申请由 requested 事件补登；绑定不成立的随即失效", func(t *testing.T) {
		p := freshDraft(t)
		d, _ := p.Draft.Get()
		sim, _ := d.Simulation.Get()
		a := direct(t, dm, d.Revision, sim.SimulationID, p)
		pump(t)
		p = getPolicy(t, dm)
		if q := p.Request.Or(cc.PolicyRequest{}); q.ApprovalID != string(a.ID) || q.State != "pending" || p.Draft.Or(cc.PolicyDraft{}).LockedBy.Or("") != string(a.ID) {
			t.Fatalf("%+v", p)
		}
		expect(t, dm, false, "POST", "/v1/identity/approvals/"+string(a.ID)+"/withdraw", fmt.Sprintf(`{"expected_version":%d}`, a.Version), 200)
		pump(t)
		// 绑定旧修订：customer-context 使其失效（object_changed），不登记
		stale := direct(t, dm, d.Revision-1, sim.SimulationID, p)
		pump(t)
		if got := getApproval(t, dm, string(stale.ID)); got.State != "invalidated" || got.ClosedReason.Or("") != "object_changed" {
			t.Fatalf("%+v", got)
		}
		if q := getPolicy(t, dm).Request.Or(cc.PolicyRequest{}); q.ApprovalID == string(stale.ID) {
			t.Fatalf("登记了绑定不成立的申请：%+v", q)
		}
	})

	t.Run("乱序：决定先于登记送达时按事件中的绑定执行，迟到的 requested 不再改变状态", func(t *testing.T) {
		p := getPolicy(t, dm)
		d, _ := p.Draft.Get()
		sim, _ := d.Simulation.Get()
		hold["identity.approval.requested.v1"] = true
		a := direct(t, dm, d.Revision, sim.SimulationID, p)
		decide(t, dm2, true, string(a.ID), "approve", "", 200)
		pump(t)
		delete(hold, "identity.approval.requested.v1")
		after := getPolicy(t, dm)
		if after.ActiveVersion <= p.ActiveVersion || after.Generation != p.Generation+1 || after.Draft.Set || after.Request.Or(cc.PolicyRequest{}).State != "executed" {
			t.Fatalf("%+v", after)
		}
		var o cc.Objective
		decode(t, expect(t, dm, false, "GET", objPath, "", 200), &o)
		if o.State != "active" {
			t.Fatalf("重新启用后的目标在激活后应为生效：%+v", o)
		}
		for _, e := range pending {
			delivered = append(delivered, e)
			deliver(t, e)
		}
		pending = nil
		if again := getPolicy(t, dm); again.Generation != after.Generation || again.Request.Or(cc.PolicyRequest{}).State != "executed" {
			t.Fatalf("%+v", again)
		}
	})

	t.Run("并发：草案保存与激活申请同时到达，审批只绑定确定的修订，不留下待审批的孤立请求", func(t *testing.T) {
		for round := range 3 {
			p := freshDraft(t)
			body, _ := json.Marshal(map[string]any{"expected_version": p.DraftVersion, "form": formOf(t, p)})
			gate := make(chan struct{})
			var wg sync.WaitGroup
			var saveCode, requestCode int
			var requestRaw []byte
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-gate
				saveCode, _ = do(t, dm, t1, false, "PUT", policyPath+"/draft", string(body), "")
			}()
			go func() {
				defer wg.Done()
				<-gate
				requestCode, requestRaw = do(t, dm2, t1, true, "POST", policyPath+"/activation-requests", activation(p, fmt.Sprintf("并发第%d轮", round)), "")
			}()
			close(gate)
			wg.Wait()
			pump(t)
			after := getPolicy(t, dm)
			var list identityapi.ApprovalList
			decode(t, expect(t, admin, false, "GET", "/v1/identity/approvals", "", 200), &list)
			switch {
			case requestCode == 200 && saveCode == 409:
				var r cc.PolicyRequest
				decode(t, requestRaw, &r)
				if after.Draft.Or(cc.PolicyDraft{}).LockedBy.Or("") != r.ApprovalID || len(list.Approvals) != 1 || r.ObjectVersion != after.Draft.Or(cc.PolicyDraft{}).Revision {
					t.Fatalf("%+v %+v", after, list)
				}
				a := getApproval(t, dm2, r.ApprovalID)
				expect(t, dm2, false, "POST", "/v1/identity/approvals/"+r.ApprovalID+"/withdraw", fmt.Sprintf(`{"expected_version":%d}`, a.Version), 200)
				pump(t)
			case saveCode == 200 && requestCode == 409:
				if len(list.Approvals) != 0 || after.Draft.Or(cc.PolicyDraft{}).LockedBy.Set {
					t.Fatalf("留下了待审批的请求：%+v %+v", after, list)
				}
			default:
				t.Fatalf("保存 %d，申请 %d：%s", saveCode, requestCode, requestRaw)
			}
		}
	})

	t.Run("并发决定：两位审批人同时批准只有一个生效；过期的版本号被拒绝", func(t *testing.T) {
		p := freshDraft(t)
		r := request(t, dm, activation(p, "并发决定"), 200)
		a := getApproval(t, admin, r.ApprovalID)
		expect(t, dm2, true, "POST", "/v1/identity/approvals/"+r.ApprovalID+"/decide", fmt.Sprintf(`{"expected_version":%d,"decision":"approve"}`, a.Version+1), 409, "version_conflict")
		gate := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, who := range []string{dm2, admin} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				codes[i], _ = do(t, who, t1, true, "POST", "/v1/identity/approvals/"+r.ApprovalID+"/decide", fmt.Sprintf(`{"expected_version":%d,"decision":"approve"}`, a.Version), "")
			}()
		}
		close(gate)
		wg.Wait()
		slices.Sort(codes)
		if codes[0] != 200 || codes[1] != 409 {
			t.Fatalf("%v", codes)
		}
		pump(t)
		if after := getPolicy(t, dm); after.Generation != p.Generation+1 || after.ActiveVersion <= p.ActiveVersion {
			t.Fatalf("%+v", after)
		}
	})

	t.Run("审批列表分页：每页至多 limit 条，游标只对签发它的角色与状态有效", func(t *testing.T) {
		var page identityapi.ApprovalList
		decode(t, expect(t, dm, false, "GET", "/v1/identity/approvals?role=applicant&limit=2", "", 200), &page)
		if len(page.Approvals) != 2 || !page.Truncated {
			t.Fatalf("%+v", page)
		}
		seen := map[string]bool{}
		for _, a := range page.Approvals {
			seen[string(a.ID)] = true
		}
		cursor := string(page.NextCursor.Or(""))
		expect(t, dm, false, "GET", "/v1/identity/approvals?role=approver&limit=2&cursor="+cursor, "", 400, "invalid_cursor")
		total := len(page.Approvals)
		for page.Truncated {
			decode(t, expect(t, dm, false, "GET", "/v1/identity/approvals?role=applicant&limit=2&cursor="+string(page.NextCursor.Or("")), "", 200), &page)
			for _, a := range page.Approvals {
				if seen[string(a.ID)] {
					t.Fatalf("重复的请求 %s", a.ID)
				}
				seen[string(a.ID)] = true
			}
			total += len(page.Approvals)
		}
		var all identityapi.ApprovalList
		decode(t, expect(t, dm, false, "GET", "/v1/identity/approvals?role=applicant", "", 200), &all)
		if total != len(all.Approvals) || total < 5 || all.Truncated {
			t.Fatalf("%d %d", total, len(all.Approvals))
		}
	})

	t.Run("用例21：跨租户访问策略、版本、差异、报告与审批全部失败", func(t *testing.T) {
		for _, path := range []string{policyPath, policyPath + "/versions", policyPath + "/versions/1", policyPath + "/diff?from=v1&to=v2"} {
			expectKey(t, admin2, t2, false, "GET", path, "", "", 404, "objective_not_found")
		}
		expectKey(t, admin2, t2, true, "POST", policyPath+"/rollback-requests", `{"target_version":2,"note":"跨租户"}`, "", 404, "objective_not_found")
		expectKey(t, admin2, t2, false, "GET", "/v1/identity/approvals/"+first.ApprovalID, "", "", 404, "approval_not_found")
		expectKey(t, admin2, t2, true, "POST", "/v1/identity/approvals/"+first.ApprovalID+"/decide", `{"expected_version":2,"decision":"approve"}`, "", 404, "approval_not_found")
		expectKey(t, admin2, t2, false, "GET", "/v1/learning/simulations/"+string(firstRun.ID), "", "", 404, "simulation_not_found")
	})

	t.Run("用例22：草案、仿真、申请、撤回、批准、驳回、失效、激活、回滚与执行失败均有审计", func(t *testing.T) {
		actions := map[string]bool{}
		for _, s := range sources {
			rows, err := s.owner.Query(ctx, `SELECT envelope->'data'->>'action' FROM kernel_outbox WHERE envelope->>'type' = 'audit.entry.recorded.v1'`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var a string
				if err := rows.Scan(&a); err != nil {
					t.Fatal(err)
				}
				actions[a] = true
			}
			rows.Close()
		}
		for _, want := range []string{"customer_context.policy.drafted", "customer_context.policy.activation_requested", "customer_context.policy.rollback_requested",
			"customer_context.policy.activated", "customer_context.policy.rolled_back", "customer_context.policy.execution_failed",
			"customer_context.policy.activation_withdrawn", "customer_context.policy.activation_rejected", "customer_context.policy.activation_invalidated",
			"customer_context.policy.activation_expired", "customer_context.policy.simulation_attached", "learning.simulation.completed", "learning.simulation.failed",
			"identity.approval.requested", "identity.approval.decided", "identity.approval.closed.withdrawn", "identity.approval.closed.expired",
			"identity.approval.closed.applicant_left", "identity.approval.closed.applicant_lost_permission"} {
			if !actions[want] {
				t.Errorf("缺少审计 %s（已有 %v）", want, actions)
			}
		}
	})
}

func getPolicyVersion(t *testing.T, expect func(*testing.T, string, bool, string, string, string, int, ...string) []byte, who, policyPath string, version int) cc.PolicyVersion {
	t.Helper()
	var v cc.PolicyVersion
	raw := expect(t, who, false, "GET", fmt.Sprintf("%s/versions/%d", policyPath, version), "", 200)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
