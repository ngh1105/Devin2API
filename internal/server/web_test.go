package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebRoutes verifies embed serving of /web/ pages and JS/CSS.
func TestWebRoutes(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServerWithAuth(t, tmp, nil)

	// 1) GET / → 302 to /web/accounts.html.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("GET / status: got %d, want %d", rec.Code, http.StatusFound)
	}
	loc := rec.Header().Get("Location")
	if loc != "/web/accounts.html" {
		t.Errorf("GET / Location: got %q, want /web/accounts.html", loc)
	}

	// 2) GET /web/accounts.html → 200, contains data-page="accounts".
	req = httptest.NewRequest(http.MethodGet, "/web/accounts.html", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/accounts.html: got %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-page="accounts"`) {
		t.Error("accounts.html missing data-page=\"accounts\"")
	}

	// 3) GET /web/tokens.html → 200, contains data-page="tokens".
	req = httptest.NewRequest(http.MethodGet, "/web/tokens.html", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/tokens.html: got %d, want %d", rec.Code, http.StatusOK)
	}
	body = rec.Body.String()
	if !strings.Contains(body, `data-page="tokens"`) {
		t.Error("tokens.html missing data-page=\"tokens\"")
	}

	// 4) GET /web/settings.html → 200, contains data-page="settings".
	req = httptest.NewRequest(http.MethodGet, "/web/settings.html", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/settings.html: got %d, want %d", rec.Code, http.StatusOK)
	}
	body = rec.Body.String()
	if !strings.Contains(body, `data-page="settings"`) {
		t.Error("settings.html missing data-page=\"settings\"")
	}

	// 5) GET /web/app.js → 200 content-type text/javascript.
	req = httptest.NewRequest(http.MethodGet, "/web/app.js", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/app.js: got %d, want %d", rec.Code, http.StatusOK)
	}
	ct := rec.Header().Get("Content-Type")
	if ct == "" || !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type: got %q, want text/javascript", ct)
	}

	// 6) GET /web/styles.css → 200 content-type text/css.
	req = httptest.NewRequest(http.MethodGet, "/web/styles.css", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/styles.css: got %d, want %d", rec.Code, http.StatusOK)
	}
	ct = rec.Header().Get("Content-Type")
	if ct == "" || !strings.Contains(ct, "css") {
		t.Errorf("styles.css Content-Type: got %q, want text/css", ct)
	}
}

// TestAdminAPIWorks verifies admin API is still operational alongside web panel.
func TestAdminAPIWorks(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServerWithAuth(t, tmp, nil)

	// Settings endpoint should return correct JSON.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("settings status: got %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"version"`) || !strings.Contains(body, `"dev"`) {
		t.Error("settings response missing version field")
	}
}
