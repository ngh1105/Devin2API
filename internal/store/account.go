// Package store provides persistent stores for accounts and tokens.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/auth"
)

// OAuth holds a single provider's connection credentials.
type OAuth struct {
	ClientID    string `json:"client_id"`
	Domain      string `json:"domain"`
	Audience    string `json:"audience"`
	RedirectURI string `json:"redirect_uri"`
}

// Tokens holds bearer credentials persisted by schema version.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix seconds
	Scope        string `json:"scope"`
}

// Profile is the userinfo payload — mirrors auth.Profile with JSON tag reuse.
type Profile struct {
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// Account matches schema v1: {"schema":1,"id":"acc_…","email":"…","sub":"…",
// "created_at":"…","oauth":{...},"tokens":{...},"profile":{...},"usage":null}.
type Account struct {
	Schema    int64     `json:"schema"`
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Sub       string    `json:"sub"`
	CreatedAt time.Time `json:"created_at"`
	OAuth     OAuth     `json:"oauth"`
	Tokens    Tokens    `json:"tokens"`
	Profile   Profile   `json:"profile"`
	Usage     *any      `json:"usage"`
}

// Expired reports whether this account's tokens have passed their expiration.
func (a *Account) Expired() bool {
	if a.Tokens.ExpiresAt == 0 {
		return false
	}
	return time.Now().Unix() > a.Tokens.ExpiresAt
}

// ImportError signals that raw input could not be normalised into an Account.
type ImportError struct {
	Reason string
}

func (e *ImportError) Error() string { return "import: " + e.Reason }

// AccountStore persists Accounts as individual <id>.json files under dir.
type AccountStore struct {
	mu       sync.RWMutex
	dir      string
	accounts map[string]*Account
}

// NewAccountStore creates (if needed) and returns a new AccountStore backed by dir.
func NewAccountStore(dir string) (*AccountStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("account store mkdir: %w", err)
	}
	s := &AccountStore{dir: dir, accounts: make(map[string]*Account)}
	if accs, err := loadAllAccounts(dir); err != nil {
		return nil, fmt.Errorf("account store loadAll: %w", err)
	} else {
		s.accounts = accs
	}
	return s, nil
}

// List returns all accounts sorted by created_at ascending.
func (s *AccountStore) List() ([]*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		cp := *a
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Get returns a copy of the account with id, or os.ErrNotExist if absent.
func (s *AccountStore) Get(id string) (*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	cp := *a
	return &cp, nil
}

// Save atomically writes account to <id>.json in the store directory.
func (s *AccountStore) Save(a *Account) error {
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal account %s: %w", a.ID, err)
	}
	path := filepath.Join(s.dir, a.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, path, err)
	}
	s.mu.Lock()
	s.accounts[a.ID] = a
	s.mu.Unlock()
	return nil
}

// Delete removes the file for id from disk and drops it from memory.
func (s *AccountStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accounts, id)
	return os.Remove(filepath.Join(s.dir, id+".json"))
}

// Import normalises raw JSON into an Account and saves it.
// Handles three shapes: full schema, bare email+password blob, bare token blobs.
func (s *AccountStore) Import(raw json.RawMessage) (*Account, error) {
	parsed, err := parseJSONRaw(raw)
	if err != nil {
		return nil, &ImportError{"malformed JSON"}
	}
	keys := keysOf(parsed)
	sort.Strings(keys)

	switch {
	case hasBareEmailPass(parsed):
		return nil, &ImportError{"email/password found; run the login flow"}
	default:
		return importSchemaOrTokens(s, parsed, keys)
	}
}

// tokenBlobKeys are the only keys allowed in a bare token import blob.
var tokenBlobKeys = map[string]bool{
	"access_token":  true,
	"refresh_token": true,
	"token":         true,
}

func isSubset(m map[string]json.RawMessage, allow map[string]bool) bool {
	for k := range m {
		if !allow[k] {
			return false
		}
	}
	return true
}

// UpdateTokens sets the token fields and computes expires_at from tok.ExpiresIn.
func (s *AccountStore) UpdateTokens(id string, tok auth.Tokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return os.ErrNotExist
	}
	a.Tokens.AccessToken = tok.AccessToken
	a.Tokens.RefreshToken = tok.RefreshToken
	a.Tokens.IDToken = tok.IDToken
	a.Tokens.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	a.Tokens.Scope = tok.Scope
	return nil
}

// AccountFromLogin builds an Account from auth login tokens and user info.
// Fills the oauth block from auth constants, generates id + created_at,
// sets expires_at using tok.ExpiresIn like UpdateTokens does.
func AccountFromLogin(email string, tok auth.Tokens, p auth.Profile) *Account {
	return &Account{
		Schema:    1,
		ID:        genHex("acc_", 8),
		Email:     email,
		Sub:       p.Sub,
		CreatedAt: time.Now(),
		OAuth: OAuth{
			ClientID:    auth.ClientID,
			Domain:      auth.AuthDomain,
			Audience:    auth.Audience,
			RedirectURI: auth.RedirectURI,
		},
		Tokens: Tokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			IDToken:      tok.IDToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			Scope:        tok.Scope,
		},
		Profile: Profile{
			Name:    p.Name,
			Picture: p.Picture,
		},
		Usage: nil,
	}
}

// --- internal helpers --------------------------------------------------------

func genHex(prefix string, nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand entropy exhaustion: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}

func loadAllAccounts(path string) (map[string]*Account, error) {
	result := make(map[string]*Account)
	files, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.IsDir() || f.Name() == "" || f.Name()[0] == '.' {
			continue
		}
		ext := filepath.Ext(f.Name())
		base := f.Name()[:len(f.Name())-len(ext)]
		if ext != ".json" || base == "tmp" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(path, f.Name()))
		if err != nil {
			continue
		}
		var acc Account
		if err := json.Unmarshal(data, &acc); err != nil {
			continue
		}
		result[acc.ID] = &acc
	}
	return result, nil
}

func parseJSONRaw(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return m, nil
}

func hasBareEmailPass(m map[string]json.RawMessage) bool {
	hasEmail := false
	hasPassword := false
	for k := range m {
		if k == "email" {
			hasEmail = true
		}
		if k == "password" {
			hasPassword = true
		}
	}
	return hasEmail && hasPassword
}

func importFromTokenBlob(m map[string]json.RawMessage, key string) *Account {
	var val string
	if raw, ok := m[key]; ok {
		json.Unmarshal(raw, &val)
	}
	toks := Tokens{AccessToken: val}
	if key == "refresh_token" {
		toks = Tokens{RefreshToken: val}
	}
	return &Account{
		Schema:    1,
		ID:        genHex("acc_", 8),
		Tokens:    toks,
		CreatedAt: time.Now(),
	}
}

func importSchema(m map[string]json.RawMessage) (*Account, error) {
	acct := &Account{Schema: 1, CreatedAt: time.Now()}

	if raw, ok := m["email"]; ok {
		json.Unmarshal(raw, &acct.Email)
	}
	if raw, ok := m["sub"]; ok {
		json.Unmarshal(raw, &acct.Sub)
	}
	if raw, ok := m["id"]; ok {
		json.Unmarshal(raw, &acct.ID)
		if acct.ID == "" {
			acct.ID = genHex("acc_", 8)
		}
	}

	tokData := getNested(m, "tokens")
	if len(tokData) > 0 {
		if v, ok := tokData["access_token"]; ok {
			json.Unmarshal(v, &acct.Tokens.AccessToken)
		}
		if v, ok := tokData["refresh_token"]; ok {
			json.Unmarshal(v, &acct.Tokens.RefreshToken)
		}
		if v, ok := tokData["id_token"]; ok {
			json.Unmarshal(v, &acct.Tokens.IDToken)
		}
		if v, ok := tokData["expires_at"]; ok {
			json.Unmarshal(v, &acct.Tokens.ExpiresAt)
		}
		if v, ok := tokData["scope"]; ok {
			json.Unmarshal(v, &acct.Tokens.Scope)
		}
		if acct.Tokens.AccessToken == "" && acct.Tokens.RefreshToken == "" {
			return nil, &ImportError{"neither access_token nor refresh_token provided"}
		}
	}

	oauthData := getNested(m, "oauth")
	if len(oauthData) > 0 {
		if v, ok := oauthData["client_id"]; ok {
			json.Unmarshal(v, &acct.OAuth.ClientID)
		}
		if v, ok := oauthData["domain"]; ok {
			json.Unmarshal(v, &acct.OAuth.Domain)
		}
		if v, ok := oauthData["audience"]; ok {
			json.Unmarshal(v, &acct.OAuth.Audience)
		}
		if v, ok := oauthData["redirect_uri"]; ok {
			json.Unmarshal(v, &acct.OAuth.RedirectURI)
		}
	}

	profData := getNested(m, "profile")
	if len(profData) > 0 {
		if v, ok := profData["name"]; ok {
			json.Unmarshal(v, &acct.Profile.Name)
		}
		if v, ok := profData["picture"]; ok {
			json.Unmarshal(v, &acct.Profile.Picture)
		}
	}

	rawUsage := m["usage"]
	if len(rawUsage) > 0 && string(rawUsage) != "null" {
		var u any
		if err := json.Unmarshal(rawUsage, &u); err == nil {
			acct.Usage = &u
		}
	}

	if acct.ID == "" {
		acct.ID = genHex("acc_", 8)
	}
	return acct, nil
}

func getNested(m map[string]json.RawMessage, key string) map[string]json.RawMessage {
	if raw, ok := m[key]; ok {
		var nested map[string]json.RawMessage
		json.Unmarshal(raw, &nested)
		return nested
	}
	return nil
}

func findSub(store *AccountStore, sub string) *Account {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, a := range store.accounts {
		if a.Sub == sub {
			return a
		}
	}
	return nil
}

func importSchemaOrTokens(store *AccountStore, m map[string]json.RawMessage, keys []string) (*Account, error) {
	// Full-schema import: dispatch when any known top-level key is present.
	for _, k := range keys {
		switch k {
		case "schema", "id", "email", "sub", "created_at", "oauth", "tokens", "profile", "usage":
			acct, err := importSchema(m)
			if err != nil {
				return nil, err
			}
			return withSubCheck(store, acct), nil
		}
	}

	// Bare token blob: keys must be a subset of {access_token, refresh_token, token}.
	if isSubset(m, tokenBlobKeys) && len(keys) > 0 {
		var tokenKey string
		for k := range m {
			if tokenBlobKeys[k] {
				tokenKey = k
				break
			}
		}
		return withSubCheck(store, importFromTokenBlob(m, tokenKey)), nil
	}

	// Anything else: unexpected keys.
	return nil, &ImportError{"unexpected fields: " + keysToReason(keys)}
}

func withSubCheck(store *AccountStore, newAcct *Account) *Account {
	// Same-sub upsert: if sub is non-empty and an existing account shares it,
	// merge the new data into the old account in-memory so Save overwrites it.
	sub := newAcct.Sub
	if sub != "" {
		old := findSub(store, sub)
		if old != nil {
			mergeInto(old, newAcct)
			return old
		}
	}
	return newAcct
}

func mergeInto(dst, src *Account) {
	dst.Schema = src.Schema
	dst.Email = src.Email
	dst.Sub = src.Sub
	dst.CreatedAt = src.CreatedAt
	dst.OAuth = src.OAuth
	dst.Tokens = src.Tokens
	dst.Profile = src.Profile
	dst.Usage = src.Usage
}

func keysToReason(keys []string) string {
	s := make([]string, len(keys))
	for i, k := range keys {
		s[i] = k
	}
	return fmt.Sprintf("%v", s)
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
