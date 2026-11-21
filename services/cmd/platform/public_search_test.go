package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	searchapi "github.com/superailabs/miaoqia/services/subsystems/search/api"
)

const (
	testPublicTenant   = "tnt_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testPublicNotice   = "ntc_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testPublicFact     = "pp_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testPublicChunk    = testPublicNotice + "~00"
	testPublicSnapshot = "snp_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testPublicOrg      = "org_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testPublicUSCC     = "91000000000000000K"
	testPublicSubject  = "public-search-test-user"
	testGeneration     = "00000000000000000000000000000001"
)

var testSearchLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func configureSearchTest(t *testing.T) {
	t.Helper()
	t.Setenv("MIAOQIA_ENV", "production")
	t.Setenv("MIAOQIA_DEPLOY_DIR", filepath.Join("..", "..", "..", "deploy"))
	t.Setenv(publicSearchPrefix+"NAMESPACE", "test")
	t.Setenv("MIAOQIA_BINDING_IDENTITY", "local")
	t.Setenv("MIAOQIA_SEARCH_DB_DSN", "postgres://sea_app@127.0.0.1:1/sea?sslmode=disable")
}

type searchTLSFixture struct {
	t   *testing.T
	dir string
}

func newSearchTLS(t *testing.T) searchTLSFixture {
	t.Helper()
	dir := t.TempDir()
	runSearchCertTool(t, "bash", filepath.Join("..", "..", "..", "tools", "dev", "m0_test_certificates.sh"), dir)
	return searchTLSFixture{t, dir}
}

func runSearchCertTool(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", name, err, out)
	}
}

func (f searchTLSFixture) peer(name string) (string, string) {
	f.t.Helper()
	base := filepath.Join(f.dir, name)
	if err := os.WriteFile(base+".ext", []byte("subjectAltName=DNS:localhost,IP:127.0.0.1,URI:spiffe://miaoqia/service/"+name+
		"\nextendedKeyUsage=clientAuth,serverAuth\n"), 0600); err != nil {
		f.t.Fatal(err)
	}
	runSearchCertTool(f.t, "openssl", "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1",
		"-nodes", "-keyout", base+".key", "-out", base+".csr", "-subj", "/CN="+name)
	runSearchCertTool(f.t, "openssl", "x509", "-req", "-in", base+".csr", "-CA", filepath.Join(f.dir, "ca.crt"),
		"-CAkey", filepath.Join(f.dir, "ca.key"), "-CAcreateserial", "-out", base+".crt", "-days", "1", "-extfile", base+".ext")
	return base + ".crt", base + ".key"
}

func (f searchTLSFixture) configure(prefix, peer string) {
	f.t.Helper()
	cert, key := f.peer(peer)
	f.t.Setenv(prefix+"_CA", filepath.Join(f.dir, "ca.crt"))
	f.t.Setenv(prefix+"_CERT", cert)
	f.t.Setenv(prefix+"_KEY", key)
}

func (f searchTLSFixture) server(peer, clientPeer string, handler http.Handler) *httptest.Server {
	f.t.Helper()
	certPath, keyPath := f.peer(peer)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		f.t.Fatal(err)
	}
	roots, err := transportsecurity.Roots(filepath.Join(f.dir, "ca.crt"))
	if err != nil {
		f.t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids := r.TLS.PeerCertificates[0].URIs
		if len(r.TLS.VerifiedChains) == 0 || len(ids) != 1 || ids[0].String() != "spiffe://miaoqia/service/"+clientPeer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	server.TLS = &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert,
	}
	server.StartTLS()
	f.t.Cleanup(server.Close)
	return server
}

func replySearchJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func checkSearchFixtureContext(t *testing.T, ctx context.Context) {
	t.Helper()
	rc, ok := reqctx.From(ctx)
	if !ok || rc != (reqctx.Context{
		Tenant: testPublicTenant, Cell: "cell-01", Purpose: "interactive",
		Principal: reqctx.Principal{ID: testPublicSubject, Kind: "user"},
	}) {
		t.Errorf("outbound reqctx = %+v", rc)
	}
}

func searchRouteFixture(t *testing.T, refs []searchapi.PublicRef, queries *atomic.Int64) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mq-public-test-current/_alias/mq-public-test-current":
			_, _ = io.WriteString(w, `{"mq-public-test-`+testGeneration+`":{"aliases":{"mq-public-test-current":{"filter":{"match_all":{"_name":"`+testGeneration+`:1795068000000"}}}}}}`)
		case "/mq-public-test-" + testGeneration + "/_search/point_in_time":
			replySearchJSON(w, map[string]string{"pit_id": "search-pit"})
		case "/_search":
			queries.Add(1)
			var body struct {
				Source []string `json:"_source"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Source) != 1 || body.Source[0] != "ref" {
				t.Error("OpenSearch should receive _source=[ref]")
			}
			hits := make([]any, 0, len(refs))
			for _, ref := range refs {
				identity, _ := json.Marshal([]string{string(ref.Kind), ref.Domain, ref.ID})
				key := sha256.Sum256(identity)
				hits = append(hits, map[string]any{
					"_source": map[string]any{"ref": ref, "text": "index-only-private-term", "title": "index-private-title"},
					"sort":    []any{1, hex.EncodeToString(key[:])},
				})
			}
			replySearchJSON(w, map[string]any{"hits": map[string]any{"hits": hits}})
		default:
			t.Errorf("unexpected OpenSearch call %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestPublicSearchAuthenticatedRoute(t *testing.T) {
	configureSearchTest(t)
	certs := newSearchTLS(t)
	certs.configure("MIAOQIA_INTERNAL_TLS", "platform-public-search-api")
	certs.configure(publicSearchPrefix+"OPENSEARCH_TLS", "public-index-reader")
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-public-search-api")
	t.Setenv(publicSearchPrefix+"CURSOR_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv(publicSearchPrefix+"DISCOVERY_SOURCES", `["synthetic"]`)

	issuer := testissuer.New(t)
	t.Setenv("MIAOQIA_GATEWAY_ISSUER", issuer.Server.URL)
	t.Setenv("MIAOQIA_GATEWAY_AUDIENCE", "miaoqia-api")
	token := issuer.Token(t, testPublicSubject, testPublicTenant, "cell-01", false)
	var membershipRemoved atomic.Bool
	identity := certs.server("platform-api", "platform-public-search-api", binding.InternalServer("identity", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSearchFixtureContext(t, r.Context())
		if r.URL.Path != "/internal/v1/identity/session" {
			t.Errorf("unexpected identity route: %s", r.URL.Path)
		}
		state := "active"
		if membershipRemoved.Load() {
			state = "removed"
		}
		replySearchJSON(w, map[string]any{
			"tenant": testPublicTenant, "cell": "cell-01", "subject": testPublicSubject, "roles": []string{},
			"state": state, "tenant_version": 1, "member_version": 1,
		})
	})))
	t.Setenv(publicSearchPrefix+"IDENTITY_URL", identity.URL+"/internal")

	var ownerMode atomic.Value
	ownerMode.Store("allowed")
	var callsMu sync.Mutex
	calls := make(map[string][]int64)
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSearchFixtureContext(t, r.Context())
		switch r.URL.Path {
		case "/internal/v1/data/get-notice-chunk":
			readSearchChunk(t, w, r, ownerMode.Load().(string))
			return
		case "/internal/v1/data/get-public-organization":
			readSearchOrganization(t, w, r, ownerMode.Load().(string))
			return
		}
		var request struct {
			ID      string `json:"fact_id"`
			Version *int64 `json:"version"`
			Basis   struct {
				View   string `json:"view"`
				Cutoff any    `json:"recorded_cutoff"`
			} `json:"basis"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Basis.View != "current" ||
			request.Basis.Cutoff != nil || r.URL.Path != "/internal/v1/data/get-fact" ||
			(request.ID != testPublicNotice && request.ID != testPublicFact) {
			t.Errorf("unexpected data-api request: %v %s %+v", err, r.URL.Path, request)
		}
		version := int64(0)
		if request.Version != nil {
			version = *request.Version
		}
		callsMu.Lock()
		calls[request.ID] = append(calls[request.ID], version)
		callsMu.Unlock()
		switch mode := ownerMode.Load(); mode {
		case "revoked":
			if version == 0 {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"title":"request rejected","status":403}`)
				return
			}
		case "unavailable":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"title":"unavailable","status":503}`)
			return
		case "null":
			replySearchJSON(w, nil)
			return
		case "unexpected-peer":
			t.Error("untrusted data-api received an authenticated request")
		}
		returnedVersion := int64(1)
		if ownerMode.Load() == "stale" && version == 0 && request.ID == testPublicNotice {
			returnedVersion = 2
		}
		tags := []string{"public:synthetic"}
		if ownerMode.Load() == "private" {
			tags = []string{"restricted:synthetic"}
		}
		if ownerMode.Load() == "outside-discovery" {
			tags = []string{"public:synthetic", "public:unapproved-source"}
		}
		replySearchJSON(w, map[string]any{
			"basis": map[string]any{"view": "current", "recorded_cutoff": nil},
			"fact": map[string]any{
				"id": request.ID, "version": returnedVersion, "classification": "public", "info_domain": "INF-PROC",
				"title": map[string]string{"value": "合成安徽工程公告"}, "rights_tags": tags,
				"location": map[string]string{"code": "340000"}, "notice_type": "award",
				"notice_code": map[string]string{"value": "AH-001"}, "project_code": map[string]string{"value": "AH-002"},
				"published_at": map[string]string{"value": "2026-11-19T00:00:00Z"},
			},
		})
	})
	owner := certs.server("data-api", "platform-public-search-api", binding.InternalServer("data-api", read))
	t.Setenv(publicSearchPrefix+"DATA_API_URL", owner.URL)
	refs := []searchapi.PublicRef{
		{Kind: searchapi.PublicNotice, Domain: "INF-PROC", ID: testPublicNotice, Version: "1"},
		{Kind: searchapi.PublicFact, Domain: "INF-PROC", ID: testPublicFact, Version: "1"},
		{Kind: searchapi.PublicChunk, Domain: "INF-PROC", ID: testPublicChunk, Version: "1"},
		{Kind: searchapi.PublicEntity, Domain: "INF-ORG", ID: testPublicOrg, Version: "1"},
	}
	var queries atomic.Int64
	index := certs.server("mock-opensearch", "public-index-reader", searchRouteFixture(t, refs, &queries))
	t.Setenv(publicSearchPrefix+"OPENSEARCH_URL", index.URL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	buildHandler := func() http.Handler {
		t.Helper()
		modules, envs, closeAll, err := assemble(ctx, testSearchLog, publicSearchAPI, nil)
		t.Cleanup(closeAll)
		if err != nil {
			t.Fatal(err)
		}
		if len(modules) != 1 || envs[0].DB != nil || envs[0].Cell != "" || envs[0].DataScope != "public" {
			t.Fatalf("unexpected public API modules and storage: %v", len(modules))
		}
		if _, ok := runtime.Lookup[searchapi.Percolator](envs[0].Bindings, searchapi.PercolatorBinding); ok {
			t.Fatal("public API loaded Cell percolation")
		}
		public, internal, err := apiHandlers(ctx, modules, envs, false)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		internal.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/search", nil))
		if w.Code != http.StatusNotFound {
			t.Fatal("public search leaked onto the internal mux")
		}
		return public
	}
	handler := buildHandler()
	call := func(handler http.Handler, bearer, payload string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		r.Header.Set(reqctx.Header, reqctx.Encode(reqctx.Context{
			Tenant: "forged-tenant", Cell: "cell-99", Purpose: "monitoring", Principal: reqctx.Principal{ID: "svc:detection", Kind: "service"},
		}))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	valid := `{"query":{"text":"安徽工程","region":"340000","from":"2026-11-19T00:00:00Z","before":"2026-11-20T00:00:00Z"},"page_size":10}`
	invalid := `{"query":{},"page_size":10,"tenant":"forged-tenant"}`
	expectSearchStatus(t, call(handler, "", valid), http.StatusUnauthorized, "unauthenticated")
	expectSearchStatus(t, call(handler, token+"tampered", valid), http.StatusUnauthorized, "unauthenticated")
	noTenant := issuer.Token(t, testPublicSubject, "", "", false)
	expectSearchStatus(t, call(handler, noTenant, valid), http.StatusForbidden, "search_denied")
	membershipRemoved.Store(true)
	expectSearchStatus(t, call(handler, token, valid), http.StatusForbidden, "invalid_tenant_context")
	membershipRemoved.Store(false)
	for _, body := range []string{invalid, `{"query":null,"page_size":1}`, `{"query":{},"page_size":"1"}`,
		`{"query":{},"page_size":1} {}`, `{"query":{},"page_size":0}`, `{"query":{"principal":"svc:detection"},"page_size":1}`} {
		expectSearchStatus(t, call(handler, token, body), http.StatusBadRequest, "invalid_search_request")
	}
	if queries.Load() != 0 {
		t.Fatal("rejected requests reached the index")
	}
	w := call(handler, token, valid)
	assertSearchHits(t, w, testPublicNotice, testPublicFact)
	callsMu.Lock()
	for _, id := range []string{testPublicNotice, testPublicFact} {
		values := calls[id]
		if len(values) != 2 || values[0] != 1 || values[1] != 0 {
			t.Errorf("%s owner reads = %v, want fixed version followed by current", id, values)
		}
	}
	callsMu.Unlock()

	assertSearchHits(t, call(handler, token, `{"query":{"text":"index-only-private-term"},"page_size":10}`))
	assertSearchHits(t, call(handler, token, `{"query":{"identifier":"index-only-code"},"page_size":10}`))
	assertSearchHits(t, call(handler, token, `{"query":{"identifier":"AH-001","kind":"notice"},"page_size":10}`), testPublicNotice)
	chunkQuery := `{"query":{"text":"证据片段","kind":"chunk","identifier":"AH-001","region":"340000","notice_type":"award"},"page_size":10}`
	chunk := call(handler, token, chunkQuery)
	var chunkPage searchapi.PublicSearchPage
	if err := json.Unmarshal(chunk.Body.Bytes(), &chunkPage); err != nil || chunk.Code != http.StatusOK || len(chunkPage.Hits) != 1 {
		t.Fatalf("chunk response (%d): %s", chunk.Code, chunk.Body)
	}
	hit := chunkPage.Hits[0]
	if hit.Ref.ID != testPublicChunk || hit.SnapshotRef != testPublicSnapshot || hit.Text != "证据片段" ||
		hit.Locator == nil || hit.Locator.Block != 1 || hit.Locator.CharStart != 2 || hit.Locator.CharEnd != 6 ||
		strings.Contains(chunk.Body.String(), "index-") {
		t.Fatalf("unexpected confirmed chunk: %+v", hit)
	}
	orgQuery := `{"query":{"text":"登记企业","identifier":"` + testPublicUSCC + `","kind":"entity","domain":"INF-ORG"},"page_size":10}`
	org := call(handler, token, orgQuery)
	var orgPage searchapi.PublicSearchPage
	if err := json.Unmarshal(org.Body.Bytes(), &orgPage); err != nil || org.Code != http.StatusOK || len(orgPage.Hits) != 1 ||
		orgPage.Hits[0] != (searchapi.PublicHit{Ref: refs[3], Title: "合成登记企业"}) || strings.Contains(org.Body.String(), "index-") {
		t.Fatalf("unexpected organization response (%d): %s", org.Code, org.Body)
	}
	ownerMode.Store("stale")
	assertSearchHits(t, call(handler, token, valid), testPublicFact)
	assertSearchHits(t, call(handler, token, chunkQuery))
	assertSearchHits(t, call(handler, token, orgQuery))
	ownerMode.Store("null")
	assertSearchHits(t, call(handler, token, valid))
	ownerMode.Store("private")
	assertSearchHits(t, call(handler, token, valid))
	ownerMode.Store("outside-discovery")
	assertSearchHits(t, call(handler, token, valid))
	assertSearchHits(t, call(handler, token, chunkQuery))
	assertSearchHits(t, call(handler, token, orgQuery))
	ownerMode.Store("revoked")
	expectSearchStatus(t, call(handler, token, valid), http.StatusForbidden, "search_denied")
	expectSearchStatus(t, call(handler, token, chunkQuery), http.StatusForbidden, "search_denied")
	expectSearchStatus(t, call(handler, token, orgQuery), http.StatusForbidden, "search_denied")
	ownerMode.Store("invalid-chunk")
	expectSearchStatus(t, call(handler, token, chunkQuery), http.StatusServiceUnavailable, "search_unavailable")
	ownerMode.Store("unavailable")
	expectSearchStatus(t, call(handler, token, valid), http.StatusServiceUnavailable, "search_unavailable")
	ownerMode.Store("allowed")

	wrongOwner := certs.server("incorrect-data-owner", "platform-public-search-api", read)
	t.Setenv(publicSearchPrefix+"DATA_API_URL", wrongOwner.URL)
	ownerMode.Store("unexpected-peer")
	expectSearchStatus(t, call(buildHandler(), token, valid), http.StatusServiceUnavailable, "search_unavailable")
	ownerMode.Store("allowed")
	t.Setenv(publicSearchPrefix+"DATA_API_URL", owner.URL)
	for _, configKey := range []string{"DATA_API_URL", "DISCOVERY_SOURCES"} {
		t.Run("missing_"+configKey, func(t *testing.T) {
			t.Setenv(publicSearchPrefix+configKey, "")
			_, _, closeAll, err := assemble(ctx, testSearchLog, publicSearchAPI, nil)
			defer closeAll()
			if err == nil {
				t.Fatalf("missing %s allowed public search startup", configKey)
			}
		})
	}
}

func readSearchOrganization(t *testing.T, w http.ResponseWriter, r *http.Request, mode string) {
	t.Helper()
	var request struct {
		OrgID   string `json:"org_id"`
		Version int    `json:"version"`
	}
	if err := decodeStrictJSON(r.Body, &request); err != nil || request.OrgID != testPublicOrg || request.Version != 1 {
		t.Errorf("unexpected organization request: %+v %v", request, err)
	}
	tags := []string{"public:synthetic"}
	switch mode {
	case "stale", "null", "private":
		replySearchJSON(w, nil)
		return
	case "outside-discovery":
		tags = []string{"public:unapproved-source"}
	case "revoked", "unavailable":
		status := http.StatusForbidden
		if mode == "unavailable" {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"title": "test error", "status": status})
		return
	}
	replySearchJSON(w, map[string]any{
		"org_id": request.OrgID, "version": request.Version, "name": "合成登记企业", "uscc": testPublicUSCC, "rights_tags": tags,
	})
}

func readSearchChunk(t *testing.T, w http.ResponseWriter, r *http.Request, mode string) {
	t.Helper()
	var request struct {
		FactID  string `json:"fact_id"`
		Version int    `json:"version"`
		Ordinal int    `json:"ordinal"`
	}
	if err := decodeStrictJSON(r.Body, &request); err != nil || request.FactID != testPublicNotice || request.Version != 1 || request.Ordinal != 0 {
		t.Errorf("unexpected chunk request: %+v %v", request, err)
	}
	tags, end := []string{"public:synthetic"}, 6
	switch mode {
	case "stale", "null", "private":
		replySearchJSON(w, nil)
		return
	case "outside-discovery":
		tags = []string{"public:unapproved-source"}
	case "invalid-chunk":
		end++
	case "revoked", "unavailable":
		status := http.StatusForbidden
		if mode == "unavailable" {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"title": "test error", "status": status})
		return
	}
	replySearchJSON(w, map[string]any{
		"fact_id": request.FactID, "version": request.Version, "ordinal": request.Ordinal,
		"snapshot_ref": testPublicSnapshot, "locator": map[string]int{"block": 1, "char_start": 2, "char_end": end},
		"text": "证据片段", "rights_tags": tags,
		"metadata": map[string]any{"identifiers": []string{testPublicNotice, "AH-001"}, "region": "340000", "notice_type": "award"},
	})
}

func expectSearchStatus(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || w.Header().Get("Content-Type") != "application/problem+json" ||
		!strings.Contains(w.Body.String(), `"code":"`+code+`"`) || strings.Contains(w.Body.String(), "index-private") {
		t.Fatalf("status=%d body=%s, want %d %s", w.Code, w.Body, status, code)
	}
}

func assertSearchHits(t *testing.T, w *httptest.ResponseRecorder, ids ...string) {
	t.Helper()
	var page searchapi.PublicSearchPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK ||
		w.Header().Get("Cache-Control") != "no-store" || page.Hits == nil || page.IndexedAt.IsZero() ||
		len(page.Hits) != len(ids) || page.NextCursor != "" {
		t.Fatalf("unexpected search response (%d): %s", w.Code, w.Body)
	}
	for i, hit := range page.Hits {
		if hit.Ref.ID != ids[i] || hit != (searchapi.PublicHit{Ref: hit.Ref}) {
			t.Fatalf("unexpected authority result: %+v", hit)
		}
	}
	if strings.Contains(w.Body.String(), "index-") || strings.Contains(w.Body.String(), "合成安徽") {
		t.Fatalf("search leaked index or owner text: %s", w.Body)
	}
}
