package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/store"
)

func makeTestAccount(t *testing.T, s *Server) *store.Account {
	t.Helper()
	acct := &store.Account{
		Schema:    2,
		ID:        "test-acc-" + time.Now().Format("20060102150405"),
		Email:     "admin@test.com",
		UserName:  "Admin User",
		UserID:    "user-test",
		OrgID:     "org-test",
		Plan:      "Pro",
		AuthKind:  "oauth",
		Enabled:   true,
		CreatedAt: time.Now(),
		Tokens:    store.Tokens{SessionToken: "devin-session-token$eyfake_token"},
	}
	if err := s.accounts.Save(acct); err != nil {
		t.Fatalf("save test account: %v", err)
	}
	return acct
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestAdminImportAccount verifies POST /admin/api/accounts with a bare token blob.
func TestAdminImportAccount(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)

	body := `{"access_token":"tok_test_abc","refresh_token":"rt_test_def"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusCreated)
	}

	var acc store.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if acc.Email != "" && acc.ID == "" {
		t.Errorf("account has no id")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want %d", rec2.Code, http.StatusOK)
	}
	var list []map[string]any
	json.Unmarshal(rec2.Body.Bytes(), &list)
	if len(list) < 1 {
		t.Fatalf("expected at least 1 account in list, got %d", len(list))
	}
}

// TestAdminImportMalformed verifies POST /admin/api/accounts with invalid JSON → 400.
func TestAdminImportMalformed(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] == "" {
		t.Error("expected error field in response")
	}
}

// TestAdminDeleteAccount verifies DELETE /admin/api/accounts/{id}.
func TestAdminDeleteAccount(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)
	acc := makeTestAccount(t, srv)

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/accounts/"+acc.ID, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want %d", rec.Code, http.StatusNoContent)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	var list []*accountExpired
	json.Unmarshal(rec2.Body.Bytes(), &list)
	for _, a := range list {
		if a.ID == acc.ID {
			t.Fatal("deleted account still in list")
		}
	}

	req3 := httptest.NewRequest(http.MethodDelete, "/admin/api/accounts/nonexistent", nil)
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Errorf("delete missing: got %d, want %d", rec3.Code, http.StatusNotFound)
	}
}

// TestAdminSettings verifies GET /admin/api/settings returns correct fields.
func TestAdminSettings(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	resp := make(map[string]string)
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["version"] != "dev" {
		t.Errorf("version: got %q, want dev", resp["version"])
	}
	if resp["listen"] != ":0" {
		t.Errorf("listen: got %q, want :0", resp["listen"])
	}
}

// TestAdminSettingsPOSTVerbs verifies PUT/POST → 405.
func TestAdminSettingsPOSTVerbs(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)

	for _, method := range []string{http.MethodPut, http.MethodPost} {
		req := httptest.NewRequest(method, "/admin/api/settings", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /admin/api/settings: got %d, want 405", method, rec.Code)
		}
		var errResp map[string]string
		json.Unmarshal(rec.Body.Bytes(), &errResp)
		if errResp["error"] != "method not allowed" {
			t.Errorf("error msg for %s: got %q", method, errResp["error"])
		}
	}
}

// TestAdminTokenLifecycle verifies create → list has 1 → delete → gone.
func TestAdminTokenLifecycle(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/tokens", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, want %d", rec.Code, http.StatusCreated)
	}
	var tok store.Token
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("unmarshal created token: %v", err)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/admin/api/tokens", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	var toks []store.Token
	json.Unmarshal(rec2.Body.Bytes(), &toks)
	if len(toks) != 1 {
		t.Fatalf("list count: got %d, want 1", len(toks))
	}

	req3 := httptest.NewRequest(http.MethodDelete, "/admin/api/tokens/"+tok.ID, nil)
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want %d", rec3.Code, http.StatusNoContent)
	}

	req4 := httptest.NewRequest(http.MethodGet, "/admin/api/tokens", nil)
	rec4 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec4, req4)
	json.Unmarshal(rec4.Body.Bytes(), &toks)
	if len(toks) != 0 {
		t.Fatalf("after delete: got %d tokens, want 0", len(toks))
	}
}

// TestAdminImportAccountGeneratesID verifies that importing an account via
// POST /admin/api/accounts generates a non-empty id and persists it on disk.
func TestAdminImportAccountGeneratesID(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)

	body := `{"session_token":"eyJhbg.test","email":"u@t.com"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusCreated)
	}
	var acc store.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if acc.ID == "" {
		t.Error("expected account id to be non-empty")
	}

	expectedFile := filepath.Join(tmp, acc.ID+".json")
	if _, err := os.Stat(expectedFile); os.IsNotExist(err) {
		t.Errorf("expected account file %s to exist on disk", expectedFile)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want %d", rec2.Code, http.StatusOK)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 account in list, got %d", len(list))
	}
}
