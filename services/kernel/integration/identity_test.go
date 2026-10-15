package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"github.com/superailabs/miaoqia/services/kernel/relay"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
)

// Real PostgreSQL + OpenFGA + HTTPS/JWT + generated HTTP + mTLS remote binding.
// The shared test issuer controls assurance claims; an actual IdP MFA ceremony remains deployment acceptance.
func TestIdentityLifecycleAndCell(t *testing.T) {
	fga := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if fga == "" {
		t.Skip("MIAOQIA_TEST_OPENFGA not set")
	}
	t.Setenv("MIAOQIA_ENV", "production")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database := testenv.NewDatabase(t)
	module := identity.Module()
	if err := db.Migrate(ctx, database.OwnerDSN, database.Name+"_app", module.Migrations); err != nil {
		t.Fatal(err)
	}
	// Applying migrations again must be harmless, including existing kernel data.
	if err := db.Migrate(ctx, database.OwnerDSN, database.Name+"_app", module.Migrations); err != nil {
		t.Fatal(err)
	}
	app, err := db.Open(ctx, database.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	bindings := runtime.NewBindings()
	env := &runtime.Env{DB: app, Bindings: bindings, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: func(string) string { return fga }}
	if err := module.Start(ctx, env); err != nil {
		t.Fatal(err)
	}
	keycloakCA := os.Getenv("MIAOQIA_GATEWAY_CA_FILE")
	issuer := testissuer.New(t)
	public, internal := http.NewServeMux(), http.NewServeMux()
	module.Routes(env, public, internal)
	local, _ := runtime.Lookup[api.SessionResolver](bindings, api.SessionBinding)
	secured, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", public, local)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(secured)
	defer server.Close()
	httpClient := server.Client()
	// Internal interface uses a genuine, allowlisted mTLS peer. Administrative commands are not mounted here.
	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	internServer := httptest.NewUnstartedServer(internal)
	internServer.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	internServer.StartTLS()
	defer internServer.Close()
	rem, err := remote.NewAuthorizer(internServer.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	remoteSession := rem.(api.SessionResolver)
	do := func(token, method, path, body string, keys ...string) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(method+path+body)))
		if len(keys) > 0 {
			key = keys[0]
		}
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set(reqctx.Header, reqctx.Encode(reqctx.Context{Tenant: "injected", Cell: "cell-02", Principal: reqctx.Principal{ID: "injected", Kind: "user"}}))
		r.Header.Set("Miaoqia-MFA", "true")
		response, err := httpClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		value, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, value
	}
	expect := func(token, method, path, body string, status int, keys ...string) []byte {
		t.Helper()
		code, value := do(token, method, path, body, keys...)
		if code != status {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, code, status, value)
		}
		return value
	}
	operator := "operator-" + database.Name
	admin := "admin-" + database.Name
	member := "member-" + database.Name
	if err := identity.Grant(ctx, fga, "user:"+operator, "operator", "platform:miaoqia"); err != nil {
		t.Fatal(err)
	}
	opToken := issuer.Token(t, operator, "", "", true)
	create := fmt.Sprintf(`{"name":"Test tenant","admin_subject":%q}`, admin)
	var tenant identityapi.Tenant
	if err := json.Unmarshal(expect(opToken, "POST", "/v1/tenants", create, 200, "create_request_001"), &tenant); err != nil {
		t.Fatal(err)
	}
	adminToken := issuer.Token(t, admin, tenant.ID, "cell-01", true)
	adminCtx := reqctx.With(ctx, reqctx.Context{Tenant: tenant.ID, Cell: "cell-01", Principal: reqctx.Principal{ID: admin, Kind: "user"}})
	version := func(value []byte) api.Session {
		var s api.Session
		if err := json.Unmarshal(value, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Run("create-is-idempotent", func(t *testing.T) {
		var again identityapi.Tenant
		_ = json.Unmarshal(expect(opToken, "POST", "/v1/tenants", create, 200, "create_request_001"), &again)
		if again.ID != tenant.ID {
			t.Fatal("duplicate tenant")
		}
		expect(opToken, "POST", "/v1/tenants", strings.Replace(create, "Test tenant", "Different", 1), 409, "create_request_001")
	})
	t.Run("concurrent-create-idempotency", func(t *testing.T) {
		body := strings.Replace(create, "Test tenant", "Concurrent tenant", 1)
		var wg sync.WaitGroup
		values := make(chan identityapi.Tenant, 2)
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, raw := do(opToken, "POST", "/v1/tenants", body, "concurrent_request_001")
				if code != 200 {
					t.Errorf("concurrent create status %d", code)
					return
				}
				var value identityapi.Tenant
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Error(err)
					return
				}
				values <- value
			}()
		}
		wg.Wait()
		close(values)
		id := ""
		n := 0
		for value := range values {
			if id != "" && id != value.ID {
				t.Fatal("same key created two tenants")
			}
			id = value.ID
			n++
		}
		if n != 2 {
			t.Fatal("missing concurrent response")
		}
		var events int
		if err := app.Pool.QueryRow(ctx, `SELECT count(*) FROM kernel_outbox WHERE envelope->>'type'='identity.tenant.created.v1' AND envelope->>'subject'=$1`, id).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if events != 1 {
			t.Fatal("duplicate tenant creation event")
		}
	})
	t.Run("authorization-and-mfa-negative", func(t *testing.T) {
		expect(opToken, "POST", "/v1/tenants", create, 400, "")
		expect(opToken, "POST", "/v1/tenants", create, 400, "short")
		expect(opToken, "PUT", "/v1/tenants/"+tenant.ID+"/state", `{"state":"suspended","expected_version":1}`, 409, "create_request_001")

		expect(issuer.Token(t, operator, "", "", false), "POST", "/v1/tenants", create, 403)
		expect(issuer.Token(t, "stranger", "", "", true), "POST", "/v1/tenants", create, 403)
		expect(adminToken, "POST", "/v1/tenants", create, 403)
		expect(opToken, "POST", "/v1/tenants", strings.TrimSuffix(create, "}")+`,"cell":"cell-02"}`, 400)
		expect(adminToken, "PUT", "/v1/identity/members/other", `{"state":"active","roles":["superadmin"],"expected_version":0}`, 400)
		expect(adminToken, "PUT", "/v1/identity/members/other", `{"state":"active","roles":["admin","admin"],"expected_version":0}`, 400)
		expect(issuer.Token(t, admin, tenant.ID, "cell-01", false), "PUT", "/v1/identity/members/other", `{"state":"active","roles":[],"expected_version":0}`, 403)
	})
	t.Run("cell-and-membership-negative", func(t *testing.T) {
		for _, token := range []string{issuer.Token(t, admin, tenant.ID, "cell-02", true), issuer.Token(t, admin, "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1", "cell-01", true), issuer.Token(t, "outsider", tenant.ID, "cell-01", true)} {
			expect(token, "GET", "/v1/identity/session", "", 403)

		}
		s := version(expect(adminToken, "GET", "/v1/identity/session?tenant=other&cell=cell-02", "", 200))
		if s.Tenant != tenant.ID || s.Cell != "cell-01" {
			t.Fatal("client routing override accepted")
		}
	})
	t.Run("last-admin-and-local-remote", func(t *testing.T) {
		expect(adminToken, "PUT", "/v1/identity/members/"+admin, `{"state":"removed","roles":[],"expected_version":1}`, 409)
		for _, resolver := range []api.SessionResolver{local, remoteSession} {
			s, err := resolver.Resolve(adminCtx)
			if err != nil || s.Tenant != tenant.ID {
				t.Fatalf("resolver %T: %v", resolver, err)
			}
		}
		decider, _ := runtime.Lookup[api.Authorizer](bindings, api.AuthorizerBinding)
		for _, checker := range []authz.Decider{decider, rem} {
			allowed, err := checker.Check(adminCtx, authz.Request{Relation: "can_manage_members", Object: "tenant:" + tenant.ID})
			if err != nil || !allowed {
				t.Fatalf("checker %T %v", checker, err)
			}
			allowed, err = checker.Check(adminCtx, authz.Request{Relation: "can_manage_members", Object: "tenant:other"})
			if err != nil || allowed {
				t.Fatal("cross tenant Check accepted")
			}
		}
	})
	t.Run("join-and-role-revocation-with-old-token", func(t *testing.T) {
		_ = version(expect(adminToken, "PUT", "/v1/identity/members/"+member, `{"state":"active","roles":["admin"],"expected_version":0}`, 200))
		token := issuer.Token(t, member, tenant.ID, "cell-01", true)
		expect(token, "PUT", "/v1/identity/members/somebody", `{"state":"active","roles":[],"expected_version":0}`, 200)
		expect(adminToken, "PUT", "/v1/identity/members/"+member, `{"state":"active","roles":["business_developer"],"expected_version":1}`, 200)
		expect(adminToken, "PUT", "/v1/identity/members/"+member, `{"state":"active","roles":["business_developer"],"expected_version":1}`, 200) // exact retry: same receipt, no second event/version bump
		// A stale persisted tuple must not restore a role removed from the SQL directory.
		if err := identity.Grant(ctx, fga, "user:"+member, "admin", "tenant:"+tenant.ID); err != nil {
			t.Fatal(err)
		}
		expect(token, "PUT", "/v1/identity/members/somebody", `{"state":"removed","roles":[],"expected_version":1}`, 403)
		expect(token, "PUT", "/v1/identity/members/somebody", `{"state":"active","roles":[],"expected_version":0}`, 403) // reauthorize before returning a stored receipt
		expect(adminToken, "PUT", "/v1/identity/members/"+member, `{"state":"removed","roles":[],"expected_version":1}`, 409)
		expect(adminToken, "PUT", "/v1/identity/members/"+member, `{"state":"removed","roles":[],"expected_version":2}`, 200)
		expect(token, "GET", "/v1/identity/session", "", 403)
		current := version(expect(adminToken, "GET", "/v1/identity/members/"+member, "", 200))
		if current.State != "removed" || current.MemberVersion != 3 {
			t.Fatal("cannot read current member version after conflict")
		}
		expect(adminToken, "GET", "/v1/identity/members/unknown-subject", "", 404)
		memberCtx := reqctx.With(ctx, reqctx.Context{Tenant: tenant.ID, Cell: "cell-01", Principal: reqctx.Principal{ID: member, Kind: "user"}})
		if _, err := remoteSession.Resolve(memberCtx); err == nil {
			t.Fatal("remote binding admits removed member")
		}
	})
	t.Run("suspension-and-resume", func(t *testing.T) {
		expect(adminToken, "PUT", "/v1/tenants/"+tenant.ID+"/state", `{"state":"suspended","expected_version":1}`, 403)
		expect(opToken, "PUT", "/v1/tenants/"+tenant.ID+"/state", `{"state":"suspended","expected_version":1}`, 200)
		expect(adminToken, "GET", "/v1/identity/session", "", 403)
		if _, err := remoteSession.Resolve(adminCtx); err == nil {
			t.Fatal("suspended tenant accepted internally")
		}
		expect(opToken, "PUT", "/v1/tenants/"+tenant.ID+"/state", `{"state":"active","expected_version":1}`, 409)
		expect(opToken, "PUT", "/v1/tenants/"+tenant.ID+"/state", `{"state":"active","expected_version":2}`, 200)
		expect(adminToken, "GET", "/v1/identity/session", "", 200)
		var replay identityapi.Tenant
		if err := json.Unmarshal(expect(opToken, "POST", "/v1/tenants", create, 200, "create_request_001"), &replay); err != nil {
			t.Fatal(err)
		}
		var current identityapi.Tenant
		if err := json.Unmarshal(expect(opToken, "GET", "/v1/tenants/"+tenant.ID, "", 200), &current); err != nil {
			t.Fatal(err)
		}
		if current.Version != 3 || current.State != "active" {
			t.Fatal("cannot read current tenant state/version")
		}
		expect(adminToken, "GET", "/v1/tenants/"+tenant.ID, "", 403)
		if replay.Version != 1 || replay.ID != tenant.ID {
			t.Fatal("idempotent create did not return its original response")
		}
	})
	t.Run("real-Keycloak-login-cell-and-revocation", func(t *testing.T) {
		kcIssuer := os.Getenv("MIAOQIA_TEST_KEYCLOAK")
		if kcIssuer == "" {
			t.Skip("MIAOQIA_TEST_KEYCLOAK not set")
		}
		roots, err := transportsecurity.Roots(keycloakCA)
		if err != nil {
			t.Fatal(err)
		}
		kcClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}
		login := func() string {
			return testenv.KeycloakToken(t, kcClient, kcIssuer, url.Values{"grant_type": {"password"}, "client_id": {"miaoqia-dev"}, "username": {"operator"}, "password": {"operator"}})
		}
		raw := login()
		claims := jwt.MapClaims{}
		if _, _, err := jwt.NewParser().ParseUnverified(raw, claims); err != nil {
			t.Fatal(err)
		}
		sub := claims["sub"].(string)
		adminGrant := testenv.KeycloakToken(t, kcClient, kcIssuer, url.Values{"grant_type": {"client_credentials"}, "client_id": {"miaoqia-integration-admin"}, "client_secret": {"isolated-integration-only"}})
		// Register admin-only profile fields: Keycloak 26 drops unmanaged attributes by default.
		profileURL := strings.TrimSuffix(kcIssuer, "/realms/miaoqia") + "/admin/realms/miaoqia/users/profile"
		profileReq, _ := http.NewRequest("GET", profileURL, nil)
		profileReq.Header.Set("Authorization", "Bearer "+adminGrant)
		profileRes, err := kcClient.Do(profileReq)
		if err != nil {
			t.Fatal(err)
		}
		var profile map[string]any
		if profileRes.StatusCode != 200 {
			profileRes.Body.Close()
			t.Fatalf("profile GET %d", profileRes.StatusCode)
		}
		err = json.NewDecoder(profileRes.Body).Decode(&profile)
		profileRes.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		attributes := profile["attributes"].([]any)
		for _, name := range []string{"miaoqia_tenant", "miaoqia_cell"} {
			found := false
			for _, attr := range attributes {
				if attr.(map[string]any)["name"] == name {
					found = true
				}
			}
			if !found {
				attributes = append(attributes, map[string]any{"name": name, "displayName": name, "permissions": map[string]any{"view": []string{"admin"}, "edit": []string{"admin"}}, "multivalued": false})
			}
		}
		profile["attributes"] = attributes
		profileBody, _ := json.Marshal(profile)
		profileReq, _ = http.NewRequest("PUT", profileURL, bytes.NewReader(profileBody))
		profileReq.Header.Set("Authorization", "Bearer "+adminGrant)
		profileReq.Header.Set("Content-Type", "application/json")
		profileRes, err = kcClient.Do(profileReq)
		if err != nil {
			t.Fatal(err)
		}
		profileRes.Body.Close()
		if profileRes.StatusCode != 200 {
			t.Fatalf("profile PUT %d", profileRes.StatusCode)
		}
		setAttrs := func(tenantID, cell string) {
			t.Helper()
			attrs := map[string]any{"miaoqia_tenant": []string{tenantID}, "miaoqia_cell": []string{cell}}
			if tenantID == "" {
				attrs = map[string]any{}
			}
			body, _ := json.Marshal(map[string]any{"username": "operator", "firstName": "本地", "lastName": "运营", "email": "operator@example.invalid", "emailVerified": true, "enabled": true, "attributes": attrs})
			// Clear attributes after this subtest, so other integration tests use platform-only operator tokens.
			realmURL := strings.TrimSuffix(kcIssuer, "/realms/miaoqia") + "/admin/realms/miaoqia/users/" + sub
			req, _ := http.NewRequest("PUT", realmURL, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+adminGrant)
			req.Header.Set("Content-Type", "application/json")
			response, err := kcClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 204 {
				b, _ := io.ReadAll(response.Body)
				t.Fatalf("Keycloak attributes %d: %s", response.StatusCode, b)
			}
		}
		var kt identityapi.Tenant
		body := fmt.Sprintf(`{"name":"Keycloak tenant","admin_subject":%q}`, sub)
		if err := json.Unmarshal(expect(opToken, "POST", "/v1/tenants", body, 200), &kt); err != nil {
			t.Fatal(err)
		}
		setAttrs(kt.ID, "cell-01")
		defer setAttrs("", "")
		t.Setenv("MIAOQIA_GATEWAY_CA_FILE", keycloakCA)
		handler, err := gateway.Public(ctx, kcIssuer, "miaoqia-api", public, local)
		if err != nil {
			t.Fatal(err)
		}
		kcServer := httptest.NewTLSServer(handler)
		defer kcServer.Close()
		getSession := func(token string, want int) {
			r, _ := http.NewRequest("GET", kcServer.URL+"/v1/identity/session", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			response, err := kcServer.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != want {
				b, _ := io.ReadAll(response.Body)
				t.Fatalf("real Keycloak session: %d want %d: %s", response.StatusCode, want, b)
			}
		}
		old := login()
		getSession(old, 200)
		setAttrs(kt.ID, "cell-02")
		getSession(login(), 403)
		setAttrs(kt.ID, "cell-01")
		getSession(login(), 200)
		// Management uses the independently tested signed-MFA fixture; real password-login tokens do not assert MFA.
		adminFixture := issuer.Token(t, sub, kt.ID, "cell-01", true)
		expect(adminFixture, "PUT", "/v1/identity/members/backup", `{"state":"active","roles":["admin"],"expected_version":0}`, 200)
		backup := issuer.Token(t, "backup", kt.ID, "cell-01", true)
		expect(backup, "PUT", "/v1/identity/members/"+sub, `{"state":"removed","roles":[],"expected_version":1}`, 200)
		getSession(old, 403)
		getSession(login(), 403)
	})
	t.Run("concurrent-last-admin-protection", func(t *testing.T) {
		second := "second-" + database.Name
		expect(adminToken, "PUT", "/v1/identity/members/"+second, `{"state":"active","roles":["admin"],"expected_version":0}`, 200)
		secondToken := issuer.Token(t, second, tenant.ID, "cell-01", true)
		var wg sync.WaitGroup
		codes := make(chan int, 2)
		for _, item := range [][2]string{{adminToken, admin}, {secondToken, second}} {
			wg.Add(1)
			go func(item [2]string) {
				defer wg.Done()
				code, _ := do(item[0], "PUT", "/v1/identity/members/"+item[1], `{"state":"removed","roles":[],"expected_version":1}`)
				codes <- code
			}(item)
		}
		wg.Wait()
		close(codes)
		success, conflict := 0, 0
		for c := range codes {
			if c == 200 {
				success++
			}
			if c == 409 {
				conflict++
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatalf("success %d conflict %d", success, conflict)
		}
	})
	t.Run("audit-failure-rolls-back", func(t *testing.T) {
		owner, err := pgx.Connect(ctx, database.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close(ctx)
		// Snapshot all durable effects before injecting a real database constraint failure.
		var before int
		if err := owner.QueryRow(ctx, "SELECT count(*) FROM kernel_outbox").Scan(&before); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `ALTER TABLE kernel_outbox ADD CONSTRAINT reject_identity_audit CHECK ((envelope->>'type') <> 'audit.entry.recorded.v1') NOT VALID`); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = owner.Exec(ctx, `ALTER TABLE kernel_outbox DROP CONSTRAINT reject_identity_audit`) }()
		badCreate := strings.Replace(create, "Test tenant", "Rollback tenant", 1)
		response := expect(opToken, "POST", "/v1/tenants", badCreate, 503, "rollback_request_002")
		if bytes.Contains(response, []byte("constraint")) {
			t.Fatal("raw SQL error exposed")
		}
		var n, after int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM identity_tenants WHERE name='Rollback tenant'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM kernel_outbox`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		var receipts int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM identity_commands WHERE request_key='rollback_request_002'`).Scan(&receipts); err != nil {
			t.Fatal(err)
		}
		if n != 0 || before != after || receipts != 0 {
			t.Fatal("partial lifecycle commit")
		}
	})
	t.Run("directory-outage-fails-closed", func(t *testing.T) {
		owner, err := pgx.Connect(ctx, database.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close(ctx)
		// Either original or second admin remains; membership-independent SQL failure must be 503.
		if _, err := owner.Exec(ctx, `ALTER TABLE identity_members RENAME TO unavailable_members`); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = owner.Exec(ctx, `ALTER TABLE unavailable_members RENAME TO identity_members`) }()
		response := expect(adminToken, "GET", "/v1/identity/session", "", 503)
		if bytes.Contains(response, []byte("identity_members")) {
			t.Fatal("raw directory failure exposed")
		}
	})
	t.Run("persisted-events-match-original-schemas", func(t *testing.T) {
		rows, err := app.Pool.Query(ctx, `SELECT envelope FROM kernel_outbox WHERE envelope->>'type' LIKE 'identity.%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var envelope outbox.Envelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			schema, err := events.Schema(envelope.Type)
			if err != nil {
				t.Fatal(err)
			}
			if err := events.Validate(schema, envelope.Data); err != nil {
				t.Fatal(err)
			}
			if err := envelope.CheckTopic("platform.identity"); err != nil {
				t.Fatal(err)
			}
			count++
		}
		if rows.Err() != nil || count < 6 {
			t.Fatalf("event count %d: %v", count, rows.Err())
		}
	})
	t.Run("real-kafka-lifecycle-and-audit-relay", func(t *testing.T) {
		brokers := testenv.Brokers(t)
		database.GrantRelay(t)
		producer, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
		if err != nil {
			t.Fatal(err)
		}
		defer producer.Close()
		rctx, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		if _, err := kadm.NewClient(producer).CreateTopics(rctx, 3, 1, nil, "platform.identity", "platform.audit"); err != nil {
			t.Fatal(err)
		}
		consumer, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics("platform.identity", "platform.audit"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
		if err != nil {
			t.Fatal(err)
		}
		defer consumer.Close()
		rows, err := app.Pool.Query(ctx, `SELECT event_id FROM kernel_outbox`)
		if err != nil {
			t.Fatal(err)
		}
		expected := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			expected[id] = true
		}
		rows.Close()
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		relayErr := make(chan error, 1)
		go func() {
			relayErr <- (&relay.Relay{Sources: []relay.Source{{Name: "identity", DSN: database.RelayDSN}}, Producer: producer, Log: env.Log, Poll: 10 * time.Millisecond}).Run(rctx)
		}()
		defer func() { stop(); <-relayErr }()
		for len(expected) > 0 {
			fetches := consumer.PollFetches(rctx)
			if err := fetches.Err(); err != nil {
				t.Fatalf("lifecycle delivery remaining=%d: %v", len(expected), err)
			}
			for _, record := range fetches.Records() {
				var event outbox.Envelope
				if err := json.Unmarshal(record.Value, &event); err != nil {
					t.Fatal(err)
				}
				if !expected[event.ID] {
					continue
				}
				if err := event.CheckTopic(record.Topic); err != nil {
					t.Fatal(err)
				}
				schema, err := events.Schema(event.Type)
				if err != nil {
					t.Fatal(err)
				}
				if err := events.Validate(schema, event.Data); err != nil {
					t.Fatal(err)
				}
				delete(expected, event.ID)
			}
		}
	})

}
