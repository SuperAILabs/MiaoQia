package dataread

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

type localFixture struct{ dataapi.UnimplementedHandler }

func (localFixture) GetFact(ctx context.Context, req *dataapi.FactRequest) (dataapi.NilFactRead, error) {
	var result dataapi.NilFactRead
	if _, exists := reqctx.From(ctx); !exists {
		return result, errors.New("fixture missing request context")
	}
	if req.FactID == "ntc_absent" {
		result.SetToNull()
		return result, nil
	}
	if req.FactID == "ntc_denied" {
		return result, &dataapi.ProblemStatusCode{
			StatusCode: http.StatusForbidden, Response: dataapi.Problem{Title: "request rejected", Status: 403},
		}
	}
	result.SetTo(dataapi.FactRead{Basis: req.Basis, Fact: dataapi.Fact{ID: req.FactID, Version: 2}})
	return result, nil
}

func (localFixture) NewError(_ context.Context, err error) *dataapi.ProblemStatusCode {
	var problem *dataapi.ProblemStatusCode
	if errors.As(err, &problem) {
		return problem
	}
	return &dataapi.ProblemStatusCode{StatusCode: 503, Response: dataapi.Problem{Title: "unavailable", Status: 503}}
}

func TestLocalAndRemoteUseSameReadContract(t *testing.T) {
	owner := localFixture{}
	local, err := NewLocal(owner)
	if err != nil {
		t.Fatal(err)
	}
	server, err := dataapi.NewServer(owner)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, err := reqctx.Decode(r.Header.Get(reqctx.Header))
		if err != nil {
			t.Errorf("remote context: %v", err)
			return
		}
		server.ServeHTTP(w, r.WithContext(reqctx.With(r.Context(), rc)))
	}))
	defer httpServer.Close()
	remote, err := NewWithHTTPClient(httpServer.URL, &http.Client{Transport: binding.Transport{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := readContext()
	basis := Basis{View: "current"}
	for name, reader := range map[string]Reader{"local": local, "remote": remote} {
		t.Run(name, func(t *testing.T) {
			value, err := reader.ReadFact(ctx, factRequest("ntc_fixture", basis, nil))
			if err != nil || value == nil || value.Fact.Version != 2 {
				t.Fatalf("expected fixture, got %#v, %v", value, err)
			}
			value, err = reader.ReadFact(ctx, factRequest("ntc_absent", basis, nil))
			if err != nil || value != nil {
				t.Fatalf("expected null, got %#v, %v", value, err)
			}
			var problem *Error
			_, err = reader.ReadFact(ctx, factRequest("ntc_denied", basis, nil))
			if !errors.As(err, &problem) || problem.Status != http.StatusForbidden {
				t.Fatalf("expected same denial, got %v", err)
			}
			_, err = reader.ReadFact(ctx, factRequest("ntc_fixture", Basis{View: "known_at"}, nil))
			if err == nil {
				t.Fatal("accepted known_at without cutoff")
			}
		})
	}
}

// The Python synthetic publisher test supplies a PostgreSQL-backed mTLS listener
// and short-lived fixture certificates for this Go-process side of the check.
func TestAgainstPythonListener(t *testing.T) {
	base, factID, evidenceID := os.Getenv("MIAOQIA_TEST_DATAREAD_URL"), os.Getenv("MIAOQIA_TEST_DATAREAD_FACT"), os.Getenv("MIAOQIA_TEST_DATAREAD_EVIDENCE")
	if base == "" {
		t.Skip("run via the Python real PostgreSQL fixture with MIAOQIA_TEST_DATAREAD_GO=1")
	}
	client, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, basis := readContext(), Basis{View: "current"}.dto()
	fact, err := client.ReadFact(ctx, factRequest(factID, Basis{View: "current"}, new(3)))
	if err != nil || fact == nil || fact.Fact.Version != 3 {
		t.Fatalf("get-fact: %#v, %v", fact, err)
	}
	history, err := client.ReadFactHistory(ctx, dataapi.FactHistoryRequest{FactID: factID, Basis: basis, Limit: 2})
	if err != nil || len(history.Items) != 2 || history.NextCursor.Or("") != "2" {
		t.Fatalf("fact-history: %#v, %v", history, err)
	}
	query, err := client.ReadFacts(ctx, dataapi.QueryFactsRequest{InfoDomain: "INF-PROC", ObjectType: "tendering.notice", Basis: basis, Limit: 10})
	if err != nil || len(query.Items) != 1 || query.Items[0].ID != factID {
		t.Fatalf("query-facts: %#v, %v", query, err)
	}
	units, err := client.ReadBidResultUnits(ctx, factRequest(factID, Basis{View: "current"}, new(3)))
	if err != nil || len(units.Items) != 1 || units.Items[0].Subject.Version != 3 {
		t.Fatalf("bid-result-units: %#v, %v", units, err)
	}
	evidence, err := client.ReadEvidence(ctx, dataapi.EvidenceRequest{EvidenceRef: evidenceID})
	if err != nil || evidence == nil || evidence.Evidence.Snippet.Or("") == "" {
		t.Fatalf("resolve-evidence: %#v, %v", evidence, err)
	}
	chunk, err := client.ReadNoticeChunk(ctx, dataapi.NoticeChunkRequest{FactID: factID, Version: 3, Ordinal: 0})
	if err != nil || chunk == nil || chunk.Text == "" || chunk.SnapshotRef == "" {
		t.Fatalf("get-notice-chunk: %#v, %v", chunk, err)
	}
}

func readContext() context.Context {
	return reqctx.With(context.Background(), reqctx.Context{
		Tenant: "mock-tenant", Cell: "mock-cell", Principal: reqctx.Principal{ID: "mock-research-user", Kind: "user"}, Purpose: "interactive",
	})
}
