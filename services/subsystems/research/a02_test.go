package research_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-faster/jx"
	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	ai "github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
	"github.com/superailabs/miaoqia/services/subsystems/research"
	"github.com/superailabs/miaoqia/services/subsystems/research/api"
)

// Everything in this fixture, including the publisher, observation and names, is synthetic.
const (
	fixtureNotice   = "ntc_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	fixtureSnapshot = "snp_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	fixtureEvidence = "ev_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	fixtureResult   = "bdr_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	fixtureWinner   = "合成样本中标单位"
)

var (
	actor = reqctx.Context{Principal: reqctx.Principal{ID: "fixture-user", Kind: "user"}, Tenant: "fixture-tenant", Cell: "fixture-cell", Purpose: "interactive"}
	basis = dataapi.Basis{View: dataapi.BasisViewCurrent, RecordedCutoff: dataapi.NilDateTime{Null: true}}
)

type ownerFixture struct {
	dataapi.UnimplementedHandler
	mu       sync.Mutex
	calls    []string
	gate     func(context.Context, string) error
	fact     dataapi.FactRead
	units    dataapi.BidResultPage
	evidence map[string]dataapi.EvidenceRead
}

func newOwner(t *testing.T, winners int) *ownerFixture {
	t.Helper()
	o := &ownerFixture{
		fact: dataapi.FactRead{Basis: basis, Fact: dataapi.Fact{ID: fixtureNotice, Version: 3, AdditionalProps: dataapi.FactAdditional{
			"notice_type": jx.Raw(`"award"`), "snapshot_ref": jx.Raw(`"` + fixtureSnapshot + `"`),
		}}},
		units:    dataapi.BidResultPage{Basis: basis, Items: []dataapi.BidResultPageItemsItem{}},
		evidence: make(map[string]dataapi.EvidenceRead),
	}
	results := make([]any, 0, winners)
	for i := range winners {
		id, evidence, name := fixtureResult, fixtureEvidence, fixtureWinner
		if i > 0 {
			id, evidence, name = id[:len(id)-1]+"2", evidence[:len(evidence)-1]+"2", "合成样本另一中标单位"
		}
		results = append(results, map[string]any{"id": id, "stage": "award", "is_winner": true,
			"bidder": map[string]any{"name_raw": name, "prov": map[string]any{"evidence_ref": evidence, "method": "rule", "confidence": 1}}})
		o.units.Items = append(o.units.Items, dataapi.BidResultPageItemsItem{
			Key: "tendering.bid_result:" + fixtureNotice + ":" + id, FactType: "tendering.bid_result", Domain: "INF-PROC", Event: "tendering.result.published.v1",
			Subject: dataapi.BidResultPageItemsItemSubject{ID: fixtureNotice, Version: 3, PartID: id},
			Fields: dataapi.BidResultPageItemsItemFields{
				ResultType: "awarded", Winner: dataapi.BidResultPageItemsItemFieldsWinner{NameRaw: name},
				Candidates: []dataapi.BidResultPageItemsItemFieldsCandidatesItem{},
			},
			ItemRefs: []dataapi.BidResultPageItemsItemItemRefsItem{{Name: "fact", ID: fixtureNotice, Version: 3, Domain: "INF-PROC"}},
			Display:  dataapi.BidResultPageItemsItemDisplay{Title: "合成样本公告", WinnerName: name, LotKey: "1"},
		})
		o.evidence[evidence] = dataapi.EvidenceRead{Kind: "doc", Evidence: dataapi.EvidenceReadEvidence{
			ID: evidence, SnapshotRef: fixtureSnapshot, SourceID: "synthetic-a02", SourceKey: "MOCK-AWARD-3",
			ObservedAt: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC), DisplayLevel: "excerpt",
			Locator: dataapi.NewNilEvidenceReadEvidenceLocator(dataapi.EvidenceReadEvidenceLocator{Block: i + 2}),
			Snippet: dataapi.NewNilString(name), Context: dataapi.NilString{Null: true},
		}}
	}
	lots, err := json.Marshal([]any{map[string]any{"lot_key": "1", "bid_results": results}})
	if err != nil {
		t.Fatal(err)
	}
	o.fact.Fact.AdditionalProps["lots"] = jx.Raw(lots)
	return o
}

func forbidden() *dataapi.ProblemStatusCode {
	return &dataapi.ProblemStatusCode{StatusCode: 403, Response: dataapi.Problem{Title: "forbidden", Status: 403}}
}

func (o *ownerFixture) check(ctx context.Context, op string) error {
	o.mu.Lock()
	o.calls = append(o.calls, op)
	o.mu.Unlock()
	if rc, ok := reqctx.From(ctx); !ok || rc != actor {
		return forbidden()
	}
	if o.gate != nil {
		return o.gate(ctx, op)
	}
	return nil
}

func (o *ownerFixture) GetFact(ctx context.Context, request *dataapi.FactRequest) (dataapi.NilFactRead, error) {
	if err := o.check(ctx, "fact"); err != nil {
		return dataapi.NilFactRead{}, err
	}
	if request.FactID != fixtureNotice || request.Version.Or(0) != 3 || request.Basis != basis {
		return dataapi.NilFactRead{}, fmt.Errorf("fixture expected fixed notice version 3, got %+v", request)
	}
	return dataapi.NewNilFactRead(o.fact), nil
}

func (o *ownerFixture) BidResultUnits(ctx context.Context, request *dataapi.FactRequest) (*dataapi.BidResultPage, error) {
	if err := o.check(ctx, "units"); err != nil {
		return nil, err
	}
	if request.FactID != fixtureNotice || request.Version.Or(0) != 3 || request.Basis != basis {
		return nil, fmt.Errorf("fixture expected fixed notice version 3, got %+v", request)
	}
	return &o.units, nil
}

func (o *ownerFixture) ResolveEvidence(ctx context.Context, request *dataapi.EvidenceRequest) (dataapi.NilEvidenceRead, error) {
	if err := o.check(ctx, request.EvidenceRef); err != nil {
		return dataapi.NilEvidenceRead{}, err
	}
	if value, ok := o.evidence[request.EvidenceRef]; ok {
		return dataapi.NewNilEvidenceRead(value), nil
	}
	return dataapi.NilEvidenceRead{Null: true}, nil
}

func (o *ownerFixture) NewError(_ context.Context, err error) *dataapi.ProblemStatusCode {
	var problem *dataapi.ProblemStatusCode
	if errors.As(err, &problem) {
		return problem
	}
	return &dataapi.ProblemStatusCode{StatusCode: 500, Response: dataapi.Problem{Title: "fixture error", Status: 500}}
}

func (o *ownerFixture) assertCalls(t *testing.T, want ...string) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if !slices.Equal(o.calls, want) {
		t.Fatalf("owner calls %v, want %v", o.calls, want)
	}
}

func newRunner(t *testing.T, o *ownerFixture, model research.A02Model) api.A02Runner {
	t.Helper()
	runner, err := research.NewA02(ownerReader(t, o), fixtureAdmission(), model)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

type fixtureDecider func(context.Context, authz.Request) (bool, error)

func (d fixtureDecider) Check(ctx context.Context, req authz.Request) (bool, error) {
	return d(ctx, req)
}

func fixtureAdmission() research.A02Admission {
	return research.A02Admission{
		OrgUnit: func(context.Context) (string, error) { return "root", nil },
		Authz: fixtureDecider(func(ctx context.Context, req authz.Request) (bool, error) {
			rc, ok := reqctx.From(ctx)
			return ok && req.Subject == authz.Subject(rc.Principal) && req.Relation == "can_start_research" && req.Object == "org_unit:root", nil
		}),
	}
}

func request(reads, models int) api.A02Request {
	return api.A02Request{
		Notice: api.NoticeVersion{ID: fixtureNotice, Version: 3},
		Budget: api.A02Budget{Reads: reads, ModelCalls: models, Deadline: time.Now().Add(5 * time.Second)},
	}
}

func caller() context.Context { return reqctx.With(context.Background(), actor) }

func requireUnknown(t *testing.T, report api.A02Report, topic, reason string) {
	t.Helper()
	if !slices.Contains(report.Unknowns, api.A02Unknown{Topic: topic, Reason: reason}) {
		t.Fatalf("missing %s/%s in %+v", topic, reason, report.Unknowns)
	}
}

func TestA02FixedReportThroughDatareadTransport(t *testing.T) {
	o := newOwner(t, 1)
	server, err := dataapi.NewServer(o)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, err := reqctx.Decode(r.Header.Get(reqctx.Header))
		if err != nil {
			http.Error(w, "missing fixture context", http.StatusBadRequest)
			return
		}
		server.ServeHTTP(w, r.WithContext(reqctx.With(r.Context(), rc)))
	}))
	defer httpServer.Close()
	httpClient := httpServer.Client()
	httpClient.Transport = binding.Transport{Base: httpClient.Transport}
	client, err := dataread.NewWithHTTPClient(httpServer.URL, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := research.NewA02(client, fixtureAdmission(), research.A02Model{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.RunA02(caller(), request(3, 0))
	if err != nil || report.Partial || report.Notice != request(3, 0).Notice || report.Template != api.A02Template || report.ReadsUsed != 3 || len(report.Facts) != 1 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
	fact := report.Facts[0]
	if fact.WinnerName != fixtureWinner || fact.Field != "/lots/0/bid_results/0/bidder/name_raw" || fact.Citation.EvidenceRef != fixtureEvidence ||
		fact.Citation.SnapshotRef != fixtureSnapshot || fact.Citation.Quote != fixtureWinner || fact.Citation.Locator.Block != 2 ||
		fact.Citation.SourceID != "synthetic-a02" || fact.Citation.SourceKey != "MOCK-AWARD-3" || fact.Citation.ObservedAt.IsZero() || len(report.Inferences) != 0 {
		t.Fatalf("fact=%+v inferences=%+v", fact, report.Inferences)
	}
	requireUnknown(t, report, "prime_contractor", "contract_role_requires_separate_evidence")
	requireUnknown(t, report, "entity_identity", "entity_lookup_out_of_scope")
	requireUnknown(t, report, "supply_window", "supply_window_requires_separate_evidence")
	rc := actor
	rc.Cell = "other-fixture-cell"
	denied, err := runner.RunA02(reqctx.With(context.Background(), rc), request(3, 0))
	if !errors.Is(err, api.ErrA02AccessDenied) || len(denied.Facts) != 0 {
		t.Fatalf("cross-Cell HTTP report=%+v error=%v", denied, err)
	}
	o.assertCalls(t, "fact", "units", fixtureEvidence, "fact")
}

func TestA02RejectsUnboundInputs(t *testing.T) {
	o := newOwner(t, 1)
	runner := newRunner(t, o, research.A02Model{})
	for _, rc := range []reqctx.Context{
		{},
		{Tenant: actor.Tenant, Cell: actor.Cell, Purpose: "monitoring", Principal: reqctx.Principal{ID: "svc:detection", Kind: "service"}},
		{Tenant: actor.Tenant, Cell: actor.Cell, Purpose: "simulation", Principal: actor.Principal},
		{Tenant: actor.Tenant, Cell: actor.Cell, Purpose: "research", Principal: actor.Principal},
		{Tenant: actor.Tenant, Purpose: actor.Purpose, Principal: actor.Principal},
		{Tenant: actor.Tenant, Cell: actor.Cell, Purpose: actor.Purpose, Principal: reqctx.Principal{ID: "fixture-service", Kind: "service_account"}},
	} {
		if _, err := runner.RunA02(reqctx.With(context.Background(), rc), request(3, 0)); !errors.Is(err, api.ErrA02AccessDenied) {
			t.Errorf("context %+v: got %v", rc, err)
		}
	}
	if _, err := runner.RunA02(context.Background(), request(3, 0)); !errors.Is(err, api.ErrA02AccessDenied) {
		t.Errorf("absent context: got %v", err)
	}
	for _, change := range []func(*api.A02Request){
		func(r *api.A02Request) { r.Notice.Version = 0 },
		func(r *api.A02Request) { r.Notice.ID = "pp_01JA8ZXXXXXXXXXXXXXXXXXXX1" },
		func(r *api.A02Request) { r.Budget.Reads = -1 },
		func(r *api.A02Request) { r.Budget.Reads = 17 },
		func(r *api.A02Request) { r.Budget.ModelCalls = 2 },
		func(r *api.A02Request) { r.Budget.Deadline = time.Now().Add(time.Minute) },
		func(r *api.A02Request) { r.Budget.Deadline = time.Time{} },
	} {
		req := request(3, 0)
		change(&req)
		if _, err := runner.RunA02(caller(), req); !errors.Is(err, api.ErrInvalidA02Request) {
			t.Errorf("request %+v: got %v", req, err)
		}
	}
	o.assertCalls(t)
}

func TestA02RequiresCurrentResearchStartPermission(t *testing.T) {
	for _, scenario := range []string{"denied", "unavailable", "unbound_unit"} {
		t.Run(scenario, func(t *testing.T) {
			o := newOwner(t, 1)
			admission := fixtureAdmission()
			var decisionCalls int
			admission.Authz = fixtureDecider(func(ctx context.Context, req authz.Request) (bool, error) {
				decisionCalls++
				rc, _ := reqctx.From(ctx)
				if rc != actor || req != (authz.Request{Subject: "user:fixture-user", Relation: "can_start_research", Object: "org_unit:root"}) {
					t.Fatalf("permission context=%+v request=%+v", rc, req)
				}
				if scenario == "unavailable" {
					return false, errors.New("fixture identity unavailable")
				}
				return false, nil
			})
			wantErr, wantChecks := api.ErrA02AccessDenied, 1
			if scenario == "unavailable" {
				wantErr = api.ErrA02AdmissionUnavailable
			}
			if scenario == "unbound_unit" {
				admission.OrgUnit = func(context.Context) (string, error) { return "", nil }
				wantChecks = 0
			}
			runner, err := research.NewA02(ownerReader(t, o), admission, research.A02Model{})
			if err != nil {
				t.Fatal(err)
			}
			report, err := runner.RunA02(caller(), request(3, 0))
			if !errors.Is(err, wantErr) || report.Template != "" || decisionCalls != wantChecks {
				t.Fatalf("report=%+v error=%v decision calls=%d", report, err, decisionCalls)
			}
			o.assertCalls(t)
		})
	}
}

func TestA02RejectsCrossScopeAndRevocation(t *testing.T) {
	for _, scope := range []string{"tenant", "cell", "user", "revoked"} {
		t.Run(scope, func(t *testing.T) {
			o := newOwner(t, 2)
			rc := actor
			switch scope {
			case "tenant":
				rc.Tenant = "other-fixture-tenant"
			case "cell":
				rc.Cell = "other-fixture-cell"
			case "user":
				rc.Principal.ID = "other-fixture-user"
			case "revoked":
				o.gate = func(_ context.Context, op string) error {
					if op == "ev_01JA8ZXXXXXXXXXXXXXXXXXXX2" {
						return forbidden()
					}
					return nil
				}
			}
			runner := newRunner(t, o, research.A02Model{})
			report, err := runner.RunA02(reqctx.With(context.Background(), rc), request(4, 0))
			if !errors.Is(err, api.ErrA02AccessDenied) || len(report.Facts) != 0 || report.Notice.ID != "" {
				t.Fatalf("report=%+v error=%v", report, err)
			}
			if scope == "revoked" {
				o.assertCalls(t, "fact", "units", fixtureEvidence, "ev_01JA8ZXXXXXXXXXXXXXXXXXXX2")
			} else {
				o.assertCalls(t, "fact")
			}
		})
	}
}

func TestA02BudgetPreservesVerifiedPartial(t *testing.T) {
	for _, scenario := range []string{"read_count", "no_reads", "deadline", "already_expired", "caller_cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			o := newOwner(t, 2)
			req := request(3, 0)
			wantTopic, wantReason, wantCount := "winner_evidence", "read_budget_exhausted", 1
			wantCalls := []string{"fact", "units", fixtureEvidence}
			ctx, cancel := context.WithCancel(caller())
			defer cancel()
			switch scenario {
			case "no_reads":
				req.Budget.Reads = 0
				wantTopic, wantCount, wantCalls = "award_result", 0, nil
			case "caller_cancelled":
				cancel()
			case "deadline":
				req.Budget.Reads = 4
				req.Budget.Deadline = time.Now().Add(500 * time.Millisecond)
				wantReason = "deadline_exceeded"
				wantCalls = append(wantCalls, "ev_01JA8ZXXXXXXXXXXXXXXXXXXX2")
				o.gate = func(ctx context.Context, op string) error {
					if op == "ev_01JA8ZXXXXXXXXXXXXXXXXXXX2" {
						<-ctx.Done()
					}
					return nil // The owner returns a late response, which must be discarded.
				}
			case "already_expired":
				req.Budget.Deadline = time.Now().Add(-time.Second)
			}
			report, err := newRunner(t, o, research.A02Model{}).RunA02(ctx, req)
			if scenario == "already_expired" {
				if !errors.Is(err, api.ErrA02AdmissionUnavailable) || report.Template != "" {
					t.Fatalf("report=%+v error=%v", report, err)
				}
				o.assertCalls(t)
				return
			}
			if scenario == "caller_cancelled" {
				if !errors.Is(err, context.Canceled) || report.Template != "" {
					t.Fatalf("report=%+v error=%v", report, err)
				}
				o.assertCalls(t)
				return
			}
			if err != nil || !report.Partial || len(report.Facts) != wantCount || report.ReadsUsed != len(wantCalls) {
				t.Fatalf("report=%+v error=%v", report, err)
			}
			requireUnknown(t, report, wantTopic, wantReason)
			o.assertCalls(t, wantCalls...)
		})
	}
}

func TestA02RejectsUnverifiableEvidence(t *testing.T) {
	for _, scenario := range []string{"wrong_version", "wrong_snapshot", "wrong_ref", "wrong_quote", "wrong_winner", "citation_only", "missing", "candidate", "no_awarded_unit"} {
		t.Run(scenario, func(t *testing.T) {
			o := newOwner(t, 1)
			evidence := o.evidence[fixtureEvidence]
			topic, reason := "winner_evidence", "evidence_mismatch"
			switch scenario {
			case "wrong_version":
				topic, reason = "award_result", "owner_read_failed"
				o.fact.Fact.Version++
			case "wrong_snapshot":
				evidence.Evidence.SnapshotRef = "snp_other"
			case "wrong_ref":
				reason = "owner_read_failed"
				evidence.Evidence.ID = "ev_other"
			case "wrong_quote":
				evidence.Evidence.Snippet = dataapi.NewNilString(fixtureWinner + "分公司")
			case "wrong_winner":
				topic, reason = "award_result", "award_view_mismatch"
				o.units.Items[0].Fields.Winner.NameRaw = "合成其他公司"
			case "citation_only":
				reason = "excerpt_access_required"
				evidence.Evidence.DisplayLevel = "citation_only"
				evidence.Evidence.Locator = dataapi.NilEvidenceReadEvidenceLocator{Null: true}
				evidence.Evidence.Snippet = dataapi.NilString{Null: true}
			case "missing":
				reason = "unresolved"
			case "candidate":
				o.fact.Fact.AdditionalProps["notice_type"] = jx.Raw(`"candidate"`)
			case "no_awarded_unit":
				topic, reason = "award_result", "award_winner_unavailable"
				o.units.Items = o.units.Items[:0]
			}
			o.evidence[fixtureEvidence] = evidence
			if scenario == "missing" {
				delete(o.evidence, fixtureEvidence)
			}
			report, err := newRunner(t, o, research.A02Model{}).RunA02(caller(), request(3, 0))
			if scenario == "candidate" {
				if !errors.Is(err, api.ErrInvalidA02Request) {
					t.Fatalf("candidate request error=%v", err)
				}
				o.assertCalls(t, "fact")
				return
			}
			if err != nil || !report.Partial || len(report.Facts) != 0 {
				t.Fatalf("report=%+v error=%v", report, err)
			}
			requireUnknown(t, report, topic, reason)
		})
	}
}

// Only Generate is exposed by the A02 template. Classify and Score are unused.
type syntheticGateway struct {
	ai.Gateway
	generate func(context.Context, ai.GenerateRequest) (*ai.GenerateResult, error)
}

func (m syntheticGateway) Generate(ctx context.Context, req ai.GenerateRequest) (*ai.GenerateResult, error) {
	return m.generate(ctx, req)
}

func TestA02ModelNeedsIndependentPermissionAndCannotAddFacts(t *testing.T) {
	for _, scenario := range []string{"allowed", "display_only", "model_disabled", "model_unavailable", "invented_role"} {
		t.Run(scenario, func(t *testing.T) {
			calls, authorizations := 0, 0
			model := research.A02Model{
				Authorize: func(ctx context.Context, use research.A02ModelUse) error {
					authorizations++
					rc, _ := reqctx.From(ctx)
					if rc != actor || use.Notice != request(3, 0).Notice || use.Template != api.A02Template ||
						use.Task != "research.a02_award_review" || use.Classification != ai.Internal ||
						!slices.Equal(use.Sources, []research.A02ModelSource{{Field: "/lots/0/bid_results/0/bidder/name_raw", EvidenceRef: fixtureEvidence, SnapshotRef: fixtureSnapshot}}) {
						t.Fatalf("model authorization context=%+v use=%+v", rc, use)
					}
					if scenario == "display_only" {
						return errors.New("fixture has display rights only")
					}
					return nil
				},
				Gateway: ai.LocalGateway(syntheticGateway{generate: func(ctx context.Context, req ai.GenerateRequest) (*ai.GenerateResult, error) {
					calls++
					rc, _ := reqctx.From(ctx)
					if rc != actor || req.Classification != ai.Internal || req.Budget.MaxOutputTokens <= 0 || req.Budget.MaxCostMicroYuan <= 0 ||
						req.PromptRef != api.A02Template || !slices.Equal(req.Inputs, []ai.Input{{Name: "award_names", Content: `["` + fixtureWinner + `"]`}}) {
						t.Fatalf("model context=%+v request=%+v", rc, req)
					}
					output := json.RawMessage(`{"recommendation":"verify_contract_role"}`)
					if scenario == "invented_role" {
						output = json.RawMessage(`{"recommendation":"verify_contract_role","prime_contractor":"合成样本中标单位"}`)
					}
					return &ai.GenerateResult{Output: output, Model: ai.ModelRef{Model: "synthetic-a02", Version: "1"}, InvocationID: "fixture-invocation"}, nil
				}}),
			}
			req := request(3, 1)
			if scenario == "model_disabled" {
				req.Budget.ModelCalls = 0
			}
			if scenario == "model_unavailable" {
				model = research.A02Model{}
			}
			report, err := newRunner(t, newOwner(t, 1), model).RunA02(caller(), req)
			if err != nil || len(report.Facts) != 1 || len(report.Inferences) != 0 || report.ModelCallsUsed != calls {
				t.Fatalf("report=%+v error=%v", report, err)
			}
			requireUnknown(t, report, "prime_contractor", "contract_role_requires_separate_evidence")
			switch scenario {
			case "allowed":
				if authorizations != 1 || calls != 1 || report.Partial || report.Model == nil || report.InvocationID != "fixture-invocation" ||
					!slices.Equal(report.Recommendations, []api.A02Recommendation{api.VerifyPrimeRole}) {
					t.Fatalf("report=%+v authorizations=%d calls=%d", report, authorizations, calls)
				}
			case "display_only":
				if authorizations != 1 || calls != 0 || !report.Partial || len(report.Recommendations) != 0 {
					t.Fatalf("report=%+v authorizations=%d calls=%d", report, authorizations, calls)
				}
				requireUnknown(t, report, "model_review", "model_use_not_authorized")
			case "model_disabled":
				if authorizations != 0 || calls != 0 || report.Partial {
					t.Fatalf("report=%+v authorizations=%d calls=%d", report, authorizations, calls)
				}
			case "model_unavailable":
				if authorizations != 0 || calls != 0 || !report.Partial {
					t.Fatalf("report=%+v authorizations=%d calls=%d", report, authorizations, calls)
				}
				requireUnknown(t, report, "model_review", "model_unavailable")
			case "invented_role":
				if authorizations != 1 || calls != 1 || len(report.Recommendations) != 0 {
					t.Fatalf("report=%+v authorizations=%d calls=%d", report, authorizations, calls)
				}
				requireUnknown(t, report, "model_review", "model_output_invalid")
			}
		})
	}
	if _, err := research.NewA02(ownerReader(t, newOwner(t, 1)), fixtureAdmission(), research.A02Model{Gateway: syntheticGateway{}}); err == nil {
		t.Fatal("a gateway without an independent model-use authorizer was accepted")
	}
}

func ownerReader(t *testing.T, o *ownerFixture) *dataread.Client {
	t.Helper()
	client, err := dataread.NewLocal(o)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
