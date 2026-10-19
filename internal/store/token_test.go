package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// NewTokenStoreFromPath loads a TokenStore from an existing index file.
func NewTokenStoreFromPath(idxPath string) (*TokenStore, error) {
	return NewTokenStore(filepath.Dir(idxPath))
}

// TestCreateVerify confirms Create produces a token that Verify recognises.
func TestCreateVerify(t *testing.T) {
	ts := mustTokenStore(t)
	tok, err := ts.Create(TokenOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tok.ID == "" || tok.Token == "" {
		t.Fatalf("empty fields: %+v", tok)
	}
	match, err := ts.Verify(tok.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !match {
		t.Fatal("expected Verify to return true for the freshly created token")
	}
}

// TestDeleteRevokes ensures that after Delete the token is no longer valid.
func TestDeleteRevokes(t *testing.T) {
	ts := mustTokenStore(t)
	tok, _ := ts.Create(TokenOptions{})
	ts.Delete(tok.ID)
	if match, _ := ts.Verify(tok.Token); match {
		t.Error("expected Verify false after Delete")
	}
}

// TestDeleteRollbackKeepsToken ensures a failed save restores the real token,
// not an empty placeholder that would match an empty Bearer value.
func TestDeleteRollbackKeepsToken(t *testing.T) {
	ts := mustTokenStore(t)
	tok, _ := ts.Create(TokenOptions{})
	// Point the index at a directory that doesn't exist so the save fails.
	ts.idxPath = filepath.Join(t.TempDir(), "sub", "missing", "tokens.json")
	if err := ts.Delete(tok.ID); err == nil {
		t.Fatal("expected save error")
	}
	if ok, _ := ts.Verify(tok.Token); !ok {
		t.Error("token lost after rollback")
	}
	if ok, _ := ts.Verify(""); ok {
		t.Error("empty token value must never verify")
	}
}

func TestDeleteMissing(t *testing.T) {
	ts := mustTokenStore(t)
	if err := ts.Delete("t_nope"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("got %v, want ErrNotExist", err)
	}
}

// TestListPersists verifies List returns all created tokens and survives reload.
func TestListPersists(t *testing.T) {
	dir := t.TempDir()
	ts1, _ := NewTokenStore(dir)
	t1, _ := ts1.Create(TokenOptions{})
	t2, _ := ts1.Create(TokenOptions{})
	list, err := ts1.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(list))
	}
	found := map[string]bool{t1.ID: true, t2.ID: true}
	for _, entry := range list {
		if !found[entry.ID] {
			t.Errorf("unexpected token in list: %s", entry.ID)
		}
		delete(found, entry.ID)
	}
	// Reload from disk and verify persistence.
	ts2, _ := NewTokenStore(dir)
	list2, _ := ts2.List()
	if len(list2) != 2 {
		t.Fatalf("after reload: expected 2, got %d", len(list2))
	}
}

// TestUse counts requests, refreshes LastUsed and enforces the key's
// limits; the counter survives a reload.
func TestUse(t *testing.T) {
	dir := t.TempDir()
	ts, _ := NewTokenStore(dir)
	tok, _ := ts.Create(TokenOptions{Name: "ci", Quota: 2})
	if _, err := ts.Use(tok.ID, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := ts.Use(tok.ID, true); err != nil {
			t.Fatalf("use %d: %v", i, err)
		}
	}
	if _, err := ts.Use(tok.ID, true); !errors.Is(err, ErrKeyQuota) {
		t.Errorf("over quota: %v", err)
	}
	if _, err := ts.Use(tok.ID, false); err != nil {
		t.Errorf("uncounted use over quota: %v", err)
	}
	got, _ := ts.Update(tok.ID, TokenOptions{Name: "ci", Quota: 2}, true)
	if got.Requests != 0 || got.LastUsed == nil {
		t.Errorf("after reset: %+v", got)
	}
	ts.Use(tok.ID, true)
	if err := ts.Flush(); err != nil {
		t.Fatal(err)
	}
	ts2, _ := NewTokenStore(dir)
	if m, ok := ts2.Match(tok.Token); !ok || m.Requests != 1 || m.Name != "ci" {
		t.Errorf("reloaded: %+v %v", m, ok)
	}

	past := time.Now().Add(-time.Minute)
	ts.Update(tok.ID, TokenOptions{ExpiresAt: &past}, false)
	if _, err := ts.Use(tok.ID, false); !errors.Is(err, ErrKeyExpired) {
		t.Errorf("expired: %v", err)
	}
	ts.Update(tok.ID, TokenOptions{Disabled: true}, false)
	if _, err := ts.Use(tok.ID, false); !errors.Is(err, ErrKeyDisabled) {
		t.Errorf("disabled: %v", err)
	}
	if _, err := ts.Use("t_nope", true); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unknown: %v", err)
	}
}

// TestKeysStoredHashed: the key value never reaches disk, and keys saved in
// plaintext by older versions are hashed on load and keep working.
func TestKeysStoredHashed(t *testing.T) {
	dir := t.TempDir()
	ts, _ := NewTokenStore(dir)
	tok, _ := ts.Create(TokenOptions{})
	b, _ := os.ReadFile(filepath.Join(dir, "tokens.json"))
	if strings.Contains(string(b), tok.Token) {
		t.Fatal("plaintext key written to disk")
	}

	legacy := `{"t_old":{"id":"t_old","token":"d2sk_0123456789abcdef0123456789abcdef","created_at":"2026-01-01T00:00:00Z","last_used":null}}`
	old := t.TempDir()
	os.WriteFile(filepath.Join(old, "tokens.json"), []byte(legacy), 0o600)
	ts2, err := NewTokenStore(old)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := ts2.Match("d2sk_0123456789abcdef0123456789abcdef"); !ok || m.ID != "t_old" || m.Hint != "d2sk_…cdef" {
		t.Errorf("legacy key: %+v %v", m, ok)
	}
	b, _ = os.ReadFile(filepath.Join(old, "tokens.json"))
	if strings.Contains(string(b), "0123456789abcdef0123") {
		t.Error("legacy plaintext kept on disk")
	}
}

func TestTokenOptionsValidation(t *testing.T) {
	ts := mustTokenStore(t)
	if _, err := ts.Create(TokenOptions{Quota: -1}); err == nil {
		t.Error("negative quota accepted")
	}
	if _, err := ts.Create(TokenOptions{Name: strings.Repeat("x", 65)}); err == nil {
		t.Error("long name accepted")
	}
	tok, err := ts.Create(TokenOptions{Name: "  a  ", Models: []string{" swe-2 ", "", "swe-2"}})
	if err != nil || tok.Name != "a" || len(tok.Models) != 1 || tok.Models[0] != "swe-2" {
		t.Errorf("normalized: %+v %v", tok, err)
	}
}
