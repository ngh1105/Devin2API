package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Token is a single OAuth bearer credential persisted to disk.
type Token struct {
	ID        string     `json:"id"`
	Token     string     `json:"token"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used"` // null = never touched
}

// TokenStore persists Tokens as an index file (<dir>/tokens.json).
type TokenStore struct {
	mu      sync.RWMutex
	dir     string
	tokens  map[string]Token // id -> Token
	idxPath string
}

// NewTokenStore creates (if needed) and returns a new TokenStore backed by dir.
func NewTokenStore(dir string) (*TokenStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("token store mkdir: %w", err)
	}
	t := &TokenStore{
		dir:     dir,
		tokens:  make(map[string]Token),
		idxPath: filepath.Join(dir, "tokens.json"),
	}
	if err := t.loadTokens(); err != nil {
		return nil, fmt.Errorf("token store load: %w", err)
	}
	return t, nil
}

// Create adds a fresh token and returns its full record.
func (s *TokenStore) Create() (Token, error) {
	id := genHex("t_", 8)      // "t_" + 16 hex chars
	val := genHex("d2sk_", 16) // "d2sk_" + 32 hex chars
	now := time.Now()
	tok := Token{ID: id, Token: val, CreatedAt: now, LastUsed: nil}
	s.mu.Lock()
	s.tokens[id] = tok
	if err := s.saveTokens(); err != nil {
		delete(s.tokens, id)
		s.mu.Unlock()
		return tok, fmt.Errorf("save after create: %w", err)
	}
	s.mu.Unlock()
	return tok, nil
}

// List returns all tokens in the store.
func (s *TokenStore) List() ([]Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, t)
	}
	return out, nil
}

// Delete removes a token from the store and its persistence file.
func (s *TokenStore) Delete(id string) error {
	s.mu.Lock()
	delete(s.tokens, id)
	if err := s.saveTokens(); err != nil {
		// Rollback: re-add since disk still holds it.
		s.tokens[id] = Token{ID: id} // placeholder to restore consistency
		s.mu.Unlock()
		return fmt.Errorf("save after delete: %w", err)
	}
	s.mu.Unlock()
	return nil
}

// Verify checks whether tokenVal matches any stored token's plaintext value.
func (s *TokenStore) Verify(tokenVal string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tokens {
		if t.Token == tokenVal {
			return true, nil
		}
	}
	return false, nil
}

// Touch updates the LastUsed timestamp for the token with id (best-effort).
func (s *TokenStore) Touch(id string) {
	s.mu.Lock()
	if t, ok := s.tokens[id]; ok {
		now := time.Now()
		t.LastUsed = &now
		s.tokens[id] = t
		s.mu.Unlock()
		s.saveTokens() // best-effort — ignore error
		return
	}
	s.mu.Unlock()
}

func (s *TokenStore) saveTokens() error {
	data, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	tmp := s.idxPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.idxPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func (s *TokenStore) loadTokens() error {
	data, err := os.ReadFile(s.idxPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.tokens)
}
