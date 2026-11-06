package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

func TestDatasetLoadsA02UnitsFromDataAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/data/query-facts":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{
				"id": "ntc_01", "version": 1, "published_at": map[string]any{"value": "2026-10-01T00:00:00Z"}, "recorded_at": "2026-10-01T00:00:01Z",
			}}, "next_cursor": ""})
		case "/internal/v1/data/bid-result-units":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{
				"fact_type": "tendering.bid_result", "domain": "INF-PROC", "event": "tendering.result.published.v1",
				"subject": map[string]any{"id": "ntc_01", "version": 1, "part_id": "bdr_01"},
				"fields":  map[string]any{"result_type": "awarded"}, "items": map[string]any{}, "evidence": map[string]any{},
				"display": map[string]any{"title": "示例结果", "amount_cny": 100},
			}}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewWithHTTPClient(server.URL, &http.Client{Transport: contextTransport{base: http.DefaultTransport}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_01", Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:learning", Kind: "service"}, Purpose: "simulation"})
	slice, err := client.Load(ctx, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if slice.Fixture || len(slice.Facts) != 1 || slice.Facts[0].Title != "示例结果" || slice.Facts[0].AmountCNY == nil || *slice.Facts[0].AmountCNY != 100 {
		t.Fatalf("unexpected dataset: %#v", slice)
	}
}

type contextTransport struct{ base http.RoundTripper }

func (t contextTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c, ok := reqctx.From(r.Context())
	if !ok {
		return nil, reqctx.ErrMissing
	}
	r = r.Clone(r.Context())
	r.Header.Set(reqctx.Header, reqctx.Encode(c))
	return t.base.RoundTrip(r)
}
