package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/buildinfo"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

const (
	// maxUpstreams caps the endpoint list; maxSystemPrompt caps the global
	// system prompt.
	maxUpstreams    = 8
	maxSystemPrompt = 32 << 10

	systemPrepend = "prepend"
	systemAppend  = "append"
)

// runtimeSettings are the settings editable from the panel, persisted as
// settings.json in the data dir.
type runtimeSettings struct {
	// Upstreams are Devin server base URLs tried in order; empty means the
	// built-in default.
	Upstreams []string `json:"upstreams"`
	// SystemPrompt is added to every chat request's system prompt when
	// SystemPromptEnabled is set, before or after it per SystemPromptMode.
	SystemPrompt        string `json:"system_prompt"`
	SystemPromptEnabled bool   `json:"system_prompt_enabled"`
	SystemPromptMode    string `json:"system_prompt_mode"`
	// CacheEnabled turns on the response cache (see cache.go). The limits
	// default when zero: CacheTTLMinutes 5 (counted from the last hit),
	// CacheMaxEntries 1000, CacheMaxMB 64.
	CacheEnabled    bool `json:"cache_enabled"`
	CacheTTLMinutes int  `json:"cache_ttl_minutes"`
	CacheMaxEntries int  `json:"cache_max_entries"`
	CacheMaxMB      int  `json:"cache_max_mb"`
}

// defaultSettings are the settings of a fresh data dir.
func defaultSettings() runtimeSettings {
	return runtimeSettings{SystemPromptMode: systemPrepend, CacheEnabled: true}
}

func (v runtimeSettings) cacheTTL() time.Duration {
	if v.CacheTTLMinutes <= 0 {
		return defaultCacheTTL
	}
	return time.Duration(v.CacheTTLMinutes) * time.Minute
}

func (v runtimeSettings) cacheMaxEntries() int {
	if v.CacheMaxEntries <= 0 {
		return defaultCacheEntries
	}
	return v.CacheMaxEntries
}

func (v runtimeSettings) cacheMaxBytes() int {
	if v.CacheMaxMB <= 0 {
		return defaultCacheMB << 20
	}
	return v.CacheMaxMB << 20
}

// settingsStore holds the runtime settings and their file.
type settingsStore struct {
	mu   sync.RWMutex
	v    runtimeSettings
	path string // "" keeps settings in memory only
}

func (st *settingsStore) load(dataDir string) {
	st.v = defaultSettings()
	if dataDir == "" {
		return
	}
	st.path = filepath.Join(dataDir, "settings.json")
	b, err := os.ReadFile(st.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("settings: read failed, using defaults", "err", err)
		}
		return
	}
	var v runtimeSettings
	if err := json.Unmarshal(b, &v); err != nil {
		slog.Warn("settings: invalid settings.json, using defaults", "err", err)
		return
	}
	// A settings.json from before the response cache has no cache_enabled
	// key; the cache is on by default, so only an explicit false turns it off.
	var keys map[string]json.RawMessage
	if json.Unmarshal(b, &keys) == nil {
		if _, ok := keys["cache_enabled"]; !ok {
			v.CacheEnabled = true
		}
	}
	// An upstream the current rules refuse (e.g. a plain-http LAN address
	// saved by an older version) is dropped on its own, so the rest of the
	// settings survive.
	kept := v.Upstreams[:0]
	for _, raw := range v.Upstreams {
		if _, err := normalizeUpstream(raw); err != nil {
			slog.Warn("settings: dropping upstream endpoint", "err", err)
			continue
		}
		kept = append(kept, raw)
	}
	v.Upstreams = kept
	if v, err = normalizeSettings(v); err != nil {
		slog.Warn("settings: invalid settings.json, using defaults", "err", err)
		return
	}
	st.v = v
}

func (st *settingsStore) get() runtimeSettings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	v := st.v
	v.Upstreams = append([]string(nil), st.v.Upstreams...)
	return v
}

// set validates, saves and applies v. The file is written before the
// in-memory value changes, so a failed save changes nothing.
func (st *settingsStore) set(v runtimeSettings) (runtimeSettings, error) {
	v, err := normalizeSettings(v)
	if err != nil {
		return v, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.path != "" {
		b, _ := json.MarshalIndent(v, "", "  ")
		if err := store.WriteFileAtomic(st.path, b, 0o600); err != nil {
			return v, fmt.Errorf("save settings: %w", err)
		}
	}
	st.v = v
	return v, nil
}

// normalizeSettings checks v and puts it in canonical form: upstream URLs
// trimmed of trailing slashes and de-duplicated, mode defaulted.
func normalizeSettings(v runtimeSettings) (runtimeSettings, error) {
	var ups []string
	seen := map[string]bool{}
	for i, raw := range v.Upstreams {
		u, err := normalizeUpstream(raw)
		if err != nil {
			return v, fmt.Errorf("upstreams[%d]: %w", i, err)
		}
		if u != "" && !seen[u] {
			seen[u] = true
			ups = append(ups, u)
		}
	}
	if len(ups) > maxUpstreams {
		return v, fmt.Errorf("at most %d upstream endpoints", maxUpstreams)
	}
	v.Upstreams = ups
	if len(v.SystemPrompt) > maxSystemPrompt {
		return v, fmt.Errorf("system prompt exceeds %d KB", maxSystemPrompt>>10)
	}
	switch v.SystemPromptMode {
	case "":
		v.SystemPromptMode = systemPrepend
	case systemPrepend, systemAppend:
	default:
		return v, fmt.Errorf("system_prompt_mode must be %q or %q", systemPrepend, systemAppend)
	}
	switch {
	case v.CacheTTLMinutes < 0 || v.CacheTTLMinutes > maxCacheTTLMinutes:
		return v, fmt.Errorf("cache_ttl_minutes must be between 0 and %d", maxCacheTTLMinutes)
	case v.CacheMaxEntries < 0 || v.CacheMaxEntries > maxCacheEntries:
		return v, fmt.Errorf("cache_max_entries must be between 0 and %d", maxCacheEntries)
	case v.CacheMaxMB < 0 || v.CacheMaxMB > maxCacheMB:
		return v, fmt.Errorf("cache_max_mb must be between 0 and %d", maxCacheMB)
	}
	return v, nil
}

// normalizeUpstream accepts an http(s) base URL with no query or fragment
// and returns it without a trailing slash. Blank input returns "".
func normalizeUpstream(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("%q is not an http(s) URL", raw)
	}
	// Every account's session token goes to the upstream, so plain http is
	// only allowed to a local address (tests, a local proxy).
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("%q must use https (http is allowed for localhost only)", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("%q must not carry a query, fragment or credentials", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// applySystemPrompt adds the global system prompt to a request's own.
func (v runtimeSettings) applySystemPrompt(system string) string {
	global := strings.TrimSpace(v.SystemPrompt)
	if !v.SystemPromptEnabled || global == "" {
		return system
	}
	if strings.TrimSpace(system) == "" {
		return global
	}
	if v.SystemPromptMode == systemAppend {
		return system + "\n\n" + global
	}
	return global + "\n\n" + system
}

// upstreams returns the Devin server bases to try, in order.
func (s *Server) upstreams() []string {
	if ups := s.settings.get().Upstreams; len(ups) > 0 {
		return ups
	}
	return []string{s.loginServerBase}
}

// endpointFailure reports whether err points at the endpoint rather than
// the account or request: a transport error or a 502/503/504. Another
// endpoint may then succeed with the same account.
func endpointFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ue *devin.UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == http.StatusBadGateway || ue.Status == http.StatusServiceUnavailable ||
			ue.Status == http.StatusGatewayTimeout
	}
	return !errors.Is(err, devin.ErrTokenExpired)
}

// onUpstreams runs fn against each endpoint in turn until one does not
// fail as an endpoint (see endpointFailure).
func (s *Server) onUpstreams(fn func(base string) error) error {
	ups := s.upstreams()
	var err error
	for i, base := range ups {
		if err = fn(base); !endpointFailure(err) {
			return err
		}
		if i < len(ups)-1 {
			slog.Warn("devin endpoint failed, trying the next one", "endpoint", base, "err", err)
		}
	}
	return err
}

// fetchUserStatus reads an account's quota through the endpoint list.
func (s *Server) fetchUserStatus(ctx context.Context, sessionToken string) (*store.Usage, error) {
	var usage *store.Usage
	err := s.onUpstreams(func(base string) error {
		var err error
		usage, err = devin.FetchUserStatus(ctx, base, sessionToken)
		return err
	})
	return usage, err
}

// fetchModelsFor reads an account's model catalog through the endpoint list.
func (s *Server) fetchModelsFor(ctx context.Context, sessionToken string) ([]devin.ModelInfo, error) {
	var models []devin.ModelInfo
	err := s.onUpstreams(func(base string) error {
		var err error
		models, err = devin.FetchModels(ctx, base, sessionToken)
		return err
	})
	return models, err
}

// settingsView is the admin API shape of the settings.
type settingsView struct {
	Listen          string `json:"listen"`
	DataDir         string `json:"data_dir"`
	Version         string `json:"version"`
	DefaultUpstream string `json:"default_upstream"`
	runtimeSettings
}

func (s *Server) settingsView() settingsView {
	return settingsView{
		Listen: s.listen, DataDir: s.dataDir, Version: buildinfo.String(),
		DefaultUpstream: s.loginServerBase, runtimeSettings: s.settings.get(),
	}
}

// handleSettings serves GET (read) and PUT (replace the editable fields).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeAdminJSON(w, http.StatusOK, s.settingsView())
	case http.MethodPut:
		var v runtimeSettings
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid settings JSON: "+err.Error())
			return
		}
		if _, err := s.settings.set(v); err != nil {
			status := http.StatusBadRequest
			if strings.HasPrefix(err.Error(), "save settings") {
				status = http.StatusInternalServerError
			}
			writeErr(w, status, err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, s.settingsView())
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// upstreamTestClient is short: the check only proves reachability.
var upstreamTestClient = &http.Client{Timeout: 10 * time.Second}

// handleUpstreamTest checks that an endpoint answers the Devin API path.
// It sends no account token, so testing an unknown URL leaks nothing; any
// HTTP answer counts as reachable.
func (s *Server) handleUpstreamTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	base, err := normalizeUpstream(body.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if base == "" {
		writeErr(w, http.StatusBadRequest, "url is required")
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, base+"/exa.api_server_pb.ApiServerService/GetCliModelConfigs", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/proto")
	start := time.Now()
	resp, err := upstreamTestClient.Do(req)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		msg := err.Error()
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			msg = "host not found: " + dnsErr.Name
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"ok": false, "ms": ms, "error": msg})
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "ms": ms, "status": resp.StatusCode})
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
