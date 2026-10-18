package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebRoutesSpa verifies single-page admin panel routing:
//
//	GET /          → 302 Location /web/
//	GET /web/      → 200 contains id="app"
//	GET /web/index.html → 200
//	GET /web/app.js   → 200 + text/javascript
//	GET /web/styles.css → 200 + text/css
//	GET /web/accounts.html → 404 (old file deleted)
func TestWebRoutesSpa(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)

	// 1) GET / → 302 to /web/.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("GET / status: got %d, want %d", rec.Code, http.StatusFound)
	}
	loc := rec.Header().Get("Location")
	if loc != "/web/" {
		t.Errorf("GET / Location: got %q, want /web/", loc)
	}

	// 2) GET /web/ → 200, body contains id="app".
	req = httptest.NewRequest(http.MethodGet, "/web/", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/ status: got %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="app"`) {
		t.Error("/web/ body missing id=\"app\"")
	}

	// 3) GET /web/index.html → 200.
	req = httptest.NewRequest(http.MethodGet, "/web/index.html", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/index.html: got %d, want %d", rec.Code, http.StatusOK)
	}

	// 4) GET /web/app.js → 200, content-type text/javascript.
	req = httptest.NewRequest(http.MethodGet, "/web/app.js", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/app.js status: got %d, want %d", rec.Code, http.StatusOK)
	}
	ct := rec.Header().Get("Content-Type")
	if ct == "" || !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type: got %q, want text/javascript", ct)
	}

	// 5) GET /web/styles.css → 200, content-type text/css.
	req = httptest.NewRequest(http.MethodGet, "/web/styles.css", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /web/styles.css status: got %d, want %d", rec.Code, http.StatusOK)
	}
	ct = rec.Header().Get("Content-Type")
	if ct == "" || !strings.Contains(ct, "css") {
		t.Errorf("styles.css Content-Type: got %q, want text/css", ct)
	}

	// 6) GET /web/accounts.html → 404 (old file removed).
	req = httptest.NewRequest(http.MethodGet, "/web/accounts.html", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /web/accounts.html status: got %d, want %d", rec.Code, http.StatusNotFound)
	}

	// 7) GET /web/tokens.html → 404 (old file removed).
	req = httptest.NewRequest(http.MethodGet, "/web/tokens.html", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /web/tokens.html status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestWebPathTraversalGuard exercises the unexported webFileServer directly
// to verify it rejects ".." in the embedded FS path with 404.
func TestWebPathTraversalGuard(t *testing.T) {
	for _, p := range []string{"/web/x/../../go.mod", "/web/../../web/index.html", "/web/foo%2F..%2Fbar"} {
		req, _ := http.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		webFileServer(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s webFileServer: got %d, want %d (path traversal must be rejected)", p, rec.Code, http.StatusNotFound)
		}
	}
}

// TestAdminAPIWorks verifies admin API is still operational alongside web panel.
func TestAdminAPIWorks(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)

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
