package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	searchapi "github.com/superailabs/miaoqia/services/subsystems/search/api"
)

type watchRecorder struct {
	registered []searchapi.WatchQuery
	removed    []string
}

func (w *watchRecorder) Register(_ context.Context, query searchapi.WatchQuery) error {
	w.registered = append(w.registered, query)
	return nil
}

func (w *watchRecorder) Remove(_ context.Context, id string) error {
	w.removed = append(w.removed, id)
	return nil
}

func (*watchRecorder) Percolate(context.Context, searchapi.Fact) ([]string, error) { return nil, nil }

type failingWatchRecorder struct{ watchRecorder }

func (failingWatchRecorder) Register(context.Context, searchapi.WatchQuery) error {
	return errors.New("index unavailable")
}

type activePolicyReader struct{ pages []*cc.ActivePolicyList }

func (r activePolicyReader) ActivePolicies(_ context.Context, _ int, _ string) (*cc.ActivePolicyList, error) {
	if len(r.pages) == 0 {
		return &cc.ActivePolicyList{}, nil
	}
	return r.pages[0], nil
}

type contextReader struct{}

func (contextReader) ContextVersion(context.Context) (*customercontext.Version, error) {
	return &customercontext.Version{ContextVersion: 7}, nil
}

func (contextReader) ContextSnapshot(_ context.Context, unit string, version int64) (*customercontext.Snapshot, error) {
	return &customercontext.Snapshot{OrgUnitID: unit, ContextVersion: version, PolicyView: cc.ContextPolicyView{
		ServiceRegions: []string{"330000"}, OfferingCategories: []string{"CAT-PIPE"},
	}}, nil
}

func watchContext() context.Context {
	return reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1", Cell: "cell-01",
		Principal: reqctx.Principal{ID: "svc:detection", Kind: "service"}, Purpose: "monitoring"})
}

func policyFromJSON(t *testing.T, body string) cc.ActivePolicy {
	t.Helper()
	var p cc.ActivePolicy
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicyChangedRegistersExplicitAndProfileDefaults(t *testing.T) {
	explicit := policyFromJSON(t, `{"objective_id":"obj_01JA8ZXXXXXXXXXXXXXXXXXXX1","objective_code":"A02","objective_state":"active","org_unit_id":"root","policy_id":"pol_01JA8ZXXXXXXXXXXXXXXXXXXX1","generation":1,"version":1,"template_id":"tpl","template_version":"1","enabled_domains":[],"effective_at":"2026-01-01T00:00:00Z","document":{"id":"tpl","version":"1","fact_types":["tendering.bid_result"],"param_values":{"regions":["330000"],"categories":["CAT-PIPE"]}}}`)
	defaults := policyFromJSON(t, `{"objective_id":"obj_01JA8ZXXXXXXXXXXXXXXXXXXX2","objective_code":"A02","objective_state":"active","org_unit_id":"root","policy_id":"pol_01JA8ZXXXXXXXXXXXXXXXXXXX2","generation":1,"version":1,"template_id":"tpl","template_version":"1","enabled_domains":[],"effective_at":"2026-01-01T00:00:00Z","document":{"id":"tpl","version":"1","scope":{"fact_types":["tendering.bid_result"]}}}`)
	recorder := &watchRecorder{}
	s := &Service{Watches: recorder, Policies: activePolicyReader{pages: []*cc.ActivePolicyList{{Policies: []cc.ActivePolicy{explicit, defaults}}}}, Context: contextReader{}}
	if err := s.PolicyChanged(watchContext(), nil, &outbox.Envelope{}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.registered) != 2 {
		t.Fatalf("registered=%#v", recorder.registered)
	}
	if got := recorder.registered[0]; len(got.Regions) != 1 || got.Regions[0] != "330000" || got.Categories[0] != "CAT-PIPE" {
		t.Fatalf("explicit query=%#v", got)
	}
	if got := recorder.registered[1]; len(got.Regions) != 1 || got.Regions[0] != "330000" || got.Categories[0] != "CAT-PIPE" {
		t.Fatalf("default query=%#v", got)
	}
}

func TestObjectiveChangedRemovesOnlyInactiveOrDisabledWatch(t *testing.T) {
	for _, state := range []string{"inactive", "disabled"} {
		t.Run(state, func(t *testing.T) {
			recorder := &watchRecorder{}
			s := &Service{Watches: recorder}
			data, _ := json.Marshal(map[string]string{"objective_id": "obj_01JA8ZXXXXXXXXXXXXXXXXXXX1", "state": state})
			if err := s.ObjectiveChanged(watchContext(), nil, &outbox.Envelope{Data: data}); err != nil {
				t.Fatal(err)
			}
			if len(recorder.removed) != 1 || recorder.removed[0] != "obj_01JA8ZXXXXXXXXXXXXXXXXXXX1" {
				t.Fatalf("removed=%v", recorder.removed)
			}
		})
	}
	recorder := &watchRecorder{}
	data, _ := json.Marshal(map[string]string{"objective_id": "obj_01JA8ZXXXXXXXXXXXXXXXXXXX1", "state": "active"})
	if err := (&Service{Watches: recorder}).ObjectiveChanged(watchContext(), nil, &outbox.Envelope{Data: data}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.removed) != 0 {
		t.Fatalf("active target removed=%v", recorder.removed)
	}
}

func TestPolicyChangedFailsClosedWhenRegistrationFails(t *testing.T) {
	active := policyFromJSON(t, `{"objective_id":"obj_01JA8ZXXXXXXXXXXXXXXXXXXX1","objective_code":"A02","objective_state":"active","org_unit_id":"root","policy_id":"pol_01JA8ZXXXXXXXXXXXXXXXXXXX1","generation":1,"version":1,"template_id":"tpl","template_version":"1","enabled_domains":[],"effective_at":"2026-01-01T00:00:00Z","document":{"id":"tpl","version":"1","scope":{"fact_types":["tendering.bid_result"]}}}`)
	s := &Service{Watches: &failingWatchRecorder{}, Policies: activePolicyReader{pages: []*cc.ActivePolicyList{{Policies: []cc.ActivePolicy{active}}}}}
	if err := s.PolicyChanged(watchContext(), nil, &outbox.Envelope{}); err == nil {
		t.Fatal("registration failure was swallowed")
	}
}
