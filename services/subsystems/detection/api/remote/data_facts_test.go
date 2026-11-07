package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

func TestFactsReadsAndValidatesBidResultUnits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/data/bid-result-units" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{
			"key": "tendering.bid_result:pp_abc:bdr_1", "fact_type": "tendering.bid_result", "domain": "INF-PROC",
			"subject": map[string]any{"id": "pp_abc", "version": 2, "part_id": "bdr_1"}, "event": "tendering.result.published.v1",
			"fields": map[string]any{"winner": map[string]any{"name_raw": "示例公司"}}, "items": map[string]any{}, "item_domains": map[string]string{},
			"item_refs": []any{}, "evidence": map[string]any{"fact.winner": map[string]any{"confidence": 0.99, "method": "rule", "suspect": false}},
			"display": map[string]any{"title": "示例项目", "winner_name": "示例公司", "categories": []string{"pipe"}},
		}}})
	}))
	defer server.Close()
	facts, err := NewFactsWithHTTPClient(server.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_a", Cell: "cell-01", Principal: reqctx.Principal{ID: "usr_a", Kind: "user"}, Purpose: "interactive"})
	units, err := facts.Units(ctx, api.FactRef{ID: "pp_abc", Version: 2, ObjectType: "tendering.notice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].Display.WinnerName != "示例公司" || units[0].Evidence["fact.winner"].Confidence != 0.99 {
		t.Fatalf("unexpected units: %#v", units)
	}
}

func TestFactsRejectsMismatchedSubject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"key":"x","fact_type":"tendering.bid_result","domain":"INF-PROC","subject":{"id":"pp_other","version":1,"part_id":"bdr_1"},"fields":{},"items":{},"item_domains":{},"item_refs":[],"evidence":{},"display":{}}]}`))
	}))
	defer server.Close()
	facts, err := NewFactsWithHTTPClient(server.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_a", Cell: "cell-01", Principal: reqctx.Principal{ID: "usr_a", Kind: "user"}, Purpose: "interactive"})
	if _, err := facts.Units(ctx, api.FactRef{ID: "pp_abc", Version: 1}); err == nil {
		t.Fatal("mismatched subject accepted")
	}
}

func TestFactsMapsForbiddenToReadDeniedAndKeepsUnavailableRetryable(t *testing.T) {
	for _, tc := range []struct {
		status int
		denied bool
	}{{http.StatusForbidden, true}, {http.StatusServiceUnavailable, false}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"title":"request rejected"}`))
		}))
		facts, err := NewFactsWithHTTPClient(server.URL, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		ctx := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_a", Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:detection", Kind: "service"}, Purpose: "monitoring"})
		_, err = facts.Units(ctx, api.FactRef{ID: "pp_abc", Version: 1})
		server.Close()
		if err == nil || errors.Is(err, api.ErrReadDenied) != tc.denied {
			t.Fatalf("status %d: err=%v", tc.status, err)
		}
	}
}
