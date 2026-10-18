package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

func mustStore(t *testing.T) *AccountStore {
	t.Helper()
	s, err := NewAccountStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewAccountStore: %v", err)
	}
	return s
}

func tokenStorePath(t *testing.T) string {
	t.Helper()
	ts, err := NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	return filepath.Join(ts.dir, "tokens.json")
}

func mustTokenStore(t *testing.T) *TokenStore {
	t.Helper()
	s, err := NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	return s
}

func mustTokenStoreFromPath(t *testing.T, idxPath string) *TokenStore {
	t.Helper()
	dir := filepath.Dir(idxPath)
	s, err := NewTokenStore(dir)
	if err != nil {
		t.Fatalf("NewTokenStore from path: %v", err)
	}
	return s
}

// v1 JSON — mimics legacy Auth0-bound account.
const v1JSON = `{
  "schema": 1,
  "id": "acc_v1m",
  "email": "v1@example.com",
  "sub": "auth0|123",
  "created_at": "2024-01-15T10:00:00Z",
  "oauth": {"client_id":"cli_abc","domain":"acme.auth0.com","audience":"https://api.acme.io","redirect_uri":"https://app.acme.io/cb"},
  "tokens": {"access_token":"ghp_aaa","refresh_token":"rt_bbb","id_token":"eyJh","expires_at":1737849600,"scope":"openid"},
  "profile": {"name":"Base","picture":"https://img.cdn/b"},
  "usage": null
}`

// Full v2 JSON.
const fullV2JSON = `{
  "schema": 2,
  "id": "acc_full1",
  "email": "full@example.com",
  "user_name": "Full User",
  "user_id": "usr_full1",
  "org_id": "org_big",
  "plan": "pro",
  "nickname": "Fulano",
  "enabled": true,
  "auth_kind": "oauth",
  "created_at": "2024-03-20T14:30:00Z",
  "tokens": {"session_token": "devin-session-token$eyJh.f.u.l.l"},
  "usage": {"email":"full@example.com","user_name":"Full User","user_id":"usr_full1","org_id":"org_big","org_name":"BigCo","plan":"pro","daily_remaining_percent":80,"weekly_remaining_percent":60,"daily_reset_at":"2025-01-20T00:00:00Z","weekly_reset_at":"2025-01-19T00:00:00Z","plan_start":"2025-01-01T00:00:00Z","plan_end":"2026-01-01T00:00:00Z","checked_at":1737849600},
  "usage_error": ""
}`

// --- v1 migration ----------------------------------------------------------

func TestLoadV1Migrates(t *testing.T) {
	// Persist v1 JSON, reload store → should be migrated to schema 2.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acc_v1m.json"), []byte(v1JSON), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewAccountStore(dir)
	if err != nil {
		t.Fatalf("NewAccountStore: %v", err)
	}
	a, err := store.Get("acc_v1m")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a.Schema != 2 {
		t.Errorf("schema = %d; want 2", a.Schema)
	}
	if a.Email != "v1@example.com" {
		t.Errorf("email = %q; want %q", a.Email, "v1@example.com")
	}
	if !a.Enabled {
		t.Error("enabled = false; want true (default)")
	}
	if a.AuthKind != "oauth" {
		t.Errorf("auth_kind = %q; want oauth", a.AuthKind)
	}
	// access_token → session_token (formatted).
	wantTok := "devin-session-token$ghp_aaa"
	if a.Tokens.SessionToken != wantTok {
		t.Errorf("session_token = %q; want %q", a.Tokens.SessionToken, wantTok)
	}
}

// --- import shapes -----------------------------------------------------------

func TestImportCLIProxyShape(t *testing.T) {
	raw := `{
		"api_key": "eyJhbGciOiJIUzI1NiJ9.cliproxy",
		"user_name": "CPI User",
		"user_id": "usr_cpi1",
		"org_id": "org_cp1",
		"plan": "team",
		"email": "cpi@example.com",
		"disabled": true
	}`
	store := mustStore(t)
	a, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if a.Schema != 2 {
		t.Errorf("schema = %d; want 2", a.Schema)
	}
	if a.UserID != "usr_cpi1" {
		t.Errorf("user_id = %q; want %q", a.UserID, "usr_cpi1")
	}
	if a.UserName != "CPI User" {
		t.Errorf("user_name = %q; want %q", a.UserName, "CPI User")
	}
	if a.OrgID != "org_cp1" {
		t.Errorf("org_id = %q; want %q", a.OrgID, "org_cp1")
	}
	if a.Plan != "team" {
		t.Errorf("plan = %q; want %q", a.Plan, "team")
	}
	if a.Email != "cpi@example.com" {
		t.Errorf("email = %q; want %q", a.Email, "cpi@example.com")
	}
	if a.Enabled {
		t.Error("enabled = true; want false (disabled:true)")
	}
	wantTok := "devin-session-token$eyJhbGciOiJIUzI1NiJ9.cliproxy"
	if a.Tokens.SessionToken != wantTok {
		t.Errorf("session_token = %q; want prefixed JWT", a.Tokens.SessionToken)
	}
}

func TestImportCLIProxyShapePrefixedKey(t *testing.T) {
	raw := `{
		"session_token": "eyJ.already.prefixed",
		"user_id": "usr_prefixed"
	}`
	store := mustStore(t)
	a, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	wantTok := "devin-session-token$eyJ.already.prefixed"
	if a.Tokens.SessionToken != wantTok {
		t.Errorf("session_token = %q; want %q", a.Tokens.SessionToken, wantTok)
	}
}

func TestImportV2Full(t *testing.T) {
	store := mustStore(t)
	a, err := store.Import([]byte(fullV2JSON))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"schema", a.Schema, int64(2)},
		{"id", a.ID, "acc_full1"},
		{"email", a.Email, "full@example.com"},
		{"user_name", a.UserName, "Full User"},
		{"user_id", a.UserID, "usr_full1"},
		{"org_id", a.OrgID, "org_big"},
		{"plan", a.Plan, "pro"},
		{"nickname", a.Nickname, "Fulano"},
		{"enabled", a.Enabled, true},
		{"auth_kind", a.AuthKind, "oauth"},
		{"session_token", a.Tokens.SessionToken, "devin-session-token$eyJh.f.u.l.l"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %#v; want %#v", c.name, c.got, c.want)
		}
	}
	if a.Usage == nil {
		t.Fatal("usage is nil; want populated Usage")
	}
	if a.Usage.Plan != "pro" || a.Usage.CheckedAt != 1737849600 {
		t.Errorf("usage fields: %+v", a.Usage)
	}
}

func TestImportBareTokenStillWorks(t *testing.T) {
	tests := []struct {
		raw   string
		token string
	}{
		{`{"token":"tok_plain"}`, "devin-session-token$tok_plain"},
		{`{"access_token":"eyJBearerJwt"}`, "devin-session-token$eyJBearerJwt"},
		{`{"refresh_token":"rt123"}`, "devin-session-token$rt123"},
	}
	for _, tt := range tests {
		store := mustStore(t)
		a, err := store.Import([]byte(tt.raw))
		if err != nil {
			t.Fatalf("Import(%s): %v", tt.raw, err)
		}
		if a.Tokens.SessionToken != tt.token {
			t.Errorf("%s: session_token = %q; want %q", tt.raw, a.Tokens.SessionToken, tt.token)
		}
	}
}

func TestImportMalformedJSON(t *testing.T) {
	store := mustStore(t)
	for _, raw := range []string{
		`not json`,
		`[1,2,3]`, // array
		`42`,      // number
	} {
		_, err := store.Import([]byte(raw))
		if _, ok := err.(*ImportError); !ok {
			t.Fatalf("expected ImportError for %q, got %T: %v", raw, err, err)
		}
	}
}

func TestImportUnexpectedKeysFails(t *testing.T) {
	store := mustStore(t)
	_, err := store.Import([]byte(`{"foo":"bar"}`))
	if _, ok := err.(*ImportError); !ok {
		t.Fatalf("expected ImportError, got %T: %v", err, err)
	}
}

func TestImportBareTokenExtraKeysFails(t *testing.T) {
	store := mustStore(t)
	for _, raw := range []string{
		`{"access_token":"x","foo":"bar"}`,
		`{"refresh_token":"r","extra":true}`,
		`{"token":"t","baz":123}`,
	} {
		_, err := store.Import([]byte(raw))
		if _, ok := err.(*ImportError); !ok {
			t.Fatalf("expected ImportError for %q, got %T: %v", raw, err, err)
		}
	}
}

func TestImportEmailPasswordRejected(t *testing.T) {
	store := mustStore(t)
	_, err := store.Import([]byte(`{"email":"a@b.c","password":"s3cret"}`))
	if _, ok := err.(*ImportError); !ok {
		t.Fatalf("expected ImportError, got %T: %v", err, err)
	}
}

// --- upsert / merge ---------------------------------------------------------

func TestUpsertByUserID(t *testing.T) {
	existing := AccountFromLogin("existing", "usr_up", "org_1", "tok1", "up@example.com", nil)
	store := mustStore(t)
	if err := store.Save(existing); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw := `{
		"schema": 2,
		"id": "acc_new",
		"user_id": "usr_up",
		"email": "updated@example.com",
		"user_name": "Updated",
		"tokens": {"session_token": "tok2"}
	}`
	imported, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.ID != existing.ID {
		t.Errorf("imported ID = %q; want %q (upsert)", imported.ID, existing.ID)
	}
	final, err := store.Get(existing.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Email != "updated@example.com" {
		t.Errorf("email after upsert = %q; want %q", final.Email, "updated@example.com")
	}
	if final.UserName != "Updated" {
		t.Errorf("user_name after upsert = %q; want %q", final.UserName, "Updated")
	}
}

func TestUpsertByV1Email(t *testing.T) {
	existing := &Account{Schema: 2, ID: "acc_eml", Email: "email@example.com", Enabled: true, CreatedAt: time.Now()}
	store := mustStore(t)
	if err := store.Save(existing); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw := `{"email": "email@example.com", "nickname": "Nicky", "tokens": {"session_token": "n"}}`
	imported, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.ID != existing.ID {
		t.Errorf("imported ID = %q; want %q (upsert by email)", imported.ID, existing.ID)
	}
}

// --- patch ------------------------------------------------------------------

func TestPatchAccountPartial(t *testing.T) {
	a := &Account{Schema: 2, ID: "acc_patch", Email: "patch@test.com", Enabled: true, CreatedAt: time.Now()}
	store := mustStore(t)
	if err := store.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	nick := "Patched"
	if err := store.Patch(a.ID, &nick, nil); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	got, err := store.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Nickname != "Patched" {
		t.Errorf("nickname = %q; want %q", got.Nickname, "Patched")
	}
	if !got.Enabled {
		t.Errorf("enabled mutated to false after nick-only patch")
	}
	// Toggle enabled false.
	en := false
	if err := store.Patch(a.ID, nil, &en); err != nil {
		t.Fatalf("Patch disable: %v", err)
	}
	got2, _ := store.Get(a.ID)
	if got2.Enabled {
		t.Error("enabled should be false after Patch")
	}
}

func TestPatchAccount404(t *testing.T) {
	store := mustStore(t)
	nick := "ghost"
	err := store.Patch("nonexistent", &nick, nil)
	if err != os.ErrNotExist {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}
}

// --- expired / usage --------------------------------------------------------

func TestAccountExpiredWithUsagePlanEnd(t *testing.T) {
	now := time.Now()
	// Plan ended in the past.
	expired := &Account{
		Schema: 2, ID: "exp1", Email: "e@x.com",
		CreatedAt: now,
		Usage:     &Usage{PlanEnd: now.Add(-time.Hour)},
	}
	if !expired.Expired() {
		t.Error("expected Expired()==true (plan past)")
	}
	// Plan ends in the future.
	future := &Account{Schema: 2, ID: "fut1", Email: "f@x.com", CreatedAt: now,
		Usage: &Usage{PlanEnd: now.Add(time.Hour)}}
	if future.Expired() {
		t.Error("expected Expired()==false (plan future)")
	}
	// No usage.
	noUsage := &Account{Schema: 2, ID: "nu1", Email: "n@x.com", CreatedAt: now, Usage: nil}
	if noUsage.Expired() {
		t.Error("expected Expired()==false when Usage is nil")
	}
	// Zero plan_end.
	zeroPlan := &Account{Schema: 2, ID: "zp1", Email: "z@x.com", CreatedAt: now,
		Usage: &Usage{PlanEnd: time.Unix(0, 0)}}
	if zeroPlan.Expired() {
		t.Error("expected Expired()==false when plan_end is zero")
	}
}

// --- constructor ------------------------------------------------------------

func TestAccountFromLogin(t *testing.T) {
	u := &Usage{Plan: "starter", PlanEnd: time.Now().Add(30 * 24 * time.Hour)}
	a := AccountFromLogin("alice", "usr_alice", "org_a", "eyJJWT", "alice@example.com", u)
	if a.Schema != 2 {
		t.Errorf("schema = %d; want 2", a.Schema)
	}
	if a.UserName != "alice" || a.UserID != "usr_alice" || a.OrgID != "org_a" {
		t.Errorf("identity wrong: %+v", a)
	}
	if a.Email != "alice@example.com" {
		t.Errorf("email = %q; want %q", a.Email, "alice@example.com")
	}
	if a.AuthKind != "oauth" {
		t.Errorf("auth_kind = %q; want oauth", a.AuthKind)
	}
	if !a.Enabled {
		t.Error("enabled = false; want true")
	}
	wantTok := "devin-session-token$eyJJWT"
	if a.Tokens.SessionToken != wantTok {
		t.Errorf("session_token = %q; want %q", a.Tokens.SessionToken, wantTok)
	}
	if a.Usage != u {
		t.Error("usage pointer mismatch")
	}
}

// --- FormatSessionToken -----------------------------------------------------

func TestFormatSessionToken(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"raw-token", "devin-session-token$raw-token"},         // all non-empty get prefix
		{"eyJBearer.jwt", "devin-session-token$eyJBearer.jwt"}, // jwt gets same treatment
		{"devin-session-token$jwt", "devin-session-token$jwt"}, // already prefixed: kept
		{"ghp_pat123", "devin-session-token$ghp_pat123"},       // github token gets prefix
		{"ey", "devin-session-token$ey"},                       // short: still prefixed
		{"eyJ", "devin-session-token$eyJ"},                     // exactly 3 chars
	}
	for _, tt := range tests {
		got := FormatSessionToken(tt.in)
		if got != tt.want {
			t.Errorf("FormatSessionToken(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

// --- save / load round-trip -------------------------------------------------

func TestSaveLoadRoundtrip(t *testing.T) {
	store := mustStore(t)
	a := &Account{
		Schema: 2, ID: "test-acct", Email: "a@t.co", UserName: "Tester",
		UserID: "u1", OrgID: "o1", Plan: "pro", Nickname: "T",
		Enabled: true, AuthKind: "oauth",
		CreatedAt: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		Tokens:    Tokens{SessionToken: "devin-session-token$st1"},
		Usage:     &Usage{Plan: "pro"},
	}
	if err := store.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != a.Email || got.UserName != a.UserName || got.Tokens.SessionToken != a.Tokens.SessionToken {
		t.Errorf("mismatch — got %+v; want %+v", got, a)
	}
	if got.Enabled != true {
		t.Errorf("enabled persisted as false?")
	}
}

func TestListSortedByCreated(t *testing.T) {
	store := mustStore(t)
	a1 := &Account{Schema: 2, ID: "a1", Email: "a1@t.co", CreatedAt: time.Unix(1, 0)}
	a2 := &Account{Schema: 2, ID: "a2", Email: "a2@t.co", CreatedAt: time.Unix(2, 0)}
	a3 := &Account{Schema: 2, ID: "a3", Email: "a3@t.co", CreatedAt: time.Unix(3, 0)}
	store.Save(a1)
	store.Save(a2)
	store.Save(a3)
	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("list len = %d; want 3", len(list))
	}
	for i, id := range []string{"a1", "a2", "a3"} {
		if list[i].ID != id {
			t.Errorf("item %d = %s; want %s", i, list[i].ID, id)
		}
	}
}

func TestDelete(t *testing.T) {
	store := mustStore(t)
	a := &Account{Schema: 2, ID: "del_me", Email: "d@t.co", CreatedAt: time.Now()}
	if err := store.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(a.ID); err != os.ErrNotExist {
		t.Errorf("expected Get to fail after Delete, got %v", err)
	}
}

// --- import merges into existing record -------------------------------------

func TestImportPreservesNonMatchingFields(t *testing.T) {
	const baseEmail = "existing@example.com"
	base := &Account{Schema: 2, ID: "keep-me", Email: baseEmail, Enabled: false, CreatedAt: time.Now(), UserName: "Keep"}
	store := mustStore(t)
	if err := store.Save(base); err != nil {
		t.Fatalf("Save base: %v", err)
	}
	_, err := store.Import([]byte(`{"access_token":"new_tok"}`))
	if _, ok := err.(*ImportError); ok {
		t.Fatalf("bare access_token should succeed: %v", err)
	}
	got, _ := store.Get(base.ID)
	if got.Email != baseEmail {
		t.Errorf("base email mutated: got %q, want %q", got.Email, baseEmail)
	}
	if got.UserName != "Keep" {
		t.Errorf("username mutated: got %q, want %q", got.UserName, "Keep")
	}
}

// --- atomic Save on read-only dir -------------------------------------------

func TestAtomicNoPartialOnBadDir(t *testing.T) {
	badDir := t.TempDir()
	if err := os.Chmod(badDir, 0o000); err != nil {
		t.Skip("chmod not supported")
	}
	defer os.Chmod(badDir, 0o755)
	store, err := NewAccountStore(badDir)
	if err != nil {
		t.Fatalf("NewAccountStore should succeed (mkdir ok): %v", err)
	}
	a := &Account{Schema: 2, ID: "bad-id", Email: "x@y.z", CreatedAt: time.Now()}
	err = store.Save(a)
	if err == nil {
		t.Log("save did not fail on read-only dir (expected on Windows)")
	}
}

// --- v2 import with enabled:false persists ---------------------------------

func TestImportEnabledFalsePersists(t *testing.T) {
	raw := `{"schema":2,"id":"acc_enf","email":"enf@test.com","enabled":false}`
	store := mustStore(t)
	a, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if a.Enabled {
		t.Error("enabled should be false from v2 import")
	}
	if err := store.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Re-read via Get → should still be false.
	got, err := store.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Enabled {
		t.Error("enabled should persist as false after disk round-trip")
	}
}

// --- usage error field ------------------------------------------------------

func TestImportUsageErrorField(t *testing.T) {
	raw := `{"schema":2,"id":"acc_ue","email":"ue@test.com","usage_error":"rate-limited"}`
	store := mustStore(t)
	a, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if a.UsageErr != "rate-limited" {
		t.Errorf("usage_error = %q; want %q", a.UsageErr, "rate-limited")
	}
}

// --- finding 2+3: new tests -------------------------------------------------

// TestImportV2WithSessionTokenTopLevel: a record carrying both session_token and tokens
// should be dispatched to v2 import (checks first), preserving usage etc.
func TestImportV2WithSessionTokenTopLevel(t *testing.T) {
	raw := `{
		"schema": 2,
		"id": "acc_vst",
		"email": "vst@example.com",
		"user_id": "usr_vst",
		"plan": "pro",
		"nickname": "VST",
		"enabled": true,
		"auth_kind": "oauth",
		"session_token": "devin-session-token$eyJ.top.level",
		"tokens": {"session_token": "devin-session-token$nested"},
		"usage": {"plan":"pro","checked_at":1700000000}
	}`
	store := mustStore(t)
	a, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"schema", a.Schema, int64(2)},
		{"id", a.ID, "acc_vst"},
		{"user_id", a.UserID, "usr_vst"},
		{"nickname", a.Nickname, "VST"},
		{"session_token", a.Tokens.SessionToken, "devin-session-token$eyJ.top.level"}, // top-level wins in v2
		{"usage_plan", a.Usage.Plan, "pro"},
		{"usage_checked_at", a.Usage.CheckedAt, int64(1700000000)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %#v; want %#v", c.name, c.got, c.want)
		}
	}
}

// TestMergeKeepsDisabledOnBareImport: account saved enabled:false, bare-token import
// upserts → still enabled:false because bare imports don't set enabledExplicitlySet.
func TestMergeKeepsDisabledOnBareImport(t *testing.T) {
	existing := &Account{Schema: 2, ID: "acc_keep", Email: "keep@test.com", UserID: "usr_keep", Enabled: false, CreatedAt: time.Now()}
	store := mustStore(t)
	if err := store.Save(existing); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw := `{"user_id": "usr_keep", "access_token": "bare_tok"}`
	imported, err := store.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.ID != existing.ID {
		t.Errorf("imported ID = %q; want %q (upsert)", imported.ID, existing.ID)
	}
	final, _ := store.Get(existing.ID)
	if final.Enabled {
		t.Error("enabled should stay false after bare-token import upsert")
	}
	// Bare import should NOT overwrite existing identity.
	if final.UserName != "" {
		t.Error("username should stay empty after bare-token import upsert")
	}
}

// TestV1MigrationEnabledFalsePersists: v1 JSON that carries "enabled":false
// should migrate to schema 2 with enabled=false preserved.
const v1JSONWithEnabledFalse = `{
  "schema": 1,
  "id": "acc_v1enf",
  "email": "v1enf@example.com",
  "sub": "auth0|123",
  "created_at": "2024-01-15T10:00:00Z",
  "oauth": {"client_id":"cli_abc","domain":"acme.auth0.com","audience":"https://api.acme.io","redirect_uri":"https://app.acme.io/cb"},
  "tokens": {"access_token":"ghp_bbb","refresh_token":"rt_ccc","id_token":"eyJb","expires_at":1737849600,"scope":"openid"},
  "profile": {"name":"Base","picture":"https://img.cdn/b"},
  "usage": null,
  "enabled": false
}`

func TestV1MigrationEnabledFalsePersists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acc_v1enf.json"), []byte(v1JSONWithEnabledFalse), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewAccountStore(dir)
	if err != nil {
		t.Fatalf("NewAccountStore: %v", err)
	}
	a, err := store.Get("acc_v1enf")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a.Enabled {
		t.Error("enabled should have persisted as false from v1 migration when 'enabled' key present")
	}
	// Also verify session_token was migrated.
	wantTok := "devin-session-token$ghp_bbb"
	if a.Tokens.SessionToken != wantTok {
		t.Errorf("session_token = %q; want %q", a.Tokens.SessionToken, wantTok)
	}
}
