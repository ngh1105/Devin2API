package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngh1105/Devin2API/internal/config"
)

func passwordServer(t *testing.T) *Server {
	t.Helper()
	loginFailDelay = 0
	return New(config.Config{Listen: ":0", AdminPassword: "s3cret"}, mustAccounts(t), mustTokens(t))
}

func adminDo(srv *Server, method, path, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = adminRequest(method, path, strings.NewReader(body))
	} else {
		req = adminRequest(method, path, nil)
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func loginCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	rec := adminDo(srv, http.MethodPost, "/admin/api/login", `{"password":"s3cret"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Errorf("cookie flags: %+v", c)
			}
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestNoPasswordNoLogin(t *testing.T) {
	srv := New(config.Config{Listen: ":0"}, mustAccounts(t), mustTokens(t))
	rec := adminDo(srv, http.MethodGet, "/admin/api/session", "", nil)
	if !strings.Contains(rec.Body.String(), `"auth_required":false`) {
		t.Errorf("session = %s", rec.Body)
	}
	if rec := adminDo(srv, http.MethodGet, "/admin/api/tokens", "", nil); rec.Code != http.StatusOK {
		t.Errorf("tokens without password: %d", rec.Code)
	}
}

func TestAdminPasswordFlow(t *testing.T) {
	srv := passwordServer(t)

	if rec := adminDo(srv, http.MethodGet, "/admin/api/tokens", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", rec.Code)
	}
	var sess map[string]bool
	json.Unmarshal(adminDo(srv, http.MethodGet, "/admin/api/session", "", nil).Body.Bytes(), &sess)
	if !sess["auth_required"] || sess["authenticated"] {
		t.Errorf("session before login = %v", sess)
	}
	if rec := adminDo(srv, http.MethodPost, "/admin/api/login", `{"password":"wrong"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", rec.Code)
	}

	c := loginCookie(t, srv)
	withCookie := func(r *http.Request) { r.AddCookie(c) }
	if rec := adminDo(srv, http.MethodGet, "/admin/api/tokens", "", withCookie); rec.Code != http.StatusOK {
		t.Errorf("with session: %d", rec.Code)
	}
	// With a password, the panel may be used from a LAN address.
	lan := func(r *http.Request) { r.AddCookie(c); r.Host = "192.168.1.20:8080" }
	if rec := adminDo(srv, http.MethodGet, "/admin/api/tokens", "", lan); rec.Code != http.StatusOK {
		t.Errorf("LAN host with session: %d", rec.Code)
	}
	// The CSRF guards still apply to logged-in requests.
	cross := func(r *http.Request) { r.AddCookie(c); r.Header.Set("Origin", "https://evil.example") }
	if rec := adminDo(srv, http.MethodGet, "/admin/api/tokens", "", cross); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin with session: %d", rec.Code)
	}
	noMarker := func(r *http.Request) { r.AddCookie(c); r.Header.Del("X-Devin2API") }
	if rec := adminDo(srv, http.MethodPost, "/admin/api/tokens", "", noMarker); rec.Code != http.StatusForbidden {
		t.Errorf("write without marker: %d", rec.Code)
	}

	adminDo(srv, http.MethodPost, "/admin/api/logout", "", withCookie)
	if rec := adminDo(srv, http.MethodGet, "/admin/api/tokens", "", withCookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", rec.Code)
	}
}

func TestAdminLoginRateLimit(t *testing.T) {
	srv := passwordServer(t)
	for i := 0; i < loginFailsPerMinute; i++ {
		adminDo(srv, http.MethodPost, "/admin/api/login", `{"password":"nope"}`, nil)
	}
	// Even the right password is refused until the window passes.
	if rec := adminDo(srv, http.MethodPost, "/admin/api/login", `{"password":"s3cret"}`, nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("after %d failures: %d", loginFailsPerMinute, rec.Code)
	}
}

// TestPasswordKeepsKeylessV1Local: the password opens the panel to the LAN,
// but keyless /v1 stays loopback-only (create API keys for LAN clients).
func TestPasswordKeepsKeylessV1Local(t *testing.T) {
	srv := passwordServer(t)
	req := v1Request(http.MethodGet, "/v1/models", nil)
	req.Host = "192.168.1.20:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("keyless /v1 from LAN: %d", rec.Code)
	}
}

// TestForgedHostFromNetworkRejected: without a password, a client on the
// network cannot pass the loopback guard by sending "Host: localhost".
func TestForgedHostFromNetworkRejected(t *testing.T) {
	srv := New(config.Config{Listen: ":0"}, mustAccounts(t), mustTokens(t))
	remote := func(r *http.Request) { r.RemoteAddr = "192.168.1.50:41000"; r.Host = "localhost:8080" }
	if rec := adminDo(srv, http.MethodPost, "/admin/api/tokens", "", remote); rec.Code != http.StatusForbidden {
		t.Errorf("admin from network with forged Host: %d, want 403", rec.Code)
	}
	req := v1Request(http.MethodGet, "/v1/models", nil)
	remote(req)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("keyless /v1 from network with forged Host: %d, want 401", rec.Code)
	}
	// A local client still works.
	if rec := adminDo(srv, http.MethodGet, "/admin/api/tokens", "", nil); rec.Code != http.StatusOK {
		t.Errorf("local client: %d", rec.Code)
	}
}
