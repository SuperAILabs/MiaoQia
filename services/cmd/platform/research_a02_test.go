package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	researchapi "github.com/superailabs/miaoqia/services/subsystems/research/api"
)

type a02Decider func(context.Context, authz.Request) (bool, error)

func (d a02Decider) Check(ctx context.Context, request authz.Request) (bool, error) {
	return d(ctx, request)
}

func TestA02AgainstPythonListener(t *testing.T) {
	base := os.Getenv("MIAOQIA_TEST_DATAREAD_URL")
	if base == "" {
		t.Skip("run via the Python real PostgreSQL fixture with MIAOQIA_TEST_DATAREAD_GO=1")
	}
	reader, err := dataread.New(base)
	if err != nil {
		t.Fatal(err)
	}
	scope := reqctx.Context{
		Tenant: "mock-tenant", Cell: "mock-cell", Principal: reqctx.Principal{ID: "mock-research-user", Kind: "user"}, Purpose: "interactive",
	}
	calls := 0
	admit := true
	handler := a02ResearchHandler(reader, a02Decider(func(ctx context.Context, request authz.Request) (bool, error) {
		calls++
		got, _ := reqctx.From(ctx)
		if got != scope || request.Subject != "user:mock-research-user" ||
			request.Relation != "can_start_research" || request.Object != "org_unit:root" {
			t.Fatalf("incorrect research authorization scope: %+v %+v", got, request)
		}
		return admit, nil
	}))
	id, evidenceID := os.Getenv("MIAOQIA_TEST_DATAREAD_FACT"), os.Getenv("MIAOQIA_TEST_DATAREAD_EVIDENCE")
	payload := `{"org_unit_id":"root","notice":{"id":"` + id + `","version":3}}`
	request := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/research/a02", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(reqctx.With(r.Context(), scope)))
		return w
	}
	w := request(payload)
	var report researchapi.A02Report
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &report) != nil ||
		report.Notice.ID != id || report.Notice.Version != 3 || report.ModelCallsUsed != 0 || len(report.Facts) != 1 ||
		report.Facts[0].Citation.EvidenceRef != evidenceID || report.Facts[0].Citation.Quote != "虚构测试建设有限公司" {
		t.Fatalf("unexpected evidence report: %d %s", w.Code, w.Body.String())
	}
	admit = false
	if w := request(payload); w.Code != http.StatusForbidden || calls != 2 {
		t.Fatalf("research denial: %d %s, calls=%d", w.Code, w.Body.String(), calls)
	}
	if w := request(strings.Replace(payload, "root", "mock-cell", 1)); w.Code != http.StatusBadRequest || calls != 2 {
		t.Fatalf("Cell was accepted as org unit: %d %s", w.Code, w.Body.String())
	}
}
