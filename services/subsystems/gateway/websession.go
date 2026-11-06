package gateway

// websession contains the development and browser-session boundary for the
// workspace. The production OIDC authorization-code exchange is intentionally
// kept behind the same boundary; until the IdP client is configured, only the
// loopback development login is enabled. API requests never expose the stored
// bearer token to browser JavaScript: the middleware injects it server-side.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	devSessionCookie  = "mq_sid"
	hostSessionCookie = "__Host-mq_sid"
	csrfCookie        = "mq_csrf"
)

type WebSessionConfig struct {
	Issuer      string
	Audience    string
	Development bool
	DevTokenURL string
	DevSubject  string
	DevTenant   string
	DevCell     string
	Now         func() time.Time
}

type webSession struct {
	token     string
	subject   string
	tenant    string
	cell      string
	csrf      string
	expiresAt time.Time
}

type webSessionStore struct {
	mu       sync.Mutex
	sessions map[string]webSession
}

// WebSession wraps the bearer-only public handler with HttpOnly cookie
// sessions and /auth endpoints. It is deliberately in-memory for the first
// development slice; production OIDC persistence is a separate W-04 gate.
func WebSession(next http.Handler, cfg WebSessionConfig) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	store := &webSessionStore{sessions: map[string]webSession{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			handleWebAuth(w, r, store, cfg)
			return
		}
		if sid := sessionCookieName(cfg.Development); sid != "" {
			if cookie, err := r.Cookie(sid); err == nil && cookie.Value != "" {
				s, ok := store.get(cookie.Value, cfg.Now())
				if !ok {
					writeWebProblem(w, http.StatusUnauthorized, "session_expired", "session_expired")
					return
				}
				r = r.Clone(r.Context())
				r.Header.Set("Authorization", "Bearer "+s.token)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func handleWebAuth(w http.ResponseWriter, r *http.Request, store *webSessionStore, cfg WebSessionConfig) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/auth/login":
		if !cfg.Development {
			writeWebProblem(w, http.StatusNotImplemented, "web_login_not_configured", "web_login_not_configured")
			return
		}
		returnTo, err := safeReturnTo(r.URL.Query().Get("return_to"))
		if err != nil {
			writeWebProblem(w, http.StatusBadRequest, "invalid_return_to", "invalid_return_to")
			return
		}
		q := url.Values{"return_to": []string{returnTo}}
		if subject := r.URL.Query().Get("subject"); subject != "" {
			q.Set("subject", subject)
		}
		if tenant := r.URL.Query().Get("tenant"); tenant != "" {
			q.Set("tenant", tenant)
		}
		if cell := r.URL.Query().Get("cell"); cell != "" {
			q.Set("cell", cell)
		}
		if mfa := r.URL.Query().Get("mfa"); mfa != "" {
			q.Set("mfa", mfa)
		}
		http.Redirect(w, r, "/auth/dev-login?"+q.Encode(), http.StatusFound)
	case r.Method == http.MethodGet && r.URL.Path == "/auth/dev-login":
		if !cfg.Development {
			writeWebProblem(w, http.StatusNotFound, "not_found", "not_found")
			return
		}
		handleDevLogin(w, r, store, cfg)
	case r.Method == http.MethodGet && r.URL.Path == "/auth/session":
		handleSessionInfo(w, r, store, cfg)
	case r.Method == http.MethodPost && r.URL.Path == "/auth/logout":
		handleLogout(w, r, store, cfg)
	default:
		writeWebProblem(w, http.StatusNotFound, "not_found", "not_found")
	}
}

func handleDevLogin(w http.ResponseWriter, r *http.Request, store *webSessionStore, cfg WebSessionConfig) {
	returnTo, err := safeReturnTo(r.URL.Query().Get("return_to"))
	if err != nil {
		writeWebProblem(w, http.StatusBadRequest, "invalid_return_to", "invalid_return_to")
		return
	}
	subject, tenant, cell := r.URL.Query().Get("subject"), r.URL.Query().Get("tenant"), r.URL.Query().Get("cell")
	if subject == "" {
		subject = cfg.DevSubject
	}
	if tenant == "" {
		tenant = cfg.DevTenant
	}
	if cell == "" {
		cell = cfg.DevCell
	}
	if subject == "" || (tenant == "") != (cell == "") {
		writeWebProblem(w, http.StatusBadRequest, "dev_login_subject_required", "dev_login_subject_required")
		return
	}
	tokenURL := cfg.DevTokenURL
	if tokenURL == "" {
		tokenURL = strings.TrimRight(cfg.Issuer, "/") + "/token"
	}
	form := url.Values{"subject": []string{subject}, "tenant": []string{tenant}, "cell": []string{cell}}
	if r.URL.Query().Get("mfa") == "1" || r.URL.Query().Get("mfa") == "true" {
		form.Set("mfa", "1")
	}
	resp, err := http.PostForm(tokenURL, form)
	if err != nil {
		writeWebProblem(w, http.StatusServiceUnavailable, "identity_unavailable", "identity_unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeWebProblem(w, http.StatusUnauthorized, "dev_login_rejected", "dev_login_rejected")
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		writeWebProblem(w, http.StatusServiceUnavailable, "identity_unavailable", "identity_unavailable")
		return
	}
	if body.ExpiresIn <= 0 || body.ExpiresIn > 12*60*60 {
		body.ExpiresIn = 15 * 60
	}
	sid, err := randomToken(32)
	if err != nil {
		writeWebProblem(w, http.StatusInternalServerError, "internal", "internal")
		return
	}
	csrf, err := randomToken(32)
	if err != nil {
		writeWebProblem(w, http.StatusInternalServerError, "internal", "internal")
		return
	}
	store.put(sid, webSession{token: body.AccessToken, subject: subject, tenant: tenant, cell: cell, csrf: csrf, expiresAt: cfg.Now().Add(time.Duration(body.ExpiresIn) * time.Second)})
	setSessionCookies(w, cfg.Development, sid, csrf, cfg.Now().Add(time.Duration(body.ExpiresIn)*time.Second))
	http.Redirect(w, r, returnTo, http.StatusFound)
}

func handleSessionInfo(w http.ResponseWriter, r *http.Request, store *webSessionStore, cfg WebSessionConfig) {
	sid, err := r.Cookie(sessionCookieName(cfg.Development))
	if err != nil {
		writeWebProblem(w, http.StatusUnauthorized, "unauthenticated", "unauthenticated")
		return
	}
	s, ok := store.get(sid.Value, cfg.Now())
	if !ok {
		writeWebProblem(w, http.StatusUnauthorized, "session_expired", "session_expired")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"subject": s.subject, "tenant": s.tenant, "cell": s.cell, "csrf_token": s.csrf, "expires_at": s.expiresAt.UTC().Format(time.RFC3339)})
}

func handleLogout(w http.ResponseWriter, r *http.Request, store *webSessionStore, cfg WebSessionConfig) {
	if !sameOrigin(r) || r.Header.Get("X-Miaoqia-Request") != "1" {
		writeWebProblem(w, http.StatusForbidden, "csrf_rejected", "csrf_rejected")
		return
	}
	cookie, err := r.Cookie(sessionCookieName(cfg.Development))
	if err != nil {
		writeWebProblem(w, http.StatusUnauthorized, "unauthenticated", "unauthenticated")
		return
	}
	s, ok := store.get(cookie.Value, cfg.Now())
	if !ok || s.csrf == "" || r.Header.Get("X-Miaoqia-CSRF") != s.csrf {
		writeWebProblem(w, http.StatusForbidden, "csrf_rejected", "csrf_rejected")
		return
	}
	store.delete(cookie.Value)
	clearSessionCookies(w, cfg.Development)
	w.WriteHeader(http.StatusNoContent)
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Origin") == "" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host) && strings.EqualFold(u.Scheme, requestScheme(r))
}

func requestScheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func safeReturnTo(value string) (string, error) {
	if value == "" {
		return "/start", nil
	}
	if strings.HasPrefix(value, "//") || strings.Contains(value, "\\") {
		return "", errors.New("external return_to")
	}
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return "", errors.New("external return_to")
	}
	return u.RequestURI(), nil
}

func sessionCookieName(development bool) string {
	if development {
		return devSessionCookie
	}
	return hostSessionCookie
}

func setSessionCookies(w http.ResponseWriter, development bool, sid, csrf string, expires time.Time) {
	secure := !development
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName(development), Value: sid, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrf, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()), HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode})
}

func clearSessionCookies(w http.ResponseWriter, development bool) {
	secure := !development
	for _, name := range []string{sessionCookieName(development), csrfCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: name != csrfCookie, Secure: secure, SameSite: http.SameSiteLaxMode})
	}
}

func randomToken(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *webSessionStore) put(id string, value webSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[hashSessionID(id)] = value
}

func (s *webSessionStore) get(id string, now time.Time) (webSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[hashSessionID(id)]
	if !ok || !now.Before(value.expiresAt) {
		delete(s.sessions, hashSessionID(id))
		return webSession{}, false
	}
	return value, true
}

func (s *webSessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, hashSessionID(id))
}

func hashSessionID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func writeWebProblem(w http.ResponseWriter, status int, code, title string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://errors.miaoqia.com/" + code, "title": title, "status": status, "code": code})
}
