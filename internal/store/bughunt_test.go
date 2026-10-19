package store

import (
	"os"
	"path/filepath"
	"testing"
)

func mustImport(t *testing.T, s *AccountStore, raw string) *Account {
	t.Helper()
	a, err := s.Import([]byte(raw))
	if err != nil {
		t.Fatalf("Import %s: %v", raw, err)
	}
	return a
}

// TestImportSameIDDifferentUserKeepsBoth: an import naming an existing id
// but a different user must not overwrite that account.
func TestImportSameIDDifferentUserKeepsBoth(t *testing.T) {
	s := mustStore(t)
	mustImport(t, s, `{"schema":2,"id":"acc_a","user_id":"u1","email":"a@x.io","tokens":{"session_token":"t1"}}`)
	got := mustImport(t, s, `{"schema":2,"id":"acc_a","user_id":"u2","email":"b@x.io","tokens":{"session_token":"t2"}}`)
	if got.ID == "acc_a" {
		t.Fatal("second user overwrote acc_a")
	}
	orig, _ := s.Get("acc_a")
	if orig.UserID != "u1" || orig.Tokens.SessionToken != FormatSessionToken("t1") {
		t.Errorf("acc_a = %+v", orig)
	}
}

// TestImportSameIDUpdatesInPlace: re-importing an export (same id, no
// identity) updates that account and keeps its nickname.
func TestImportSameIDUpdatesInPlace(t *testing.T) {
	s := mustStore(t)
	mustImport(t, s, `{"schema":2,"id":"acc_b","user_id":"u1","nickname":"work","tokens":{"session_token":"t1"}}`)
	got := mustImport(t, s, `{"schema":2,"id":"acc_b","tokens":{"session_token":"t2"}}`)
	if got.ID != "acc_b" || got.Nickname != "work" || got.UserID != "u1" || got.Tokens.SessionToken != FormatSessionToken("t2") {
		t.Errorf("got %+v", got)
	}
}

// TestEmailFallbackNeedsMissingUserID: same email, different user ids are
// different accounts.
func TestEmailFallbackNeedsMissingUserID(t *testing.T) {
	s := mustStore(t)
	mustImport(t, s, `{"schema":2,"user_id":"u1","email":"same@x.io","tokens":{"session_token":"t1"}}`)
	mustImport(t, s, `{"schema":2,"user_id":"u2","email":"same@x.io","tokens":{"session_token":"t2"}}`)
	if list, _ := s.List(); len(list) != 2 {
		t.Errorf("accounts = %d, want 2", len(list))
	}
	// Without a user id, the email still finds the account.
	got := mustImport(t, s, `{"schema":2,"email":"other@x.io","tokens":{"session_token":"t3"}}`)
	again := mustImport(t, s, `{"schema":2,"email":"other@x.io","tokens":{"session_token":"t4"}}`)
	if got.ID != again.ID {
		t.Error("email-only re-import created a duplicate")
	}
}

// TestReimportKeepsDisabled: an import without an enabled/disabled key
// must not re-enable an account the user disabled.
func TestReimportKeepsDisabled(t *testing.T) {
	s := mustStore(t)
	a := mustImport(t, s, `{"schema":2,"user_id":"u1","tokens":{"session_token":"t1"}}`)
	off := false
	s.Patch(a.ID, nil, &off)
	for _, raw := range []string{
		`{"schema":2,"user_id":"u1","tokens":{"session_token":"t2"}}`,
		`{"api_key":"t3","user_id":"u1"}`,
	} {
		if got := mustImport(t, s, raw); got.Enabled {
			t.Errorf("%s re-enabled the account", raw)
		}
	}
	if got := mustImport(t, s, `{"api_key":"t4","user_id":"u1","disabled":false}`); !got.Enabled {
		t.Error("explicit disabled:false did not enable")
	}
}

// TestBareTokenImportIsUsable: a new bare token import must be enabled, or
// chat routing never uses it.
func TestBareTokenImportIsUsable(t *testing.T) {
	s := mustStore(t)
	for _, raw := range []string{`{"token":"eyJa"}`, `{"access_token":"eyJb"}`} {
		if a := mustImport(t, s, raw); !a.Enabled || a.Tokens.SessionToken == "" {
			t.Errorf("%s: %+v", raw, a)
		}
	}
}

// TestImportV1ExportKeepsToken: a v1 export (tokens.access_token) imported
// through the panel must keep its token, formatted as a session token.
func TestImportV1ExportKeepsToken(t *testing.T) {
	s := mustStore(t)
	a := mustImport(t, s, `{"schema":2,"email":"v1@x.io","tokens":{"access_token":"eyJv1"}}`)
	if a.Tokens.SessionToken != FormatSessionToken("eyJv1") {
		t.Errorf("token = %q", a.Tokens.SessionToken)
	}
}

// TestV2FieldNamedAccessTokenStaysV2: a v2 record whose nickname is the
// string "access_token" must load as v2, not be migrated as v1.
func TestV2FieldNamedAccessTokenStaysV2(t *testing.T) {
	dir := t.TempDir()
	raw := `{"schema":2,"id":"acc_n","user_id":"u1","nickname":"access_token","enabled":true,"tokens":{"session_token":"devin-session-token$x"}}`
	os.WriteFile(filepath.Join(dir, "acc_n.json"), []byte(raw), 0o600)
	s, err := NewAccountStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("acc_n")
	if err != nil || got.UserID != "u1" || got.Nickname != "access_token" {
		t.Errorf("got %+v err %v", got, err)
	}
}

// TestDeleteRemovesSourceFile: an account loaded from a file not named
// <id>.json is deleted from disk and does not come back on reload.
func TestDeleteRemovesSourceFile(t *testing.T) {
	dir := t.TempDir()
	raw := `{"schema":2,"id":"acc_src","user_id":"u1","enabled":true,"tokens":{"session_token":"devin-session-token$x"}}`
	os.WriteFile(filepath.Join(dir, "imported-by-hand.json"), []byte(raw), 0o600)
	s, _ := NewAccountStore(dir)
	if err := s.Delete("acc_src"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	reloaded, _ := NewAccountStore(dir)
	if _, err := reloaded.Get("acc_src"); err == nil {
		t.Error("deleted account came back on reload")
	}
	// Deleting an account whose file is already gone succeeds.
	a := AccountFromLogin("u", "usr_gone", "org", "tok", "", nil)
	s.Save(a)
	os.Remove(filepath.Join(dir, a.ID+".json"))
	if err := s.Delete(a.ID); err != nil {
		t.Errorf("delete with missing file: %v", err)
	}
}

// TestSaveMovesLegacyFile: saving an account loaded from a legacy file name
// writes <id>.json and removes the old file, so no stale copy remains.
func TestSaveMovesLegacyFile(t *testing.T) {
	dir := t.TempDir()
	raw := `{"schema":2,"id":"acc_mv","user_id":"u1","enabled":true,"tokens":{"session_token":"devin-session-token$x"}}`
	legacy := filepath.Join(dir, "old-name.json")
	os.WriteFile(legacy, []byte(raw), 0o600)
	s, _ := NewAccountStore(dir)
	nick := "n"
	s.Patch("acc_mv", &nick, nil)
	if _, err := os.Stat(legacy); err == nil {
		t.Error("legacy file left behind")
	}
	reloaded, _ := NewAccountStore(dir)
	if got, _ := reloaded.Get("acc_mv"); got == nil || got.Nickname != "n" {
		t.Errorf("reloaded = %+v", got)
	}
}

func TestTokenStoreNullFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tokens.json"), []byte("null"), 0o600)
	ts, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(); err != nil { // used to panic on a nil map
		t.Fatal(err)
	}
}
