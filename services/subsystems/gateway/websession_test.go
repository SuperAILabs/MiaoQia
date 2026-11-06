package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWebSessionDevLoginInjectsBearerWithoutExposingIt(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			t.Fatalf("unexpected token request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "signed-dev-token", "expires_in": 300})
	}))
	defer tokenServer.Close()

	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})
	h := WebSession(next, WebSessionConfig{Development: true, DevTokenURL: tokenServer.URL + "/token", Now: time.Now})

	login := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/login?return_to=/briefings", nil)
	h.ServeHTTP(login, r)
	if login.Code != http.StatusFound || !strings.Contains(login.Header().Get("Location"), "/auth/dev-login") {
		t.Fatalf("login redirect: status=%d location=%q", login.Code, login.Header().Get("Location"))
	}

	dev := httptest.NewRecorder()
	devReq := httptest.NewRequest(http.MethodGet, "/auth/dev-login?return_to=/briefings&subject=dm&tenant=tnt_demo&cell=cell-01", nil)
	h.ServeHTTP(dev, devReq)
	if dev.Code != http.StatusFound || dev.Header().Get("Location") != "/briefings" {
		t.Fatalf("dev login: status=%d location=%q", dev.Code, dev.Header().Get("Location"))
	}
	var sid, csrf string
	for _, cookie := range dev.Result().Cookies() {
		switch cookie.Name {
		case "mq_sid":
			sid = cookie.Value
		case "mq_csrf":
			csrf = cookie.Value
		}
	}
	if sid == "" || csrf == "" {
		t.Fatalf("missing session cookies: sid=%q csrf=%q", sid, csrf)
	}

	api := httptest.NewRecorder()
	apiReq := httptest.NewRequest(http.MethodGet, "/v1/objects/pp_demo", nil)
	apiReq.AddCookie(&http.Cookie{Name: "mq_sid", Value: sid})
	h.ServeHTTP(api, apiReq)
	if api.Code != http.StatusNoContent || seen != "Bearer signed-dev-token" {
		t.Fatalf("session request: status=%d authorization=%q", api.Code, seen)
	}
}

func TestWebSessionRejectsOpenRedirectAndRequiresCSRFForLogout(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 300})
	}))
	defer tokenServer.Close()
	h := WebSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), WebSessionConfig{Development: true, DevTokenURL: tokenServer.URL + "/token", Now: time.Now})

	open := httptest.NewRecorder()
	h.ServeHTTP(open, httptest.NewRequest(http.MethodGet, "/auth/login?return_to="+url.QueryEscape("https://evil.example"), nil))
	if open.Code != http.StatusBadRequest {
		t.Fatalf("open redirect status=%d", open.Code)
	}

	dev := httptest.NewRecorder()
	h.ServeHTTP(dev, httptest.NewRequest(http.MethodGet, "/auth/dev-login?return_to=/start&subject=dm", nil))
	var sid, csrf string
	for _, cookie := range dev.Result().Cookies() {
		switch cookie.Name {
		case "mq_sid":
			sid = cookie.Value
		case "mq_csrf":
			csrf = cookie.Value
		}
	}
	logout := httptest.NewRecorder()
	logoutReq := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	logoutReq.AddCookie(&http.Cookie{Name: "mq_sid", Value: sid})
	logoutReq.Header.Set("Origin", "http://example.com")
	logoutReq.Header.Set("X-Miaoqia-Request", "1")
	h.ServeHTTP(logout, logoutReq)
	if logout.Code != http.StatusForbidden {
		t.Fatalf("csrf status=%d", logout.Code)
	}

	ok := httptest.NewRecorder()
	okReq := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	okReq.AddCookie(&http.Cookie{Name: "mq_sid", Value: sid})
	okReq.Header.Set("Origin", "http://example.com")
	okReq.Header.Set("X-Miaoqia-Request", "1")
	okReq.Header.Set("X-Miaoqia-CSRF", csrf)
	h.ServeHTTP(ok, okReq)
	if ok.Code != http.StatusNoContent {
		t.Fatalf("valid csrf status=%d", ok.Code)
	}
}
