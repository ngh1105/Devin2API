package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Token is an API key for /v1. Only a SHA-256 hash of the key is stored:
// the plaintext value is set in Token solely on the record Create returns.
type Token struct {
	ID        string     `json:"id"`
	Name      string     `json:"name,omitempty"`
	Token     string     `json:"token,omitempty"` // plaintext, never persisted
	Hash      string     `json:"hash,omitempty"`  // hex SHA-256 of the key
	Hint      string     `json:"hint,omitempty"`  // masked key for display
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used"` // null = never touched
	Requests  int64      `json:"requests"`  // requests counted against Quota
	Disabled  bool       `json:"disabled,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Models    []string   `json:"models,omitempty"` // allowed models; empty = any
	Quota     int64      `json:"quota,omitempty"`  // max requests; 0 = unlimited
}

// TokenOptions are the editable settings of an API key.
type TokenOptions struct {
	Name      string     `json:"name"`
	Disabled  bool       `json:"disabled"`
	ExpiresAt *time.Time `json:"expires_at"`
	Models    []string   `json:"models"`
	Quota     int64      `json:"quota"`
}

// Limits on TokenOptions.
const (
	maxTokenName   = 64
	maxTokenModels = 64
)

// Reasons Use refuses a key.
var (
	ErrKeyDisabled = errors.New("API key is disabled")
	ErrKeyExpired  = errors.New("API key has expired")
	ErrKeyQuota    = errors.New("API key request quota is used up")
)

// tokenSaveDelay batches the counter updates Use makes on every request
// into one write.
const tokenSaveDelay = 5 * time.Second

// TokenStore persists Tokens as an index file (<dir>/tokens.json).
type TokenStore struct {
	mu      sync.RWMutex
	dir     string
	tokens  map[string]Token // id -> Token
	idxPath string
	pending *time.Timer // delayed save, nil when none is scheduled
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

// hashKey returns the stored form of a key value.
func hashKey(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// normalize validates options and cleans up the model list.
func (o *TokenOptions) normalize() error {
	o.Name = strings.TrimSpace(o.Name)
	if len(o.Name) > maxTokenName {
		return fmt.Errorf("name is longer than %d characters", maxTokenName)
	}
	if o.Quota < 0 {
		return errors.New("quota cannot be negative")
	}
	seen := map[string]bool{}
	models := []string{}
	for _, m := range o.Models {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			models = append(models, m)
		}
	}
	if len(models) > maxTokenModels {
		return fmt.Errorf("at most %d models can be allowed", maxTokenModels)
	}
	o.Models = models
	if len(o.Models) == 0 {
		o.Models = nil
	}
	return nil
}

// Create adds a fresh key with opts and returns its record, the only one
// that carries the plaintext value.
func (s *TokenStore) Create(opts TokenOptions) (Token, error) {
	if err := opts.normalize(); err != nil {
		return Token{}, err
	}
	val := genHex("d2sk_", 16) // "d2sk_" + 32 hex chars
	tok := Token{
		ID: genHex("t_", 8), Name: opts.Name, Hash: hashKey(val), Hint: MaskSecret(val),
		CreatedAt: time.Now(), Disabled: opts.Disabled, ExpiresAt: opts.ExpiresAt,
		Models: opts.Models, Quota: opts.Quota,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[tok.ID] = tok
	if err := s.saveTokens(); err != nil {
		delete(s.tokens, tok.ID)
		return Token{}, fmt.Errorf("save after create: %w", err)
	}
	tok.Token = val
	return tok, nil
}

// Update replaces the settings of key id and, with resetRequests, zeroes
// its request counter. Returns os.ErrNotExist when id is unknown.
func (s *TokenStore) Update(id string, opts TokenOptions, resetRequests bool) (Token, error) {
	if err := opts.normalize(); err != nil {
		return Token{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.tokens[id]
	if !ok {
		return Token{}, os.ErrNotExist
	}
	t := old
	t.Name, t.Disabled, t.ExpiresAt, t.Models, t.Quota = opts.Name, opts.Disabled, opts.ExpiresAt, opts.Models, opts.Quota
	if resetRequests {
		t.Requests = 0
	}
	s.tokens[id] = t
	if err := s.saveTokens(); err != nil {
		s.tokens[id] = old
		return Token{}, fmt.Errorf("save after update: %w", err)
	}
	return t, nil
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

// Verify checks whether tokenVal is a stored key.
func (s *TokenStore) Verify(tokenVal string) (bool, error) {
	_, ok := s.Match(tokenVal)
	return ok, nil
}

// Match returns the key whose value is tokenVal. Empty values never match;
// hashes are compared in constant time.
func (s *TokenStore) Match(tokenVal string) (Token, bool) {
	if tokenVal == "" {
		return Token{}, false
	}
	h := []byte(hashKey(tokenVal))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tokens {
		if t.Hash != "" && subtle.ConstantTimeCompare([]byte(t.Hash), h) == 1 {
			return t, true
		}
	}
	return Token{}, false
}

// Use admits one request made with key id: it fails when the key is
// disabled, expired or out of quota, and otherwise updates LastUsed and,
// when counted, the request counter. Returns os.ErrNotExist when id is
// unknown.
func (s *TokenStore) Use(id string, counted bool) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return Token{}, os.ErrNotExist
	}
	now := time.Now()
	switch {
	case t.Disabled:
		return t, ErrKeyDisabled
	case t.ExpiresAt != nil && !now.Before(*t.ExpiresAt):
		return t, ErrKeyExpired
	case counted && t.Quota > 0 && t.Requests >= t.Quota:
		return t, ErrKeyQuota
	}
	t.LastUsed = &now
	if counted {
		t.Requests++
	}
	s.tokens[id] = t
	s.scheduleSave()
	return t, nil
}

// Refund takes back a request Use counted, for a request that was then
// refused before reaching upstream.
// Empty reports whether no API keys exist (keyless mode).
func (s *TokenStore) Empty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tokens) == 0
}

func (s *TokenStore) Refund(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok || t.Requests == 0 {
		return
	}
	t.Requests--
	s.tokens[id] = t
	s.scheduleSave()
}

// scheduleSave writes the index after tokenSaveDelay unless a write is
// already scheduled. Caller must hold s.mu.
func (s *TokenStore) scheduleSave() {
	if s.pending != nil {
		return
	}
	s.pending = time.AfterFunc(tokenSaveDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.pending = nil
		if err := s.saveTokens(); err != nil {
			slog.Warn("save api keys", "error", err)
		}
	})
}

// Flush writes a scheduled save now.
func (s *TokenStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return nil
	}
	s.pending.Stop()
	s.pending = nil
	return s.saveTokens()
}

// saveTokens writes the index to disk. Caller must hold s.mu.
func (s *TokenStore) saveTokens() error {
	data, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	return WriteFileAtomic(s.idxPath, data, 0600)
}

// loadTokens reads the index. Keys saved in plaintext by older versions are
// replaced by their hash, and the file is rewritten without them.
func (s *TokenStore) loadTokens() error {
	data, err := os.ReadFile(s.idxPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal(data, &s.tokens); err != nil {
		return err
	}
	if s.tokens == nil { // file held "null"
		s.tokens = make(map[string]Token)
	}
	migrated := 0
	for id, t := range s.tokens {
		if t.Token == "" {
			continue
		}
		t.Hash, t.Hint, t.Token = hashKey(t.Token), MaskSecret(t.Token), ""
		s.tokens[id] = t
		migrated++
	}
	if migrated > 0 {
		if err := s.saveTokens(); err != nil {
			return fmt.Errorf("hash stored keys: %w", err)
		}
		slog.Info("stored API keys hashed", "keys", migrated)
	}
	return nil
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
