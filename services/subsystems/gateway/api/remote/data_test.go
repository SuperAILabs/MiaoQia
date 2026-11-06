package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
)

func TestA02PublicDataReadsProjectAndSections(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/data/get-fact" && r.URL.Path != "/internal/v1/data/resolve-evidence" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"basis": map[string]any{"view": "current"},
			"fact":  map[string]any{"id": "pp_01", "version": 1, "info_domain": "INF-PROC", "title": map[string]any{"value": "示例项目"}, "stage": "awarded", "notice_ids": []string{"ntc_01"}, "lots": []any{}},
		})
	}))
	defer server.Close()
	client, err := NewWithHTTPClient(server.URL, &http.Client{Transport: contextTransport{base: http.DefaultTransport}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_01", Cell: "cell-01", Principal: reqctx.Principal{ID: "usr_01", Kind: "user"}, Purpose: "interactive"})
	obj, err := client.Resolve(ctx, "pp_01", api.Basis{View: api.ViewCurrent})
	if err != nil || obj.Name != "示例项目" || obj.Type != "procurement_project" {
		t.Fatalf("unexpected object: %#v err=%v", obj, err)
	}
	section, err := client.Section(ctx, api.SectionRequest{Object: *obj, Section: "summary", Basis: api.Basis{View: api.ViewCurrent}, Limit: 20})
	if err != nil || section.Status != "ok" || len(section.Items) != 1 {
		t.Fatalf("unexpected section: %#v err=%v", section, err)
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
