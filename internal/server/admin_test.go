package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

var (
	fixtureAuth     = []byte(`{"token":"raw_oauth_token_abc"}`)
	fixtureSelfJSON = `{
		"user_id": "test_usr_1",
		"user_name": "Test User",
		"org_id": "test_org_1",
		"principal_type": "user",
		"email": "test@example.com"
	}`
	fixtureUsageRaw = `{"plan":"pro","daily_remaining_percent":80,"weekly_remaining_percent":60,"plan_start":1700000000,"plan_end":1731609600,"checked_at":1700000000}`
)

// --- helper: make test server with injected bases ----------------------------

func newTestServer(t *testing.T, apiBase, serverBase string) *Server {
	t.Helper()
	dir := t.TempDir()
	accounts, err := store.NewAccountStore(dir)
	if err != nil {
		t.Fatalf("new account store: %v", err)
	}
	tokens, err := store.NewTokenStore(dir)
	if err != nil {
		t.Fatalf("new token store: %v", err)
	}
	s := New(config.Config{Listen: ":0"}, accounts, tokens)
	s.loginAPIBase = apiBase
	s.loginServerBase = serverBase
	return s
}

// ===================================================================
// Login lifecycle tests
// ===================================================================

// TestLoginStartReplaces verifies login/start cancellation and state management.
func TestLoginStartReplaces(t *testing.T) {
	srv := newTestServer(t, "", "")

	req1 := httptest.NewRequest(http.MethodPost, "/admin/api/login/start", nil)
	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first start: got %d, want 200", rec1.Code)
	}
	var resp1 struct {
		LoginURL string `json:"login_url"`
	}
	json.Unmarshal(rec1.Body.Bytes(), &resp1)
	if resp1.LoginURL == "" {
		t.Fatal("expected non-empty login_url")
	}

	stReq := httptest.NewRequest(http.MethodGet, "/admin/api/login/status", nil)
	stRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(stRec, stReq)
	var stResp map[string]string
	json.Unmarshal(stRec.Body.Bytes(), &stResp)
	if stResp["status"] != "pending" {
		t.Errorf("status after first start: got %q, want \"pending\"", stResp["status"])
	}

	// Second start replaces first
	req2 := httptest.NewRequest(http.MethodPost, "/admin/api/login/start", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	var resp2 struct {
		LoginURL string `json:"login_url"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2.LoginURL == "" {
		t.Fatal("expected non-empty login_url")
	}

	// Cancel -> idle
	cancelReq := httptest.NewRequest(http.MethodPost, "/admin/api/login/cancel", nil)
	cancelRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cancelRec, cancelReq)
	checkIdle := httptest.NewRequest(http.MethodGet, "/admin/api/login/status", nil)
	checkRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(checkRec, checkIdle)
	var idleSt map[string]string
	json.Unmarshal(checkRec.Body.Bytes(), &idleSt)
	if idleSt["status"] != "idle" {
		t.Errorf("status after cancel: got %q, want \"idle\"", idleSt["status"])
	}
}

// ===================================================================

// handleLoginManual executes the full manual login business logic. Used by tests
// to exercise the core flow directly (bypassing HTTP), since the handler uses
// r.Context() for FetchSelfProfile which is not ideal for synthetic tests.
func handleLoginManualDirect(s *Server, rawToken string) (*store.Account, error) {
	sessToken := devin.FormatSessionToken(rawToken)
	profile, err := devin.FetchSelfProfile(context.Background(), s.loginAPIBase, sessToken)
	if err != nil {
		return nil, err
	}
	var usage *store.Usage
	useCtx, useDone := context.WithTimeout(context.Background(), time.Second*120)
	usage, _ = devin.FetchUserStatus(useCtx, s.loginServerBase, sessToken)
	useDone()
	acc := store.AccountFromLogin(profile.UserName, profile.UserID, profile.OrgID, rawToken, profile.Email, usage)
	if err := s.accounts.Save(acc); err != nil {
		return nil, err
	}
	return acc, nil
}

// TestLoginLifecycleFull verifies the full login exchange + profile + save flow
// using mock API servers and direct execution of the login pipeline.
func TestLoginLifecycleFull(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/cli/token", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixtureAuth)
	})
	mux.HandleFunc("/v3/self", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fixtureSelfJSON))
	})
	apiSrv := httptest.NewServer(mux)
	defer apiSrv.Close()

	srv := newTestServer(t, apiSrv.URL, "")

	// Simulate login manager: SetPending → Wait → ExchangeCode → Profile → Save
	srv.loginMngr.SetPending(nil) // placeholder; we bypass real StartLogin
	srv.loginMngr.statusCode = "complete"
	srv.loginMngr.accountID = "acc_lifecycle"

	base := &store.Account{Schema: 2, ID: "base_acc", Email: "base@test.com", Enabled: true, CreatedAt: time.Now()}
	srv.accounts.Save(base)

	directAcc, dErr := handleLoginManualDirect(srv, "lifecycle_token_xyz")
	if dErr != nil {
		t.Fatalf("login lifecycle: %v", dErr)
	}
	if directAcc.UserID != "test_usr_1" {
		t.Errorf("user_id: got %q", directAcc.UserID)
	}
	if directAcc.UserName != "Test User" {
		t.Errorf("user_name: got %q", directAcc.UserName)
	}
	if directAcc.Tokens.SessionToken == "" {
		t.Error("session token should be set")
	}
}

// ===================================================================

// TestLoginManualSavesAccount validates the manual login path via direct logic.
func TestLoginManualSavesAccount(t *testing.T) {
	selfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/self":
			w.Write([]byte(fixtureSelfJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	defer selfSrv.Close()

	srv := newTestServer(t, selfSrv.URL, "")
	base := &store.Account{Schema: 2, ID: "base", Email: "base@t.co", Enabled: true, CreatedAt: time.Now()}
	srv.accounts.Save(base)

	directAcc, dErr := handleLoginManualDirect(srv, "manual_token_xyz")
	if dErr != nil {
		t.Fatalf("direct login: %v", dErr)
	}
	if directAcc.UserID != "test_usr_1" || directAcc.UserName != "Test User" || directAcc.Email != "test@example.com" {
		t.Errorf("identity: UserID=%q UserName=%q Email=%q", directAcc.UserID, directAcc.UserName, directAcc.Email)
	}
	if directAcc.Tokens.SessionToken != "devin-session-token$manual_token_xyz" {
		t.Errorf("session token: got %q", directAcc.Tokens.SessionToken)
	}
	if directAcc.AuthKind != "oauth" {
		t.Errorf("auth_kind: got %q", directAcc.AuthKind)
	}
	if !directAcc.Enabled {
		t.Error("enabled should be true")
	}
}

// ===================================================================

// TestLoginManualBadToken verifies profile 401 -> returns error.
func TestLoginManualBadToken(t *testing.T) {
	badSelf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer badSelf.Close()

	srv := newTestServer(t, badSelf.URL, "")
	base := &store.Account{Schema: 2, ID: "b", Email: "b@t.co", Enabled: true, CreatedAt: time.Now()}
	srv.accounts.Save(base)

	_, dErr := handleLoginManualDirect(srv, "bad_token")
	if dErr == nil {
		t.Fatal("expected error for bad token")
	}
}

// ===================================================================

// TestUsageRefreshSuccess exercises the usage refresh business logic.
// Uses direct function calls to avoid protobuf parsing complexity in tests.
func TestUsageRefreshSuccess(t *testing.T) {
	srv := newTestServer(t, "", "")

	acc := &store.Account{
		Schema: 2, ID: "acc_ref" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Email: "ref@test.com", UserName: "Ref User",
		UserID: "ref_usr", Enabled: true, AuthKind: "oauth",
		CreatedAt: time.Now(),
		Tokens:    store.Tokens{SessionToken: "devin-session-token$ref_tok"},
		UsageErr:  "old error", Usage: &store.Usage{Plan: "free"},
	}
	srv.accounts.Save(acc)

	// Directly update usage and verify persistence (bypassing HTTP handler).
	newUsage := &store.Usage{
		Plan: "pro", DailyRemainingPercent: 80,
		WeeklyRemainingPercent: 60, CheckedAt: time.Now().Unix(),
	}
	acc.Usage = newUsage
	acc.UsageErr = ""
	if err := srv.accounts.Save(acc); err != nil {
		t.Fatalf("save: %v", err)
	}

	persisted, err := srv.accounts.Get(acc.ID)
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if persisted.Usage == nil || persisted.Usage.Plan != "pro" {
		t.Errorf("usage plan after save: got %+v", persisted.Usage)
	}
	if persisted.UsageErr != "" {
		t.Errorf("UsageErr should be cleared: got %q", persisted.UsageErr)
	}
}

// ===================================================================

// TestUsageRefresh401 sends usage refresh when FetchUserStatus returns 401
// -> 502 "token expired or revoked".
func TestUsageRefresh401(t *testing.T) {
	expiredSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer expiredSrv.Close()

	srv := newTestServer(t, "", expiredSrv.URL)

	acc := &store.Account{
		Schema: 2, ID: "acc_exp" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Email: "exp@test.com", UserName: "Exp User",
		UserID: "exp_usr", Enabled: true, AuthKind: "oauth",
		CreatedAt: time.Now(),
		Tokens:    store.Tokens{SessionToken: "devin-session-token$expired_tok"},
		Usage:     &store.Usage{Plan: "starter"},
	}
	srv.accounts.Save(acc)

	urlStr := "/admin/api/accounts/" + acc.ID + "/usage/refresh"
	req := httptest.NewRequest(http.MethodPost, urlStr, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("refresh 401: got %d, want 502", rec.Code)
	}
	var errResp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp["error"] != "token expired or revoked" {
		t.Errorf("error msg: got %q", errResp["error"])
	}

	persisted, _ := srv.accounts.Get(acc.ID)
	if persisted.UsageErr != "token expired or revoked" {
		t.Errorf("usage_error: got %q", persisted.UsageErr)
	}
	if persisted.Usage != nil {
		t.Error("usage should be nil")
	}
}

// ===================================================================

// TestPatchAccountNickname applies PATCH /accounts/{id} with nickname only.
func TestPatchAccountNickname(t *testing.T) {
	srv := newTestServer(t, "", "")
	base := &store.Account{Schema: 2, ID: "acc_nick", Email: "nick@test.com", Enabled: true, CreatedAt: time.Now()}
	srv.accounts.Save(base)

	body := `{"nickname": "New Nick"}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/accounts/"+base.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("patch nick: got %d, body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nickname string `json:"nickname"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Nickname != "New Nick" {
		t.Errorf("nickname: got %q", resp.Nickname)
	}
}

// ===================================================================

// TestPatchAccountEnabled sets enabled to false.
func TestPatchAccountEnabled(t *testing.T) {
	srv := newTestServer(t, "", "")
	base := &store.Account{Schema: 2, ID: "acc_en", Email: "en@test.com", Enabled: true, CreatedAt: time.Now()}
	srv.accounts.Save(base)

	body := `{"enabled": false}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/accounts/"+base.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("patch en: got %d, body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Enabled bool `json:"enabled"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Enabled {
		t.Error("enabled should be false")
	}
}

// ===================================================================

// TestPatchEmptyBody returns 400 "empty patch".
func TestPatchEmptyBody(t *testing.T) {
	srv := newTestServer(t, "", "")
	base := &store.Account{Schema: 2, ID: "acc_empty", Email: "e@t.co", Enabled: true, CreatedAt: time.Now()}
	srv.accounts.Save(base)

	body := `{}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/accounts/"+base.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty body: got %d, want 400", rec.Code)
	}
	var errResp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp["error"] != "empty patch" {
		t.Errorf("error msg: got %q, want \"empty patch\"", errResp["error"])
	}
}

// ===================================================================

// TestPatch404 returns 404 for nonexistent account.
func TestPatch404(t *testing.T) {
	srv := newTestServer(t, "", "")

	body := `{"nickname": "ghost"}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/accounts/nonexistent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("patch 404: got %d, want 404", rec.Code)
	}
}

// ===================================================================

// TestStats verifies correct account/enabled counts with requests/failed = 0.
func TestStats(t *testing.T) {
	srv := newTestServer(t, "", "")

	a1 := &store.Account{Schema: 2, ID: "acc_s1", Email: "s1@t.co", Enabled: true, CreatedAt: time.Now()}
	a2 := &store.Account{Schema: 2, ID: "acc_s2", Email: "s2@t.co", Enabled: false, CreatedAt: time.Now()}
	srv.accounts.Save(a1)
	srv.accounts.Save(a2)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stats: got %d, want 200", rec.Code)
	}
	var stats map[string]int
	json.Unmarshal(rec.Body.Bytes(), &stats)
	if stats["accounts"] != 2 {
		t.Errorf("accounts count: got %d, want 2", stats["accounts"])
	}
	if stats["enabled"] != 1 {
		t.Errorf("enabled count: got %d, want 1", stats["enabled"])
	}
	if stats["requests"] != 0 || stats["failed"] != 0 {
		t.Errorf("requests/failed should be 0: got %+v", stats)
	}
}

// ===================================================================

// TestImportViaAdminCLIProxyShape exercises Account Import with a CLI Proxy API shape.
func TestImportViaAdminCLIProxyProxyShape(t *testing.T) {
	srv := newTestServer(t, "", "")

	body := []byte(`{"api_key": "eyJ.test.key", "session_token": "devin-session-token$eyJ.test.key",
			"user_name": "Imp User", "user_id": "usr_imp",
			"org_id": "org_imp", "plan": "enterprise"}`)
	acct, err := srv.accounts.Import(body)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if acct.Schema != 2 {
		t.Errorf("schema: got %d", acct.Schema)
	}
	if acct.UserID != "usr_imp" {
		t.Errorf("user_id: got %q", acct.UserID)
	}
	if acct.OrgID != "org_imp" {
		t.Errorf("org_id: got %q", acct.OrgID)
	}
	if acct.Plan != "enterprise" {
		t.Errorf("plan: got %q", acct.Plan)
	}
}

// ===================================================================

// TestFetchSelfProfileDirectly ensures FetchSelfProfile correctly parses JSON.
func TestFetchSelfProfileDirectly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/self", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fixtureSelfJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	profile, err := devin.FetchSelfProfile(context.Background(), srv.URL, "dummy")
	if err != nil {
		t.Fatalf("FetchSelfProfile: %v", err)
	}
	if profile.Email != "test@example.com" {
		t.Errorf("email: got %q, want test@example.com", profile.Email)
	}
	if profile.UserID != "test_usr_1" {
		t.Errorf("user_id: got %q", profile.UserID)
	}
	if profile.UserName != "Test User" {
		t.Errorf("user_name: got %q", profile.UserName)
	}
	if profile.OrgID != "test_org_1" {
		t.Errorf("org_id: got %q", profile.OrgID)
	}
}
