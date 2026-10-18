// Package store provides persistent stores for accounts and tokens.
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ---------- types ----------------------------------------------------------

// Tokens holds v2 bearer credentials — session_token only.
type Tokens struct {
	SessionToken string `json:"session_token"`
}

// Usage tracks per-account quota and plan metadata.
type Usage struct {
	Email                  string    `json:"email"`
	UserName               string    `json:"user_name"`
	UserID                 string    `json:"user_id"`
	OrgID                  string    `json:"org_id"`
	OrgName                string    `json:"org_name"`
	Plan                   string    `json:"plan"`
	DailyRemainingPercent  int64     `json:"daily_remaining_percent"`
	WeeklyRemainingPercent int64     `json:"weekly_remaining_percent"`
	DailyResetAt           time.Time `json:"daily_reset_at"`
	WeeklyResetAt          time.Time `json:"weekly_reset_at"`
	PlanStart              time.Time `json:"plan_start"`
	PlanEnd                time.Time `json:"plan_end"`
	CheckedAt              int64     `json:"checked_at"` // unix seconds
}

// v1Account captures legacy schema fields for deserialization + migration.
type v1Account struct {
	Schema    int64           `json:"schema"`
	ID        string          `json:"id"`
	Email     string          `json:"email"`
	Sub       string          `json:"sub"`
	OAuth     v1OAuth         `json:"oauth"`
	Tokens    v1Tokens        `json:"tokens"`
	Profile   v1Profile       `json:"profile"`
	Usage     *any            `json:"usage"`
	CreatedAt json.RawMessage `json:"created_at"`
}

// accountPlain is a field-identical shadow of Account with NO custom methods.
// Used by UnmarshalJSON so that v2 records round-trip through plain JSON
// without entering the v1 migration path.
type accountPlain struct {
	Schema               int64     `json:"schema"`
	ID                   string    `json:"id"`
	Email                string    `json:"email"`
	UserName             string    `json:"user_name"`
	UserID               string    `json:"user_id"`
	OrgID                string    `json:"org_id"`
	Plan                 string    `json:"plan"`
	Nickname             string    `json:"nickname"`
	Enabled              bool      `json:"enabled"`
	AuthKind             string    `json:"auth_kind"`
	CreatedAt            time.Time `json:"created_at"`
	Tokens               Tokens    `json:"tokens"`
	Usage                *Usage    `json:"usage"`
	UsageErr             string    `json:"usage_error,omitempty"`
	enabledExplicitlySet bool      `json:"-"`
}

// hasV1TokenMarker reports whether raw JSON carries access_token in tokens,
// which signals a v1 record even when schema key is absent.
func hasV1TokenMarker(data []byte) bool {
	return bytes.Contains(data, []byte(`"access_token"`))
}

func extractV1Field(m map[string]json.RawMessage, key string, dst *string) {
	if raw, ok := m[key]; ok {
		json.Unmarshal(raw, dst)
	}
}

func loadV1CreatedAt(raw json.RawMessage) time.Time {
	if len(raw) > 0 {
		var t time.Time
		json.Unmarshal(raw, &t) // RFC3339 or unix float → both parse into time.Time
		return t
	}
	return time.Now()
}

func resolveSessionTokenFromParsed(parsed map[string]json.RawMessage, tok *Tokens) {
	// 1. tokens.session_token (v2 nested object).
	if tokRaw, ok := parsed["tokens"]; ok {
		var nested map[string]json.RawMessage
		json.Unmarshal(tokRaw, &nested)
		if sTok, ok := nested["session_token"]; ok {
			var val string
			json.Unmarshal(sTok, &val)
			tok.SessionToken = FormatSessionToken(val)
			return
		}
	}
	// 2. Top-level session_token or access_token (v1 top-level or bare-token paths).
	for _, key := range []string{"session_token", "access_token"} {
		if raw, ok := parsed[key]; ok {
			var val string
			json.Unmarshal(raw, &val)
			tok.SessionToken = FormatSessionToken(val)
			return
		}
	}
	// 3. Fallthrough: tokens.access_token (legacy v1 shape).
	//    The above nested lookup may not match if keys were trimmed; re-parse.
	if tokRaw, ok := parsed["tokens"]; ok {
		var nested map[string]json.RawMessage
		json.Unmarshal(tokRaw, &nested)
		if at, ok := nested["access_token"]; ok {
			var val string
			json.Unmarshal(at, &val)
			tok.SessionToken = FormatSessionToken(val)
			return
		}
	}
}

// UnmarshalJSON handles v1→v2 migration during load. v2 files are unmarshalled
// via a field-identical shadow type (accountPlain) for a perfect round-trip;
// only v1 records enter the migration path.
func (a *Account) UnmarshalJSON(data []byte) error {
	// Peek at top-level keys to decide path.
	parsed, err := parseJSONRaw(data)
	if err != nil {
		return err
	}

	hasSchemaKey := false
	schemaVer := int64(2) // default to v2 for unknown records
	if raw, ok := parsed["schema"]; ok {
		json.Unmarshal(raw, &schemaVer)
		hasSchemaKey = true
	}

	isV1 := schemaVer < 2 || !hasSchemaKey || hasV1TokenMarker(data)

	if isV1 {
		return a.migrateFromV1(data, parsed)
	}

	// v2 path: use the accountPlain shadow type for a clean round-trip.
	var p accountPlain
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("unmarshal v2 account: %w", err)
	}

	a.Schema = 2
	a.ID = p.ID
	a.Email = p.Email
	a.UserName = p.UserName
	a.UserID = p.UserID
	a.OrgID = p.OrgID
	a.Plan = p.Plan
	a.Nickname = p.Nickname
	a.Enabled = p.Enabled
	a.AuthKind = p.AuthKind
	a.CreatedAt = p.CreatedAt
	a.Tokens = p.Tokens
	a.Usage = p.Usage
	a.UsageErr = p.UsageErr
	return nil
}

// migrateFromV1 migrates a v1 JSON blob into an Account.
func (a *Account) migrateFromV1(data []byte, parsed map[string]json.RawMessage) error {
	a.Schema = 2
	extractV1Field(parsed, "id", &a.ID)
	extractV1Field(parsed, "email", &a.Email)
	a.CreatedAt = loadV1CreatedAt(parsed["created_at"])

	resolveSessionTokenFromParsed(parsed, &a.Tokens)

	// Enabled: defaults true unless "enabled" key is explicitly present.
	a.Enabled = true
	if eRaw, ok := parsed["enabled"]; ok {
		json.Unmarshal(eRaw, &a.Enabled)
	}
	a.AuthKind = "oauth"

	if uRaw, ok := parsed["usage"]; ok && len(uRaw) > 0 && string(uRaw) != "null" {
		var usage Usage
		if err := json.Unmarshal(uRaw, &usage); err == nil {
			a.Usage = &usage
		}
	}

	return nil
}

type v1OAuth struct {
	ClientID    string `json:"client_id"`
	Domain      string `json:"domain"`
	Audience    string `json:"audience"`
	RedirectURI string `json:"redirect_uri"`
}

type v1Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Scope        string `json:"scope"`
}

type v1Profile struct {
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// Account matches schema v2: {"schema":2,"id":"acc_<8hex>",
// "email":"…","user_name":"…","user_id":"…","org_id":"…",
// "plan":"…","nickname":"…","enabled":true/false,
// "auth_kind":"oauth","created_at":"RFC3339",
// "tokens":{"session_token":"…"},"usage":null}.
type Account struct {
	Schema    int64     `json:"schema"`
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	UserName  string    `json:"user_name"`
	UserID    string    `json:"user_id"`
	OrgID     string    `json:"org_id"`
	Plan      string    `json:"plan"`
	Nickname  string    `json:"nickname"`
	Enabled   bool      `json:"enabled"`
	AuthKind  string    `json:"auth_kind"`
	CreatedAt time.Time `json:"created_at"`
	Tokens    Tokens    `json:"tokens"`
	Usage     *Usage    `json:"usage"`
	UsageErr  string    `json:"usage_error,omitempty"`
	// enabledExplicitlySet tracks whether Enabled was explicitly set in the
	// import payload (used by mergeOnlyExplicitFields to preserve existing state).
	enabledExplicitlySet bool `json:"-"`
}

// Expired reports whether this account's plan has ended (server-side expiry).
func (a *Account) Expired() bool {
	if a.Usage == nil {
		return false
	}
	if a.Usage.PlanEnd.Unix() <= 0 {
		return false
	}
	return time.Now().Unix() > a.Usage.PlanEnd.Unix()
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
	if err := os.WriteFile(tmp, data, 0600); err != nil {
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
// Accepts: (a) full v2 schema; (b) CLIProxyAPI shape (api_key/session_token present);
// (c) bare token blobs {"token"|"access_token"|"refresh_token":"..."}.
// Rejects email/password shapes with "run login flow instead".
func (s *AccountStore) Import(raw json.RawMessage) (*Account, error) {
	parsed, err := parseJSONRaw(raw)
	if err != nil {
		return nil, &ImportError{"malformed JSON"}
	}

	switch {
	case hasBareEmailPass(parsed):
		return nil, &ImportError{"email/password found; run the login flow"}
	case isV2Schema(parsed):
		acct, err := importSchemaV2(parsed)
		if err != nil {
			return nil, err
		}
		return upsertByUserOrEmail(s, acct), nil
	case isCLIProxyAPIShape(parsed):
		acct := importCLIProxyAPI(parsed)
		return upsertByUserOrEmail(s, acct), nil
	default:
		// Bare token blob — must have at least one token key.
		// Identity/known keys are tolerated; truly unknown keys cause an error.
		hasTokenKey := false
		for k := range parsed {
			if tokenBlobKeys[k] {
				hasTokenKey = true
				break
			}
		}
		if !hasTokenKey {
			return nil, &ImportError{"unexpected fields: " + keysToReason(keysOf(parsed))}
		}
		// Reject blobs with entirely unrecognized top-level keys.
		bareAcceptableKeys := map[string]bool{
			"access_token": true, "refresh_token": true, "token": true,
			"user_id": true, "email": true, "user_name": true,
			"org_id": true, "api_key": true, "session_token": true,
		}
		for k := range parsed {
			if !bareAcceptableKeys[k] {
				return nil, &ImportError{"unexpected fields: " + keysToReason(keysOf(parsed))}
			}
		}
		acct := importBareToken(parsed)
		return upsertByUserOrEmail(s, acct), nil
	}
}

// AccountFromLogin builds an Account from a Devin CLI login event.
func AccountFromLogin(userName, userID, orgID, sessionToken, email string, u *Usage) *Account {
	return &Account{
		Schema:    2,
		ID:        genHex("acc_", 8),
		Email:     email,
		UserName:  userName,
		UserID:    userID,
		OrgID:     orgID,
		AuthKind:  "oauth",
		Enabled:   true,
		CreatedAt: time.Now(),
		Tokens:    Tokens{SessionToken: FormatSessionToken(sessionToken)},
		Usage:     u,
	}
}

// Patch partially updates an account: nickname and/or enabled.
// Only non-nil fields are applied. Returns ErrNotFound if id is absent.
func (s *AccountStore) Patch(id string, nickname *string, enabled *bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return os.ErrNotExist
	}
	if nickname != nil {
		a.Nickname = *nickname
	}
	if enabled != nil {
		a.Enabled = *enabled
	}
	return nil
}

// ---------- token helpers ---------------------------------------------------

// FormatSessionToken formats a raw token value for persistence in Tokens.SessionToken.
// Values starting with "devin-session-token$" are kept as-is.
// All other non-empty values get the devin-session-token$ prefix.
func FormatSessionToken(value string) string {
	if value == "" {
		return value
	}
	const prefix = "devin-session-token$"
	if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
		return value
	}
	return prefix + value
}

// ---------- import shapes ---------------------------------------------------

var cliProxyDetectKeys = map[string]bool{
	"api_key":       true,
	"session_token": true,
}

var v2SchemaKeys = map[string]bool{
	"schema":      true,
	"id":          true,
	"email":       true,
	"user_name":   true,
	"user_id":     true,
	"org_id":      true,
	"plan":        true,
	"nickname":    true,
	"auth_kind":   true,
	"enabled":     true,
	"created_at":  true,
	"tokens":      true,
	"usage":       true,
	"usage_error": true,
}

var tokenBlobKeys = map[string]bool{
	"access_token":  true,
	"refresh_token": true,
	"token":         true,
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func keysToReason(keys []string) string {
	s := make([]string, len(keys))
	for i, k := range keys {
		s[i] = k
	}
	return fmt.Sprintf("%v", s)
}

func hasAnyKey(m map[string]json.RawMessage, allowed map[string]bool) bool {
	for k := range m {
		if allowed[k] {
			return true
		}
	}
	return false
}

func isSubset(m map[string]json.RawMessage, allow map[string]bool) bool {
	for k := range m {
		if !allow[k] {
			return false
		}
	}
	return true
}

func hasBareEmailPass(m map[string]json.RawMessage) bool {
	hasEmail, hasPassword := false, false
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

func isCLIProxyAPIShape(m map[string]json.RawMessage) bool {
	_, hasAPIKey := m["api_key"]
	_, hasST := m["session_token"]
	return hasAPIKey || hasST
}

func isV2Schema(m map[string]json.RawMessage) bool {
	// A record is considered v2 schema when it explicitly carries a "schema" key
	// or an inline "tokens" object — the definitive markers.
	_, hasSchema := m["schema"]
	hasTokensObj := false
	if raw, ok := m["tokens"]; ok {
		var nested json.RawMessage
		if err := json.Unmarshal(raw, &nested); err == nil && len(nested) > 0 && nested[0] == '{' {
			hasTokensObj = true
		}
	}
	return hasSchema || hasTokensObj
}

func hasKey(data []byte, key string) bool {
	target := []byte(`"` + key + `":`)
	return bytes.Contains(data, target)
}

func parseJSONRaw(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return m, nil
}

func getNested(m map[string]json.RawMessage, key string) map[string]json.RawMessage {
	if raw, ok := m[key]; ok {
		var nested map[string]json.RawMessage
		json.Unmarshal(raw, &nested)
		return nested
	}
	return nil
}

func importBareToken(m map[string]json.RawMessage) *Account {
	var val string
	if v, ok := m["access_token"]; ok {
		json.Unmarshal(v, &val)
	} else if v, ok := m["refresh_token"]; ok {
		json.Unmarshal(v, &val)
	} else if v, ok := m["token"]; ok {
		json.Unmarshal(v, &val)
	}
	acct := &Account{
		Schema:    2,
		ID:        genHex("acc_", 8),
		Tokens:    Tokens{SessionToken: FormatSessionToken(val)},
		CreatedAt: time.Now(),
	}
	// Also accept optional identity fields for upsert matching.
	if raw, ok := m["user_id"]; ok {
		json.Unmarshal(raw, &acct.UserID)
	}
	if raw, ok := m["email"]; ok {
		json.Unmarshal(raw, &acct.Email)
	}
	if raw, ok := m["user_name"]; ok {
		json.Unmarshal(raw, &acct.UserName)
	}
	if raw, ok := m["org_id"]; ok {
		json.Unmarshal(raw, &acct.OrgID)
	}
	return acct
}

func importSchemaV2(m map[string]json.RawMessage) (*Account, error) {
	acct := &Account{Schema: 2, Enabled: true, CreatedAt: time.Now(), enabledExplicitlySet: true}

	setField := func(key string, dst *string) {
		if raw, ok := m[key]; ok {
			json.Unmarshal(raw, dst)
		}
	}

	setField("id", &acct.ID)
	setField("email", &acct.Email)
	setField("user_name", &acct.UserName)
	setField("user_id", &acct.UserID)
	setField("org_id", &acct.OrgID)
	setField("plan", &acct.Plan)
	setField("nickname", &acct.Nickname)
	setField("auth_kind", &acct.AuthKind)
	setField("usage_error", &acct.UsageErr)

	if raw, ok := m["enabled"]; ok {
		json.Unmarshal(raw, &acct.Enabled)
	}
	if raw, ok := m["created_at"]; ok {
		json.Unmarshal(raw, &acct.CreatedAt)
	}

	tokData := getNested(m, "tokens")
	if raw, ok := tokData["session_token"]; ok {
		json.Unmarshal(raw, &acct.Tokens.SessionToken)
	}
	// Top-level session_token overrides tokens.session_token.
	if raw, ok := m["session_token"]; ok {
		json.Unmarshal(raw, &acct.Tokens.SessionToken)
	}

	rawUsage := m["usage"]
	if len(rawUsage) > 0 && string(rawUsage) != "null" {
		var usage Usage
		if err := json.Unmarshal(rawUsage, &usage); err == nil {
			acct.Usage = &usage
		}
	}

	if acct.ID == "" {
		acct.ID = genHex("acc_", 8)
	}
	return acct, nil
}

func importCLIProxyAPI(m map[string]json.RawMessage) *Account {
	acct := &Account{Schema: 2, AuthKind: "oauth", Enabled: true, CreatedAt: time.Now(), enabledExplicitlySet: true}

	if raw, ok := m["api_key"]; ok {
		var val string
		json.Unmarshal(raw, &val)
		acct.Tokens.SessionToken = FormatSessionToken(val)
	}
	if raw, ok := m["session_token"]; ok {
		var val string
		json.Unmarshal(raw, &val)
		acct.Tokens.SessionToken = FormatSessionToken(val)
	}

	setIfPresent := func(key string, dst *string) {
		if raw, ok := m[key]; ok {
			json.Unmarshal(raw, dst)
		}
	}

	setIfPresent("email", &acct.Email)
	setIfPresent("user_name", &acct.UserName)
	setIfPresent("user_id", &acct.UserID)
	setIfPresent("org_id", &acct.OrgID)
	setIfPresent("plan", &acct.Plan)
	setIfPresent("auth_kind", &acct.AuthKind)

	if raw, ok := m["disabled"]; ok {
		var disabled bool
		json.Unmarshal(raw, &disabled)
		if disabled {
			acct.Enabled = false
		}
	}

	acct.ID = genHex("acc_", 8)
	return acct
}

// ---------- upsert / merge --------------------------------------------------

func upsertByUserOrEmail(store *AccountStore, newAcct *Account) *Account {
	if newAcct.UserID != "" {
		if old := findIndex(store, func(a *Account) bool { return a.UserID == newAcct.UserID }); old != nil {
			mergeOnlyExplicitFields(old, newAcct)
			return old
		}
	}
	if newAcct.Email != "" {
		if old := findIndex(store, func(a *Account) bool { return a.Email == newAcct.Email }); old != nil {
			mergeOnlyExplicitFields(old, newAcct)
			return old
		}
	}
	return newAcct
}

func mergeOnlyExplicitFields(dst, src *Account) {
	// Schema always takes source value.
	dst.Schema = src.Schema

	// Token: only copy when the source actually carries one.
	if src.Tokens.SessionToken != "" {
		dst.Tokens.SessionToken = src.Tokens.SessionToken
	}
	// Identity fields: only overwrite when source has a non-empty value.
	if src.Email != "" {
		dst.Email = src.Email
	}
	if src.UserName != "" {
		dst.UserName = src.UserName
	}
	if src.UserID != "" {
		dst.UserID = src.UserID
	}
	if src.OrgID != "" {
		dst.OrgID = src.OrgID
	}
	if src.Plan != "" {
		dst.Plan = src.Plan
	}
	if src.Nickname != "" {
		dst.Nickname = src.Nickname
	}
	// Enabled: only overwrite when the source explicitly carried an enabled value.
	if src.enabledExplicitlySet {
		dst.Enabled = src.Enabled
	}
	// AuthKind: only when explicitly set in source.
	if src.AuthKind != "" {
		dst.AuthKind = src.AuthKind
	}
	// Usage: only when the source carries one.
	if src.Usage != nil {
		dst.Usage = src.Usage
	}
	// UsageError: only when the source has one.
	if src.UsageErr != "" {
		dst.UsageErr = src.UsageErr
	}
}

func findIndex(store *AccountStore, match func(*Account) bool) *Account {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, a := range store.accounts {
		if match(a) {
			return a
		}
	}
	return nil
}

// ---------- disk loading / migration ----------------------------------------

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
