package store

import (
	"crypto/subtle"
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
// Returns os.ErrNotExist when id is unknown.
func (s *TokenStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.tokens[id]
	if !ok {
		return os.ErrNotExist
	}
	delete(s.tokens, id)
	if err := s.saveTokens(); err != nil {
		// Rollback: disk still holds the token, so memory must too.
		s.tokens[id] = old
		return fmt.Errorf("save after delete: %w", err)
	}
	return nil
}

// Verify checks whether tokenVal matches any stored token's plaintext value.
func (s *TokenStore) Verify(tokenVal string) (bool, error) {
	_, ok := s.Match(tokenVal)
	return ok, nil
}

// Match returns the id of the token whose value equals tokenVal.
// Empty values never match; comparison is constant-time per token.
func (s *TokenStore) Match(tokenVal string) (string, bool) {
	if tokenVal == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, t := range s.tokens {
		if t.Token != "" && subtle.ConstantTimeCompare([]byte(t.Token), []byte(tokenVal)) == 1 {
			return id, true
		}
	}
	return "", false
}

// touchPersistEvery limits how often Touch rewrites tokens.json, since it
// runs on every authenticated request.
const touchPersistEvery = time.Minute

// Touch updates the LastUsed timestamp for the token with id (best-effort).
func (s *TokenStore) Touch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return
	}
	now := time.Now()
	persist := t.LastUsed == nil || now.Sub(*t.LastUsed) >= touchPersistEvery
	t.LastUsed = &now
	s.tokens[id] = t
	if persist {
		s.saveTokens() // best-effort — ignore error
	}
}

// saveTokens writes the index to disk. Caller must hold s.mu.
func (s *TokenStore) saveTokens() error {
	data, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	tmp := s.idxPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
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

// MaskSecret hides all but the last 4 characters of a secret value,
// keeping a known prefix such as "d2sk_" visible. Empty stays empty.
func MaskSecret(v string) string {
	if v == "" {
		return ""
	}
	prefix := ""
	if len(v) > 5 && v[:5] == "d2sk_" {
		prefix = "d2sk_"
	}
	if len(v) <= len(prefix)+8 {
		return prefix + "…"
	}
	return prefix + "…" + v[len(v)-4:]
}
