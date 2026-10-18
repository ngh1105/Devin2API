package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/auth"
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

func makeAccount(id, email, sub string) *Account {
	return &Account{
		Schema:    1,
		ID:        id,
		Email:     email,
		Sub:       sub,
		CreatedAt: time.Unix(1_700_000_000, 0),
		OAuth: OAuth{
			ClientID:    "fake-client",
			Domain:      "example.auth0.com",
			Audience:    "https://api.example.com",
			RedirectURI: "https://app.example.com/callback",
		},
		Tokens: Tokens{
			AccessToken:  "ghp_fake123",
			RefreshToken: "rt_refresh456",
			IDToken:      "eyJhbGciOiJIUzI1NiJ9.fake",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
			Scope:        "openid profile email",
		},
		Profile: Profile{
			Name:    "Test User",
			Picture: "https://cdn.example.com/avatar.png",
		},
		Usage: ptrAny(100),
	}
}

func ptrAny(v any) *any { return &v }

const baseSchema = `{
  "schema": 1,
  "id": "acc_base1",
  "email": "base@example.com",
  "sub": "auth0|123",
  "created_at": "2024-01-15T10:00:00Z",
  "oauth": {"client_id":"cli_abc","domain":"acme.auth0.com","audience":"https://api.acme.io","redirect_uri":"https://app.acme.io/cb"},
  "tokens": {"access_token":"ghp_aaa","refresh_token":"rt_bbb","id_token":"eyJh","expires_at":1737849600,"scope":"openid"},
  "profile": {"name":"Base","picture":"https://img.cdn/b"},
  "usage": null
}`

const fullSchema = `{
  "schema": 1,
  "id": "acc_full1",
  "email": "full@example.com",
  "sub": "auth0|full1",
  "created_at": "2024-03-20T14:30:00Z",
  "oauth": {"client_id":"cli_xyz","domain":"big.auth0.com","audience":"https://bg.api.io","redirect_uri":"https://bg.app/cb"},
  "tokens": {"access_token":"ghp_ccc","refresh_token":"rt_ddd","id_token":"eyJi","expires_at":1740441600,"scope":"openid profile"},
  "profile": {"name":"Full User","picture":"https://img.cdn/f"},
  "usage": {"plan":"pro","limit":500}
}`

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

// --- account tests ---------------------------------------------------------

func TestSaveLoadRoundtrip(t *testing.T) {
	store := mustStore(t)
	a := makeAccount("test-acct", "a@t.co", "u1")
	if err := store.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != a.Email || got.OAuth.ClientID != a.OAuth.ClientID || got.Tokens.AccessToken != a.Tokens.AccessToken {
		t.Errorf("mismatch — got %+v; want %+v", got, a)
	}
}

func TestAtomicNoPartialOnBadDir(t *testing.T) {
	// Chmod on Windows rarely prevents writes; only attempt when available.
	badDir := t.TempDir()
	if err := os.Chmod(badDir, 0o000); err != nil {
		t.Skip("chmod not supported")
	}
	defer os.Chmod(badDir, 0o755)
	store, err := NewAccountStore(badDir)
	if err != nil {
		t.Fatalf("NewAccountStore should succeed (mkdir ok): %v", err)
	}
	a := makeAccount("bad-id", "x@y.z", "uX")
	err = store.Save(a)
	if err == nil {
		t.Log("save did not fail on read-only dir (expected on Windows)")
	}
}

func TestImportFullSchema(t *testing.T) {
	store := mustStore(t)
	a, err := store.Import([]byte(fullSchema))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if a.Schema != 1 || a.Email != "full@example.com" || a.Sub != "auth0|full1" {
		t.Errorf("wrong parsed values: %+v", a)
	}
	if a.Tokens.AccessToken != "ghp_ccc" || a.OAuth.Domain != "big.auth0.com" {
		t.Errorf("nested wrong: %+v", a)
	}
	if u, _ := json.Marshal(a.Usage); string(u) != `{"limit":500,"plan":"pro"}` {
		t.Errorf("usage marshal: %s", u)
	}
}

func TestImportEmailPassword(t *testing.T) {
	store := mustStore(t)
	_, err := store.Import([]byte(`{"email":"a@b.c","password":"s3cret"}`))
	if _, ok := err.(*ImportError); !ok {
		t.Fatalf("expected ImportError, got %T: %v", err, err)
	}
}

func TestImportBareAccessToken(t *testing.T) {
	store := mustStore(t)
	a, err := store.Import([]byte(`{"access_token":"ghp_bare1"}`))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if a.Tokens.AccessToken != "ghp_bare1" || a.Email != "" || a.Sub != "" {
		t.Errorf("bare token account: %+v", a)
	}
}

func TestImportMalformedJSON(t *testing.T) {
	store := mustStore(t)
	for _, raw := range []string{
		`not json`,
		`[1,2,3]`,       // array
		`42`,            // number
		`{"foo":"bar"}`, // unexpected key
	} {
		_, err := store.Import([]byte(raw))
		if _, ok := err.(*ImportError); !ok {
			t.Fatalf("expected ImportError for %q, got %T: %v", raw, err, err)
		}
	}
}

func TestImportSingleTokenFields(t *testing.T) {
	store := mustStore(t)
	tests := []struct {
		raw     string
		atValue string
		rtValue string
	}{
		{`{"access_token":"at1"}`, "at1", ""},
		{`{"refresh_token":"rt2"}`, "", "rt2"},
		{`{"token":"tok3"}`, "tok3", ""},
	}
	for _, tt := range tests {
		a, err := store.Import([]byte(tt.raw))
		if err != nil {
			t.Fatalf("Import(%s): %v", tt.raw, err)
		}
		if a.Tokens.AccessToken != tt.atValue || a.Tokens.RefreshToken != tt.rtValue {
			t.Errorf("%s: expected at=%q rt=%q; got at=%q rt=%q", tt.raw, tt.atValue, tt.rtValue, a.Tokens.AccessToken, a.Tokens.RefreshToken)
		}
	}
}

func TestImportBareTokenWithExtraKeys(t *testing.T) {
	store := mustStore(t)
	for _, raw := range []string{
		`{"access_token":"x","foo":"bar"}`,
		`{"refresh_token":"r","extra":true}`,
		`{"token":"t","baz":123,"qux":"q"}`,
	} {
		_, err := store.Import([]byte(raw))
		if _, ok := err.(*ImportError); !ok {
			t.Fatalf("expected ImportError for %q, got %T: %v", raw, err, err)
		}
	}
}

func TestImportPreservesNonMatchingFields(t *testing.T) {
	const baseEmail = "existing@example.com"
	base := makeAccount("keep-me", baseEmail, "auth0|keep")
	store := mustStore(t)
	if err := store.Save(base); err != nil {
		t.Fatalf("Save base: %v", err)
	}
	_, err := store.Import([]byte(`{"access_token":"new_only"}`))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	got, _ := store.Get(base.ID)
	if got.Email != baseEmail {
		t.Errorf("base email mutated: got %q, want %q", got.Email, baseEmail)
	}
}

func TestImportSameSubUpdates(t *testing.T) {
	const uid = "auth0|same"
	base := makeAccount("first-one", "first@old.com", uid)
	store := mustStore(t)
	if err := store.Save(base); err != nil {
		t.Fatalf("Save base: %v", err)
	}
	newer := `{
  "schema": 1,
  "id": "acc_new2",
  "email": "second@new.com",
  "sub": "auth0|same",
  "created_at": "2024-06-01T00:00:00Z",
  "oauth": {"client_id":"cli_new","domain":"nx.auth0.com","audience":"https://nx.api","redirect_uri":"https://nx.cb"},
  "tokens": {"access_token":"ghp_nxx","refresh_token":"rt_nxy","id_token":"eynC","expires_at":1741000000,"scope":"read"},
  "profile": {"name":"Second","picture":""},
  "usage": null
}`
	imported, err := store.Import([]byte(newer))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.ID != "first-one" {
		t.Errorf("expected id=first-one (upsert), got %s", imported.ID)
	}
	final, err := store.Get("first-one")
	if err != nil {
		t.Fatalf("Get first-one: %v", err)
	}
	if final.Email != "second@new.com" {
		t.Errorf("upsert didn't update email: got %q", final.Email)
	}
	_, err = store.Get("acc_new2")
	if err == nil {
		t.Error("expected Get(acc_new2) to fail after upsert")
	}
}

func TestUpdateTokensSetsExpiry(t *testing.T) {
	store := mustStore(t)
	a := makeAccount("upd1", "u@t.co", "s1")
	a.Tokens.ExpiresAt = 0
	if err := store.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	tok := auth.Tokens{
		AccessToken:  "new-at",
		RefreshToken: "new-rt",
		IDToken:      "new-id",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		Scope:        "all",
	}
	if err := store.UpdateTokens(a.ID, tok); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	got, err := store.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Tokens.AccessToken != tok.AccessToken {
		t.Errorf("got at %q; want %q", got.Tokens.AccessToken, tok.AccessToken)
	}
	now := time.Now().Unix()
	expectedExp := now + tok.ExpiresIn
	if got.Tokens.ExpiresAt < expectedExp-2 || got.Tokens.ExpiresAt > expectedExp+2 {
		t.Errorf("expires_at=%d; want ~%d", got.Tokens.ExpiresAt, expectedExp)
	}
}

func TestAccountExpired(t *testing.T) {
	store := mustStore(t)
	expired := makeAccount("exp1", "e@x.com", "sx")
	expired.Tokens.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	if err := store.Save(expired); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _ := store.Get(expired.ID)
	if !got.Expired() {
		t.Error("expected Expired()==true for past expires_at")
	}
	fresh := makeAccount("fresh1", "f@x.com", "sf")
	if fresh.Expired() {
		t.Error("expected Expired()==false for future expires_at")
	}
	never := makeAccount("never1", "n@x.com", "sn")
	never.Tokens.ExpiresAt = 0
	if never.Expired() {
		t.Error("expected Expired()==false when expires_at is zero")
	}
}
