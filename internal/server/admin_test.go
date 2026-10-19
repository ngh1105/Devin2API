package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

// ---------------------------------------------------------------------------
// Wire type constant for protobuf helpers
// ---------------------------------------------------------------------------

const wireBytesVal = 2 // same as devin.WireTypeBytes

func pwTag(field int, w byte) []byte       { return devin.PwTag(field, int(w)) }
func pwVarint(v uint64) []byte             { return devin.PwVarint(v) }
func pwString(f int, s string) []byte      { return devin.PwString(f, s) }
func pwFieldVarint(f int, v uint64) []byte { return devin.PwFieldVarint(f, v) }

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
// Tests
// ===================================================================

// TestLoginStartReplaces verifies login/start cancellation, gen bump, and port-closure.
func TestLoginStartReplaces(t *testing.T) {
	srv := newTestServer(t, "", "")

	// --- first start ---------------------------------------------------------
	req1 := adminRequest(http.MethodPost, "/admin/api/login/start", nil)
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

	stReq := adminRequest(http.MethodGet, "/admin/api/login/status", nil)
	stRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(stRec, stReq)
	var stResp map[string]string
	json.Unmarshal(stRec.Body.Bytes(), &stResp)
	if stResp["status"] != "pending" {
		t.Errorf("status after first start: got %q, want \"pending\"", stResp["status"])
	}

	// Capture FIRST login's port BEFORE starting second (Fix finding #3).
	firstPort := 0
	if srv.loginMngr.current != nil {
		firstPort = srv.loginMngr.current.Port
	}

	// --- second start replaces first -----------------------------------------
	req2 := adminRequest(http.MethodPost, "/admin/api/login/start", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	var resp2 struct {
		LoginURL string `json:"login_url"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2.LoginURL == "" {
		t.Fatal("expected non-empty login_url")
	}

	// --- Port closure assertion: dial captured port via net.DialTimeout --------
	if firstPort > 0 {
		cancelReq := adminRequest(http.MethodPost, "/admin/api/login/cancel", nil)
		cancelRec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(cancelRec, cancelReq)

		addr := "127.0.0.1:" + strconv.Itoa(firstPort)
		conn, dialErr := (&net.Dialer{Timeout: 500 * time.Millisecond}).Dial("tcp", addr)
		if conn != nil {
			conn.Close()
			t.Error("expected port to be closed, but dial succeeded")
		}
		if dialErr == nil {
			t.Error("expected dial timeout/failure on stopped login port")
		}
	}

	checkIdle := adminRequest(http.MethodGet, "/admin/api/login/status", nil)
	checkRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(checkRec, checkIdle)
	var idleSt map[string]string
	json.Unmarshal(checkRec.Body.Bytes(), &idleSt)
	if idleSt["status"] != "idle" {
		t.Errorf("status after cancel: got %q, want \"idle\"", idleSt["status"])
	}
}

// ===================================================================

// buildUserStatusProto builds valid protobuf bytes matching devin/userstatus_test.go's
// buildResponseFixture. Parses into Usage: plan="Pro", daily=42, weekly=77.
func buildUserStatusProto() []byte {
	// PlanInfo: field 2="Pro", field 33=OrgInfo{4:"org-planid",8:"Test Org"}
	var pi []byte
	pi = append(pi, devin.PwString(2, "Pro")...)
	orgInner := devin.PwFieldString(4, "test_org_info")
	orgInner = append(orgInner, devin.PwFieldString(8, "Test Org Name")...)
	// Field 33 is multi-byte tag: (33<<3)|2 = 0xFA2 → varint [0x82, 0x01]
	pi = append(pi, devin.PwTag(33, devin.WireTypeBytes)...)
	pi = append(pi, devin.PwVarint(uint64(len(orgInner)))...)
	pi = append(pi, orgInner...)

	// PlanStatus: field 1=PlanInfo(bytes), field 14=42, field 15=77
	ps := devin.PwString(1, string(pi))
	ps = append(ps, devin.PwFieldVarint(14, 42)...)
	ps = append(ps, devin.PwFieldVarint(15, 77)...)

	// UserStatus inner: flat fields
	var inner []byte
	inner = append(inner, devin.PwString(3, "Test User")...)
	inner = append(inner, devin.PwString(5, "team-abc")...)
	inner = append(inner, devin.PwString(7, "test@example.com")...)
	// Wrap ps in bytes for field 13
	inner = append(inner, devin.PwString(13, string(ps))...)
	inner = append(inner, devin.PwString(36, "usr_refresh")...)

	// Outer envelope: wrap all as field 1 (bytes)
	out := devin.PwTag(1, devin.WireTypeBytes)
	out = append(out, devin.PwVarint(uint64(len(inner)))...)
	out = append(out, inner...)
	return out
}

// ===================================================================

// TestLoginLifecycleFull exercises the login pipeline with mock API servers
// TestLoginLifecycleFull exercises real loopback binding, callback delivery, and
// TestLoginLifecycleFull exercises real loopback binding via StartLogin, then
// validates the end-to-end pipeline (exchange → profile → save account) using
// direct function calls. The callback hit proves networking works; remaining
// assertions verify store state after processing.
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

	// POST start — verifies StartLogin binds real loopback port.
	startReq := adminRequest(http.MethodPost, "/admin/api/login/start", nil)
	startRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start: got %d, want 200", startRec.Code)
	}

	st := srv.loginMngr.current
	if st == nil || st.Port == 0 {
		t.Fatal("missing active login state or port")
	}

	// Hit REAL loopback callback per oauth_test.go pattern — proves networking works.
	callbackURL := "http://127.0.0.1:" + strconv.Itoa(st.Port) +
		"/callback?code=testcode&state=" + url.QueryEscape(st.State)
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status: got %d", resp.StatusCode)
	}

	// Execute post-callback steps manually (same as background goroutine does):
	rawToken := "raw_oauth_token_abc"
	profile, pErr := devin.FetchSelfProfile(context.Background(), apiSrv.URL, devin.FormatSessionToken(rawToken))
	if pErr != nil {
		t.Fatalf("FetchSelfProfile: %v", pErr)
	}

	acc := store.AccountFromLogin(profile.UserName, profile.UserID, profile.OrgID, rawToken, profile.Email, nil)
	if err := srv.accounts.Save(acc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	curGen := srv.loginMngr.genSnapshot()
	srv.loginMngr.ResolveComplete(acc.ID, curGen)

	// Verify manager state transitioned to complete.
	statusReq := adminRequest(http.MethodGet, "/admin/api/login/status", nil)
	statusRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(statusRec, statusReq)
	var finalStatus map[string]string
	json.Unmarshal(statusRec.Body.Bytes(), &finalStatus)
	if finalStatus["status"] != "complete" {
		t.Fatalf("final status: got %q, want complete (%v)", finalStatus["status"], finalStatus)
	}
	acctID := finalStatus["account_id"]
	if acctID == "" {
		t.Fatal("missing account_id")
	}
	finalAcc, err := srv.accounts.Get(acctID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if finalAcc.Email != "test@example.com" {
		t.Errorf("email: got %q", finalAcc.Email)
	}
	if finalAcc.UserID != "test_usr_1" {
		t.Errorf("user_id: got %q", finalAcc.UserID)
	}
	if finalAcc.UserName != "Test User" {
		t.Errorf("user_name: got %q", finalAcc.UserName)
	}
}

// ===================================================================

// TestUsageRefreshSuccess exercises POST /admin/api/accounts/{id}/usage/refresh
// with a valid mock userstatus server returning protobuf fixture data. Verifies
// usage merge, checked_at set, and UsageErr cleared.
func TestUsageRefreshSuccess(t *testing.T) {
	protoBody := buildUserStatusProto()
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/proto")
		w.Write(protoBody)
	}))
	defer mockSrv.Close()

	srv := newTestServer(t, "", mockSrv.URL)
	seedAccount := &store.Account{
		Schema: 2, ID: "acc_ref" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Email: "ref@test.com", UserName: "Ref User",
		UserID: "ref_usr", Enabled: true, AuthKind: "oauth",
		CreatedAt: time.Now(),
		Tokens:    store.Tokens{SessionToken: "devin-session-token$ref_tok"},
		UsageErr:  "old error", Usage: &store.Usage{Plan: "free"},
	}
	srv.accounts.Save(seedAccount)

	urlStr := "/admin/api/accounts/" + seedAccount.ID + "/usage/refresh"
	req := adminRequest(http.MethodPost, urlStr, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status: got %d, body=%q, want 200", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID       string       `json:"id"`
		Usage    *store.Usage `json:"usage"`
		UsageErr string       `json:"usage_error,omitempty"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Usage == nil {
		t.Fatal("response Usage should be non-nil")
	}
	if resp.UsageErr != "" {
		t.Errorf("UsageErr should be cleared: got %q", resp.UsageErr)
	}
	if resp.Usage.Plan != "Pro" {
		t.Errorf("plan after refresh: got %q, want Pro", resp.Usage.Plan)
	}
	if resp.Usage.DailyRemainingPercent != 42 {
		t.Errorf("daily_remaining_percent: got %d, want 42", resp.Usage.DailyRemainingPercent)
	}
	if resp.Usage.WeeklyRemainingPercent != 77 {
		t.Errorf("weekly_remaining_percent: got %d, want 77", resp.Usage.WeeklyRemainingPercent)
	}
	persisted, err := srv.accounts.Get(seedAccount.ID)
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if persisted.Usage == nil || persisted.Usage.Plan != "Pro" {
		t.Errorf("persisted plan: got %q", persisted.Usage.Plan)
	}
	if persisted.Usage.CheckedAt == 0 {
		t.Error("persisted checked_at should be > 0")
	}
	if persisted.UsageErr != "" {
		t.Errorf("persisted UsageErr should be cleared: got %q", persisted.UsageErr)
	}
}

// ===================================================================

// TestUsageRefreshNoSessionToken returns 400 when account has no session token.
func TestUsageRefreshNoSessionToken(t *testing.T) {
	srv := newTestServer(t, "", "")

	base := &store.Account{
		Schema: 2, ID: "acc_no_st", Email: "nosession@t.co", Enabled: true,
		CreatedAt: time.Now(), Tokens: store.Tokens{},
	}
	srv.accounts.Save(base)

	urlStr := "/admin/api/accounts/" + base.ID + "/usage/refresh"
	req := adminRequest(http.MethodPost, urlStr, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("no session token: got %d, want 400", rec.Code)
	}
	var errResp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp["error"] != "no session token" {
		t.Errorf("error msg: got %q", errResp["error"])
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
	req := adminRequest(http.MethodPost, urlStr, nil)
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
	req := adminRequest(http.MethodPatch, "/admin/api/accounts/"+base.ID, strings.NewReader(body))
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
	req := adminRequest(http.MethodPatch, "/admin/api/accounts/"+base.ID, strings.NewReader(body))
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
	req := adminRequest(http.MethodPatch, "/admin/api/accounts/"+base.ID, strings.NewReader(body))
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
	req := adminRequest(http.MethodPatch, "/admin/api/accounts/nonexistent", strings.NewReader(body))
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

	req := adminRequest(http.MethodGet, "/admin/api/stats", nil)
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

// ===================================================================

// TestGenerationGuard verifies stale ResolveError/ResolveComplete calls from
// old goroutines are ignored after a new login start bumps the gen counter.
func TestGenerationGuard(t *testing.T) {
	srv := newTestServer(t, "", "")

	// First login start
	srv.loginMngr.SetPending(nil) // simulate: gen becomes 1
	oldGen := srv.loginMngr.genSnapshot()

	// Second login start bumps gen
	srv.loginMngr.SetPending(nil) // gen becomes 2

	// Old resolve should be ignored because gen != current
	srv.loginMngr.ResolveComplete("stale_acc_id", oldGen)

	code, aid, _ := srv.loginMngr.Status()
	if code != "pending" {
		t.Errorf("status should still be pending, got %q", code)
	}
	if aid != "" {
		t.Errorf("account_id should be empty after stale resolve, got %q", aid)
	}

	// New resolve with current gen should succeed
	curGen := srv.loginMngr.genSnapshot()
	srv.loginMngr.ResolveComplete("valid_acc_id", curGen)

	code, aid, _ = srv.loginMngr.Status()
	if code != "complete" {
		t.Errorf("status should be complete, got %q", code)
	}
	if aid != "valid_acc_id" {
		t.Errorf("account_id should be valid_acc_id, got %q", aid)
	}
}

// ===================================================================

// TestAdminTokenLifecycle exercises POST/GET/DELETE /admin/api/tokens.
func TestAdminTokenLifecycle(t *testing.T) {
	srv := newTestServer(t, "", "")

	// Create a token.
	createReq := adminRequest(http.MethodPost, "/admin/api/tokens", nil)
	createRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create token: got %d, want 201", createRec.Code)
	}
	var createdTok map[string]string
	json.Unmarshal(createRec.Body.Bytes(), &createdTok)
	tokenID := createdTok["id"]
	if tokenID == "" {
		t.Fatal("expected non-empty id in response")
	}

	// List tokens — expect 1.
	listReq := adminRequest(http.MethodGet, "/admin/api/tokens", nil)
	listRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list tokens: got %d", listRec.Code)
	}
	var toks []map[string]string
	json.Unmarshal(listRec.Body.Bytes(), &toks)
	if len(toks) != 1 {
		t.Fatalf("expected 1 token, got %d", len(toks))
	}
	if toks[0]["id"] != tokenID {
		t.Errorf("token id = %q; want %q", toks[0]["id"], tokenID)
	}

	// Delete the token.
	delReq := adminRequest(http.MethodDelete, "/admin/api/tokens/"+tokenID, nil)
	delRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete token: got %d, want 204", delRec.Code)
	}

	// List again — now empty.
	listReq2 := adminRequest(http.MethodGet, "/admin/api/tokens", nil)
	listRec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(listRec2, listReq2)
	if listRec2.Code != http.StatusOK {
		t.Fatalf("list after delete: got %d", listRec2.Code)
	}
	var toks2 []map[string]string
	json.Unmarshal(listRec2.Body.Bytes(), &toks2)
	if len(toks2) != 0 {
		t.Errorf("expected 0 tokens after delete, got %d", len(toks2))
	}
}

// ===================================================================

// TestLoginLifecycleAsync exercises the full async login pipeline with real loopback binding,
// callback delivery, status polling, and final store verification. Proves the gen-fix works.
func TestLoginLifecycleAsync(t *testing.T) {
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

	// Start login via handler.
	startReq := adminRequest(http.MethodPost, "/admin/api/login/start", nil)
	startRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start: got %d, body=%q", startRec.Code, startRec.Body.String())
	}

	st := srv.loginMngr.current
	if st == nil || st.Port == 0 {
		t.Fatal("missing active login state or port")
	}

	// Hit REAL loopback callback per oauth_test.go pattern.
	callbackURL := "http://127.0.0.1:" + strconv.Itoa(st.Port) +
		"/callback?code=testcode&state=" + url.QueryEscape(st.State)
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("callback dial: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status: got %d", resp.StatusCode)
	}

	// Poll GET /admin/api/login/status until complete or timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		statusReq := adminRequest(http.MethodGet, "/admin/api/login/status", nil)
		statusRec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(statusRec, statusReq)
		var statusResp map[string]string
		json.Unmarshal(statusRec.Body.Bytes(), &statusResp)

		code := statusResp["status"]
		switch code {
		case "complete":
			// Verified: async pipeline succeeded.
			acctID := statusResp["account_id"]
			finalAcc, err := srv.accounts.Get(acctID)
			if err != nil {
				t.Fatalf("get account after poll: %v", err)
			}
			if finalAcc.Email != "test@example.com" {
				t.Errorf("email: got %q; want %q", finalAcc.Email, "test@example.com")
			}
			if finalAcc.UserName != "Test User" {
				t.Errorf("user_name: got %q; want %q", finalAcc.UserName, "Test User")
			}
			if finalAcc.UserID != "test_usr_1" {
				t.Errorf("user_id: got %q; want %q", finalAcc.UserID, "test_usr_1")
			}
			if !strings.HasPrefix(finalAcc.Tokens.SessionToken, "devin-session-token$") {
				t.Errorf("session_token missing prefix: got %q", finalAcc.Tokens.SessionToken)
			}
			return

		case "error":
			t.Fatalf("login ended with error: %s", statusResp["error"])

		case "idle":
			// Stale — no pending login started yet; shouldn't happen.
			t.Fatal("status idle before callback was delivered")
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// ===================================================================

// adminRequest builds a request the admin guard accepts: loopback Host and
// the X-Devin2API marker header the panel sends.
func adminRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-Devin2API", "1")
	return req
}

// TestAdminGuards verifies the DNS-rebinding, cross-origin and CSRF checks.
func TestAdminGuards(t *testing.T) {
	srv := New(config.Config{Listen: ":0"}, mustAccounts(t), mustTokens(t))
	cases := []struct {
		name   string
		method string
		mutate func(r *http.Request)
		want   int
	}{
		{"rebinding host", http.MethodGet, func(r *http.Request) { r.Host = "evil.example:8080" }, http.StatusForbidden},
		{"cross origin", http.MethodGet, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		{"post without marker", http.MethodPost, func(r *http.Request) { r.Header.Del("X-Devin2API") }, http.StatusForbidden},
		{"same origin ok", http.MethodGet, func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:8080") }, http.StatusOK},
		{"localhost ok", http.MethodGet, func(r *http.Request) { r.Host = "localhost:8080" }, http.StatusOK},
		{"ipv6 loopback ok", http.MethodGet, func(r *http.Request) { r.Host = "[::1]:8080" }, http.StatusOK},
	}
	for _, tc := range cases {
		req := adminRequest(tc.method, "/admin/api/tokens", nil)
		tc.mutate(req)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

// TestAdminMasksSecrets verifies list responses never carry plaintext
// session tokens or API keys.
func TestAdminMasksSecrets(t *testing.T) {
	accs := mustAccounts(t)
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	acc := store.AccountFromLogin("u", "usr_mask", "org", "eyJsecret.session.value", "m@x.io", nil)
	if err := accs.Save(acc); err != nil {
		t.Fatal(err)
	}
	tok, _ := srv.tokens.Create()

	for _, path := range []string{"/admin/api/accounts", "/admin/api/tokens"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, adminRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		if strings.Contains(body, "eyJsecret.session.value") || strings.Contains(body, tok.Token) {
			t.Errorf("%s leaked a plaintext secret: %s", path, body)
		}
	}
}

// TestAdminRejectsTraversalID verifies ids that could escape the data dir 404.
func TestAdminRejectsTraversalID(t *testing.T) {
	srv := New(config.Config{Listen: ":0"}, mustAccounts(t), mustTokens(t))
	for _, p := range []string{"/admin/api/accounts/..%2F..%2Fx", "/admin/api/accounts/a.b/usage/refresh"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, adminRequest(http.MethodDelete, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", p, rec.Code)
		}
	}
}

func mustAccounts(t *testing.T) *store.AccountStore {
	t.Helper()
	s, err := store.NewAccountStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustTokens(t *testing.T) *store.TokenStore {
	t.Helper()
	s, err := store.NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
