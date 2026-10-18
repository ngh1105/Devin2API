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

	"github.com/ngh1105/Devin2API/internal/auth"
	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeTestServerWithAuth(t *testing.T, tmpDir string, authClient *auth.Client) *Server {
	t.Helper()
	acc, err := store.NewAccountStore(tmpDir)
	if err != nil {
		t.Fatalf("new account store: %v", err)
	}
	tok, err := store.NewTokenStore(tmpDir)
	if err != nil {
		t.Fatalf("new token store: %v", err)
	}
	return New(config.Config{Listen: ":0", DataDir: tmpDir}, acc, tok, authClient)
}

func makeTestAccount(t *testing.T, s *Server) *store.Account {
	t.Helper()
	acct := &store.Account{
		Schema:    1,
		ID:        "test-acc-" + time.Now().Format("20060102150405"),
		Email:     "admin@test.com",
		Sub:       "auth0|testsub",
		CreatedAt: time.Now(),
		Tokens: store.Tokens{
			AccessToken:  "eyfake_token",
			RefreshToken: "rt_fake_12345",
			IDToken:      "id_fake",
			ExpiresAt:    time.Now().Add(1 * time.Hour).Unix(),
			Scope:        "openid",
		},
		OAuth: store.OAuth{
			ClientID: auth.ClientID,
			Domain:   auth.AuthDomain,
		},
		Profile: store.Profile{Name: "Admin User"},
	}
	if err := s.accounts.Save(acct); err != nil {
		t.Fatalf("save test account: %v", err)
	}
	return acct
}

// fakeAuthHandlerConfig holds parameters for the mock Auth0 server.
type fakeAuthHandlerConfig struct {
	redirectURI   string // e.g. "https://app.devin.ai/post-auth"
	skipAuthorize bool   // if true, /authorize returns error immediately (refresh-only mode)
}

// fakeAuthHandler returns an httptest.Server mimicking Auth0 endpoints.
func fakeAuthHandler(t *testing.T, cfg fakeAuthHandlerConfig) *httptest.Server {
	t.Helper()
	if cfg.redirectURI == "" {
		cfg.redirectURI = "https://app.devin.ai/post-auth"
	}
	callback := callbackHandler{redirectURI: cfg.redirectURI}
	h := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			grantType := r.FormValue("grant_type")
			if grantType == "" {
				q := r.URL.Query()
				grantType = q.Get("grant_type")
			}
			w.Header().Set("Content-Type", "application/json")
			if grantType == "refresh_token" {
				json.NewEncoder(w).Encode(struct {
					AccessToken  string `json:"access_token"`
					RefreshToken string `json:"refresh_token"`
					IDToken      string `json:"id_token"`
					TokenType    string `json:"token_type"`
					ExpiresIn    int64  `json:"expires_in"`
					Scope        string `json:"scope"`
				}{
					AccessToken:  "eynew_access",
					RefreshToken: "rt_new_67890",
					TokenType:    "Bearer",
					ExpiresIn:    3600,
					Scope:        "openid profile email",
				})
				return
			}
			json.NewEncoder(w).Encode(struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				IDToken      string `json:"id_token"`
				TokenType    string `json:"token_type"`
				ExpiresIn    int64  `json:"expires_in"`
				Scope        string `json:"scope"`
			}{
				AccessToken:  "eylogin_access",
				RefreshToken: "rt_login_refresh",
				TokenType:    "Bearer",
				ExpiresIn:    3600,
				Scope:        "openid profile email offline_access",
			})
		case "/userinfo":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(struct {
				Sub     string `json:"sub"`
				Email   string `json:"email"`
				Name    string `json:"name"`
				Picture string `json:"picture"`
			}{
				Sub:     "auth0|userinfo-sub",
				Email:   "logged@test.com",
				Name:    "Logged In User",
				Picture: "https://example.com/avatar.png",
			})
		case "/authorize":
			if cfg.skipAuthorize {
				w.Header().Set("Location", "/u/login/identifier?error=access_denied&error_description=Denied")
				w.WriteHeader(http.StatusFound)
				return
			}
			state := r.URL.Query().Get("state")
			challenge := r.URL.Query().Get("code_challenge")
			loc := "/u/login/identifier?state=" + state + "&code_challenge=" + challenge
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusFound)
		case "/u/login/identifier":
			if r.Method == http.MethodGet {
				state := r.URL.Query().Get("state")
				html := `<html><body><form>` +
					`<input name="state" value="` + state + `" />` +
					`<input name="username" /></form></body></html>`
				w.Header().Set("Content-Type", "text/html")
				w.Write([]byte(html))
				return
			}
			r.ParseForm()
			pwState := r.FormValue("state")
			loc := "/u/login/password?state=" + pwState
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusFound)
		case "/u/login/password":
			if r.Method == http.MethodGet {
				state := r.URL.Query().Get("state")
				html := `<html><body><form>` +
					`<input name="state" value="` + state + `" />` +
					`<input name="username" /><input name="password" /></form></body></html>`
				w.Header().Set("Content-Type", "text/html")
				w.Write([]byte(html))
				return
			}
			r.ParseForm()
			callback.doPassword(r, w)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

// callbackHandler manages the password submission redirect with proper redirect URI.
type callbackHandler struct {
	redirectURI string
}

func (cb *callbackHandler) doPassword(r *http.Request, w http.ResponseWriter) {
	r.ParseForm()
	pwState := r.FormValue("state")
	code := "abc123"
	query := "?code=" + code + "&state=" + pwState
	w.Header().Set("Location", cb.redirectURI+query)
	w.WriteHeader(http.StatusFound)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestAdminImportAccount verifies POST /admin/api/accounts with a bare token blob.
func TestAdminImportAccount(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServerWithAuth(t, tmp, nil)

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
	srv := makeTestServerWithAuth(t, tmp, nil)

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
	srv := makeTestServerWithAuth(t, tmp, nil)
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

// TestAdminRefreshAccount verifies POST /admin/api/accounts/{id}/refresh via fake auth.
func TestAdminRefreshAccount(t *testing.T) {
	fakeCfg := fakeAuthHandlerConfig{skipAuthorize: true}
	fakeSrv := fakeAuthHandler(t, fakeCfg)
	defer fakeSrv.Close()

	tmp := t.TempDir()
	authC := auth.NewClient(auth.WithBaseURL(fakeSrv.URL), auth.WithHTTPClient(fakeSrv.Client()))
	srv := makeTestServerWithAuth(t, tmp, authC)
	acc := makeTestAccount(t, srv)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/"+acc.ID+"/refresh", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var refreshed store.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &refreshed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if refreshed.Tokens.AccessToken != "eynew_access" {
		t.Errorf("refresh token wrong: got %q", refreshed.Tokens.AccessToken)
	}
}

// TestAdminRefreshNoToken verifies 400 when account has no refresh token.
func TestAdminRefreshNoToken(t *testing.T) {
	fakeSrv := fakeAuthHandler(t, fakeAuthHandlerConfig{})
	defer fakeSrv.Close()

	tmp := t.TempDir()
	authC := auth.NewClient(auth.WithBaseURL(fakeSrv.URL), auth.WithHTTPClient(fakeSrv.Client()))
	srv := makeTestServerWithAuth(t, tmp, authC)

	acct := &store.Account{
		Schema: 1,
		ID:     "no-refresh-id",
		Email:  "norefresh@test.com",
		Sub:    "auth0|noreflow",
		Tokens: store.Tokens{},
		OAuth:  store.OAuth{ClientID: auth.ClientID},
	}
	srv.accounts.Save(acct)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/no-refresh-id/refresh", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
	bodyBytes := rec.Body.Bytes()
	if !strings.Contains(string(bodyBytes), "no refresh token") {
		t.Errorf("missing error message in response: %s", string(bodyBytes))
	}
}

// TestAdminTokenLifecycle verifies create → list has 1 → delete → gone.
func TestAdminTokenLifecycle(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServerWithAuth(t, tmp, nil)

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

// TestAdminSettings verifies GET /admin/api/settings returns correct fields.
func TestAdminSettings(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServerWithAuth(t, tmp, nil)

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
	srv := makeTestServerWithAuth(t, tmp, nil)

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

// TestAdminLoginFlow verifies POST /admin/api/accounts with email+password runs auth.Login + UserInfo.
func TestAdminLoginFlow(t *testing.T) {
	fakeCfg := fakeAuthHandlerConfig{
		redirectURI:   "https://app.devin.ai/post-auth",
		skipAuthorize: false,
	}
	fakeSrv := fakeAuthHandler(t, fakeCfg)
	defer fakeSrv.Close()

	tmp := t.TempDir()
	authC := auth.NewClient(auth.WithBaseURL(fakeSrv.URL), auth.WithHTTPClient(fakeSrv.Client()))
	srv := makeTestServerWithAuth(t, tmp, authC)

	body := `{"email":"user@test.com","password":"secret"}`
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
	if acc.Sub != "auth0|userinfo-sub" {
		t.Errorf("sub: got %q, want auth0|userinfo-sub", acc.Sub)
	}
	if acc.Tokens.AccessToken != "eylogin_access" {
		t.Errorf("access token: got %q", acc.Tokens.AccessToken)
	}
}

// TestAdminLoginError verifies login failure returns 502.
func TestAdminLoginError(t *testing.T) {
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer badSrv.Close()

	tmp := t.TempDir()
	authC := auth.NewClient(auth.WithBaseURL(badSrv.URL), auth.WithHTTPClient(badSrv.Client()))
	srv := makeTestServerWithAuth(t, tmp, authC)

	body := `{"email":"x@x.com","password":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

// TestAdminRefreshMissingAccount verifies 404 for nonexistent account.
func TestAdminRefreshMissingAccount(t *testing.T) {
	fakeSrv := fakeAuthHandler(t, fakeAuthHandlerConfig{})
	defer fakeSrv.Close()

	tmp := t.TempDir()
	authC := auth.NewClient(auth.WithBaseURL(fakeSrv.URL), auth.WithHTTPClient(fakeSrv.Client()))
	srv := makeTestServerWithAuth(t, tmp, authC)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/gone-id/refresh", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestAdminLoginFlowGeneratesID verifies that POST /admin/api/accounts with
// email+password uses AccountFromLogin (which sets a non-empty id) and that
// an <id>.json file is persisted on disk.
func TestAdminLoginFlowGeneratesID(t *testing.T) {
	fakeCfg := fakeAuthHandlerConfig{
		redirectURI:   "https://app.devin.ai/post-auth",
		skipAuthorize: false,
	}
	fakeSrv := fakeAuthHandler(t, fakeCfg)
	defer fakeSrv.Close()

	tmp := t.TempDir()
	authC := auth.NewClient(auth.WithBaseURL(fakeSrv.URL), auth.WithHTTPClient(fakeSrv.Client()))
	srv := makeTestServerWithAuth(t, tmp, authC)

	body := `{"email":"user@test.com","password":"secret"}`
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
		t.Error("expected account id to be non-empty (AccountFromLogin should generate it)")
	}

	// Verify the .json file exists on disk in the store directory.
	expectedFile := filepath.Join(tmp, acc.ID+".json")
	if _, err := os.Stat(expectedFile); os.IsNotExist(err) {
		t.Errorf("expected account file %s to exist on disk", expectedFile)
	}

	// Also verify via GET listing that the account appears.
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
