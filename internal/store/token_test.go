package store

import (
	"path/filepath"
	"testing"
)

// NewTokenStoreFromPath loads a TokenStore from an existing index file.
func NewTokenStoreFromPath(idxPath string) (*TokenStore, error) {
	return NewTokenStore(filepath.Dir(idxPath))
}

// TestCreateVerify confirms Create produces a token that Verify recognises.
func TestCreateVerify(t *testing.T) {
	ts := mustTokenStore(t)
	tok, err := ts.Create()
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
	tok, _ := ts.Create()
	ts.Delete(tok.ID)
	if match, _ := ts.Verify(tok.Token); match {
		t.Error("expected Verify false after Delete")
	}
}

// TestListPersists verifies List returns all created tokens and survives reload.
func TestListPersists(t *testing.T) {
	dir := t.TempDir()
	ts1, _ := NewTokenStore(dir)
	t1, _ := ts1.Create()
	t2, _ := ts1.Create()
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

// TestTouch best-effort updates LastUsed and persists the change.
func TestTouch(t *testing.T) {
	dir := t.TempDir()
	ts1, _ := NewTokenStore(dir)
	tok, _ := ts1.Create()

	// Touch should set LastUsed.
	ts1.Touch(tok.ID)
	list, _ := ts1.List()
	for _, entry := range list {
		if entry.ID == tok.ID && entry.LastUsed == nil {
			t.Fatal("LastUsed after touch: LastUsed should be non-nil")
		}
	}
	// Reload and confirm LastUsed survived.
	idxPath := filepath.Join(dir, "tokens.json")
	ts3, _ := NewTokenStoreFromPath(idxPath)
	list3, _ := ts3.List()
	for _, entry := range list3 {
		if entry.ID == tok.ID && entry.LastUsed == nil {
			t.Fatal("persisted: LastUsed missing after reload")
		}
	}
}
