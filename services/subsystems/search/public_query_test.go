package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/search/api"
)

const deniedFixtureText = "只存在于未授权索引响应的保密片段"

type publicAuthorityFunc func(context.Context, api.PublicQuery, []api.PublicRef) ([]api.PublicHit, error)

func (f publicAuthorityFunc) AuthorizePublicSearch(ctx context.Context, query api.PublicQuery, refs []api.PublicRef) ([]api.PublicHit, error) {
	return f(ctx, query, refs)
}

func publicUser() reqctx.Context {
	return reqctx.Context{
		Principal: reqctx.Principal{ID: "user-a", Kind: "user"},
		Tenant:    "tenant-a", Cell: "cell-01", Purpose: "interactive",
	}
}

func publicFixtureRef(id string) api.PublicRef {
	return api.PublicRef{Kind: api.PublicFact, Domain: "procurement", ID: id, Version: "3"}
}

func putPublicJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func publicAliasFixture(t *testing.T, w http.ResponseWriter, index, alias, activation string) {
	t.Helper()
	putPublicJSON(t, w, map[string]any{index: map[string]any{
		"aliases": map[string]any{alias: map[string]any{"filter": map[string]any{
			"match_all": map[string]string{"_name": activation},
		}}},
	}})
}

func TestPublicSearchOnlyReturnsCurrentAuthorizedDTOs(t *testing.T) {
	generation := randomPublicID()
	physical, alias := "mq-public-test-"+generation, "mq-public-test-current"
	var activation atomic.Value
	activation.Store(generation + ":1795068000000")
	refs := []api.PublicRef{
		publicFixtureRef("hidden-before"), publicFixtureRef("visible-a"),
		publicFixtureRef("revoke-next"), publicFixtureRef("hidden-end"),
	}
	pit := "private-opensearch-pit"
	var findCalls atomic.Int64
	var expirePIT, hideAll atomic.Bool
	authority, admitted := error(nil), publicUser()
	allowed := map[api.PublicRef]bool{refs[1]: true, refs[2]: true}
	authorityCalls := 0
	extraRef := api.PublicRef{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + alias + "/_alias/" + alias:
			publicAliasFixture(t, w, physical, alias, activation.Load().(string))
		case "/" + physical + "/_search/point_in_time":
			if r.Method != http.MethodPost || r.URL.Query().Get("allow_partial_pit_creation") != "false" {
				t.Error("request created an unbounded or partial PIT")
			}
			putPublicJSON(t, w, map[string]string{"pit_id": pit})
		case "/_search":
			findCalls.Add(1)
			if expirePIT.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var query struct {
				After  []json.RawMessage   `json:"search_after"`
				Source []string            `json:"_source"`
				Query  json.RawMessage     `json:"query"`
				PIT    struct{ ID string } `json:"pit"`
				Total  bool                `json:"track_total_hits"`
			}
			if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("allow_partial_search_results") != "false" || query.Total || query.PIT.ID != pit ||
				len(query.Source) != 1 || query.Source[0] != "ref" {
				t.Error("search requested unnecessary or incomplete candidate data")
			}
			for _, field := range []string{`"领航项目"`, `"identifiers":"P-01"`, `"ref.kind":"fact"`,
				`"ref.domain":"procurement"`, `"region":"340000"`, `"notice_type":"award"`,
				`"withdrawn":false`, `"published_at":{"gte":"2026-11-01T00:00:00Z"}`} {
				if !bytes.Contains(query.Query, []byte(field)) {
					t.Errorf("missing query constraint %s in %s", field, query.Query)
				}
			}
			docs := make([]map[string]any, 0, len(refs))
			after := len(query.After) != 0
			for _, ref := range refs {
				if hideAll.Load() {
					break
				}
				key := publicKey(ref)
				if after {
					if string(query.After[1]) == `"`+key+`"` {
						after = false
					}
					continue
				}
				docs = append(docs, map[string]any{
					"_source":   map[string]any{"ref": ref, "text": deniedFixtureText},
					"highlight": map[string][]string{"text": {deniedFixtureText}},
					"sort":      []any{1.0, key},
				})
			}
			putPublicJSON(t, w, map[string]any{"hits": map[string]any{"hits": docs, "total": 937},
				"aggregations": map[string]string{"sensitive_bucket": deniedFixtureText}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	index, err := NewPublicOpenSearch(srv.URL, "test", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	searcher, err := NewPublicSearcher(index, publicAuthorityFunc(func(ctx context.Context, query api.PublicQuery, candidates []api.PublicRef) ([]api.PublicHit, error) {
		authorityCalls++
		if query.Text != "领航项目" || query.Identifier != "P-01" {
			t.Error("authority did not receive the query that discovered the candidates")
		}
		if scope, _ := reqctx.From(ctx); scope != admitted {
			return nil, api.ErrPublicDenied
		}
		hits := []api.PublicHit{}
		for _, ref := range candidates {
			if allowed[ref] {
				hits = append(hits, api.PublicHit{Ref: ref, Excerpt: "权威许可的摘录"})
			}
		}
		if extraRef != (api.PublicRef{}) {
			hits = append(hits, api.PublicHit{Ref: extraRef, Excerpt: deniedFixtureText})
		}
		return hits, authority
	}), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}

	base := publicUser()
	ctx := reqctx.With(t.Context(), base)
	from := time.Date(2026, 11, 1, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	initial := api.PublicSearchRequest{PageSize: 1, Query: api.PublicQuery{
		Text: "领航项目", Identifier: "P-01", Kind: api.PublicFact, Domain: "procurement",
		Region: "340000", NoticeType: "award", From: &from,
	}}
	first, err := searcher.Search(ctx, initial)
	if err != nil || len(first.Hits) != 1 || first.Hits[0].Ref != refs[1] ||
		first.Hits[0].Excerpt != "权威许可的摘录" || first.NextCursor == "" || first.IndexedAt.UnixMilli() != 1795068000000 {
		t.Fatalf("initial authorized page: %+v, %v", first, err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{deniedFixtureText, "hidden-before", "hidden-end", "revoke-next", pit, `"total"`, "sensitive_bucket"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("page leaked %q", private)
		}
	}
	next := initial
	next.Cursor = first.NextCursor
	for _, change := range []func(*reqctx.Context){
		func(rc *reqctx.Context) { rc.Tenant = "tenant-b" },
		func(rc *reqctx.Context) { rc.Cell = "cell-02" },
		func(rc *reqctx.Context) { rc.Principal.ID = "user-b" },
		func(rc *reqctx.Context) { rc.Purpose = "simulation" },
	} {
		other := base
		change(&other)
		if _, err := searcher.Search(reqctx.With(ctx, other), next); !errors.Is(err, api.ErrPublicCursor) {
			t.Fatalf("cursor reused in scope %+v: %v", other, err)
		}
	}
	changedQuery := next
	changedQuery.Query.Text += "其他"
	if _, err := searcher.Search(ctx, changedQuery); !errors.Is(err, api.ErrPublicCursor) {
		t.Fatalf("cursor reused for different query: %v", err)
	}
	tampered := next
	tampered.Cursor = "7" + tampered.Cursor
	if _, err := searcher.Search(ctx, tampered); !errors.Is(err, api.ErrPublicCursor) || findCalls.Load() != 1 || authorityCalls != 1 {
		t.Fatalf("invalid cursor reached an authority or query: calls=%d/%d, err=%v", findCalls.Load(), authorityCalls, err)
	}
	expired, err := searcher.(*publicSearcher).decode(next.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	expired.Expires = time.Now().Add(-time.Second)
	expiredCursor := next
	expiredCursor.Cursor, err = searcher.(*publicSearcher).encode(expired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := searcher.Search(ctx, expiredCursor); !errors.Is(err, api.ErrPublicCursor) {
		t.Fatalf("expired cursor: %v", err)
	}
	delete(allowed, refs[2])
	second, err := searcher.Search(ctx, next)
	if err != nil || len(second.Hits) != 0 || second.NextCursor != "" || authorityCalls != 2 {
		t.Fatalf("revoked lookahead was reused or leaked: %+v %v", second, err)
	}
	last, err := searcher.Search(ctx, initial)
	if err != nil || len(last.Hits) != 1 || last.NextCursor != "" {
		t.Fatalf("denied remainder produced continuation: %+v %v", last, err)
	}
	different := base
	different.Tenant = "tenant-b"
	if page, err := searcher.Search(reqctx.With(ctx, different), initial); !errors.Is(err, api.ErrPublicDenied) || len(page.Hits) != 0 {
		t.Fatalf("cross-tenant first page: %+v %v", page, err)
	}
	authority = errors.New(deniedFixtureText)
	if page, err := searcher.Search(ctx, initial); !errors.Is(err, api.ErrPublicUnavailable) || len(page.Hits) != 0 ||
		strings.Contains(err.Error(), deniedFixtureText) {
		t.Fatalf("authority outage returned partial data or details: %+v %v", page, err)
	}
	authority = nil
	extraRef = refs[1]
	extraRef.Version = "wrong-version"
	if page, err := searcher.Search(ctx, initial); !errors.Is(err, api.ErrPublicUnavailable) || len(page.Hits) != 0 {
		t.Fatalf("authority returned unknown fixed version: %+v %v", page, err)
	}
	extraRef = api.PublicRef{}
	expirePIT.Store(true)
	if _, err := searcher.Search(ctx, next); !errors.Is(err, api.ErrPublicCursor) {
		t.Fatalf("evicted PIT cursor: %v", err)
	}
	expirePIT.Store(false)
	activation.Store(randomPublicID() + ":1795068000000")
	if _, err := searcher.Search(ctx, next); !errors.Is(err, api.ErrPublicCursor) {
		t.Fatalf("cursor survived an alias reactivation: %v", err)
	}
	hideAll.Store(true) // an empty candidate page still needs request admission
	authority = api.ErrPublicDenied
	if _, err := searcher.Search(ctx, initial); !errors.Is(err, api.ErrPublicDenied) {
		t.Fatalf("empty page skipped admission: %v", err)
	}
}
