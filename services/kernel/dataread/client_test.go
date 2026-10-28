package dataread

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

func TestClientPostsClosedReadAndCarriesContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/data/get-fact" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get(reqctx.Header) == "" {
			t.Fatal("missing request context")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["fact_id"] != "pp_abc" {
			t.Fatalf("unexpected body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fact":{"id":"pp_abc"}}`))
	}))
	defer server.Close()
	client, err := NewWithHTTPClient(server.URL, &http.Client{Transport: transportWithContext{base: http.DefaultTransport}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_a", Cell: "cell-01", Principal: reqctx.Principal{ID: "usr_a", Kind: "user"}, Purpose: "interactive"})
	var out map[string]any
	if err := client.GetFact(ctx, "pp_abc", Basis{View: "current"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if out["fact"].(map[string]any)["id"] != "pp_abc" {
		t.Fatalf("unexpected response: %#v", out)
	}
}

type transportWithContext struct{ base http.RoundTripper }

func (t transportWithContext) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	c, ok := reqctx.From(r.Context())
	if !ok {
		return nil, reqctx.ErrMissing
	}
	r.Header.Set(reqctx.Header, reqctx.Encode(c))
	return t.base.RoundTrip(r)
}

func TestClientMapsServerErrorsAndRejectsOversizedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/data/get-fact" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"source_unavailable"}`))
			return
		}
		_, _ = w.Write(make([]byte, maxResponseBytes+1))
	}))
	defer server.Close()
	client, err := NewWithHTTPClient(server.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_a", Cell: "cell-01", Principal: reqctx.Principal{ID: "usr_a", Kind: "user"}, Purpose: "interactive"})
	err = client.GetFact(ctx, "pp_abc", Basis{View: "current"}, nil, &map[string]any{})
	var dataErr *Error
	if !errors.As(err, &dataErr) || dataErr.Code != "source_unavailable" || dataErr.Status != 503 {
		t.Fatalf("unexpected error: %v", err)
	}

	oversize := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxResponseBytes+1))
	}))
	defer oversize.Close()
	oversizeClient, err := NewWithHTTPClient(oversize.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := oversizeClient.call(ctx, "Oversize", "/internal/v1/data/oversize", map[string]any{}, &map[string]any{}); err == nil {
		t.Fatal("oversized response accepted")
	}
}
