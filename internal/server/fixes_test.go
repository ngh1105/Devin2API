package server

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
)

// setCache turns the response cache on or off for a test server.
func setCache(t *testing.T, srv *Server, on bool) {
	t.Helper()
	cfg := srv.settings.get()
	cfg.CacheEnabled = on
	if _, err := srv.settings.set(cfg); err != nil {
		t.Fatal(err)
	}
}

func enableCache(t *testing.T, srv *Server) { setCache(t, srv, true) }

func cacheEntryOf(key string) *cacheEntry {
	return &cacheEntry{Key: key, Deltas: []devin.Delta{{Text: "x"}}, Size: 1}
}

func cacheKeys(m map[string]*cacheEntry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCacheSkipsEmptyAndRefusedAnswers: an answer with no output, or one the
// model refused (content_filter), is not cached, so a retry reaches upstream.
func TestCacheSkipsEmptyAndRefusedAnswers(t *testing.T) {
	var calls atomic.Int32
	var refuse atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		if refuse.Load() {
			w.Write(connectFrame(0, append(devin.PwFieldString(3, "no"), devin.PwFieldVarint(5, 11)...)))
		}
		w.Write(connectFrame(2, []byte("{}")))
	}))
	defer up.Close()
	srv := settingsChatServer(t, runtimeSettings{Upstreams: []string{up.URL}, CacheEnabled: true})
	for _, m := range []string{"empty", "refused"} {
		refuse.Store(m == "refused")
		calls.Store(0)
		body := `{"messages":[{"role":"user","content":"` + m + `"}]}`
		for i := 0; i < 2; i++ {
			if rec := postChat(srv, body); rec.Code != http.StatusOK {
				t.Fatalf("%s %d: status %d: %s", m, i, rec.Code, rec.Body)
			}
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("%s answer: upstream called %d times, want 2 (not cached)", m, n)
		}
	}
	if v := srv.cacheView(); v.Entries != 0 {
		t.Errorf("cache holds %d entries, want 0", v.Entries)
	}
}

// TestCacheClearBeatsInFlightWork: a save that snapshotted the entries before
// a clear must not write them back, and a request that started before the
// clear must not store its answer.
func TestCacheClearBeatsInFlightWork(t *testing.T) {
	dir := t.TempDir()
	var c responseCache
	c.load(dir, time.Hour, 10, 1<<20)
	c.put(cacheEntryOf("k"), time.Hour, 10, 1<<20)
	snap, gen := c.snapshot()
	if err := c.clear(); err != nil {
		t.Fatal(err)
	}
	c.write(snap, gen)
	if _, err := os.Stat(c.path); !os.IsNotExist(err) {
		t.Error("a snapshot taken before the clear was written after it")
	}
	before := c.generation()
	c.put(cacheEntryOf("other"), time.Hour, 10, 1<<20)
	c.clear()
	c.putSince(before, cacheEntryOf("late"), time.Hour, 10, 1<<20)
	if _, ok := c.get("late", time.Hour); ok {
		t.Error("an answer from a request older than the clear was stored")
	}
	c.putSince(c.generation(), cacheEntryOf("fresh"), time.Hour, 10, 1<<20)
	if _, ok := c.get("fresh", time.Hour); !ok {
		t.Error("an answer from after the clear was not stored")
	}
	c.flush()
	if _, err := os.Stat(c.path); err != nil {
		t.Errorf("cache.json after a put: %v", err)
	}
}

// TestCacheLoadAppliesLimits: a cache.json larger than the current limits is
// trimmed on load, keeping the most recently hit entries.
func TestCacheLoadAppliesLimits(t *testing.T) {
	dir := t.TempDir()
	var c responseCache
	c.load(dir, time.Hour, 10, 1<<20)
	now := time.Now()
	for i, k := range []string{"b", "c", "a"} {
		c.put(cacheEntryOf(k), time.Hour, 10, 1<<20)
		c.entries[k].LastHit = now.Add(-time.Duration(3-i) * time.Minute)
	}
	c.flush()
	var byCount responseCache
	byCount.load(dir, time.Hour, 2, 1<<20)
	if len(byCount.entries) != 2 || byCount.entries["c"] == nil || byCount.entries["a"] == nil {
		t.Errorf("entries after count-limited load: %v", cacheKeys(byCount.entries))
	}
	var byBytes responseCache
	byBytes.load(dir, time.Hour, 10, 2)
	if len(byBytes.entries) != 2 || byBytes.bytes != 2 || byBytes.entries["b"] != nil {
		t.Errorf("entries after byte-limited load: %v (%d bytes)", cacheKeys(byBytes.entries), byBytes.bytes)
	}
}

// TestCacheEvictsManyQuickly: shrinking the limits evicts in one pass; the
// most recently used entries survive.
func TestCacheEvictsManyQuickly(t *testing.T) {
	var c responseCache
	const n = 40000
	for i := 0; i < n; i++ {
		c.put(&cacheEntry{Key: fmt.Sprint(i), Size: 1}, time.Hour, n+1, 1<<30)
	}
	start := time.Now()
	c.put(&cacheEntry{Key: "new", Size: 1}, time.Hour, 1000, 1<<30)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("shrinking %d entries to 1000 took %v", n, d)
	}
	if len(c.entries) != 1000 || c.bytes != 1000 {
		t.Errorf("%d entries, %d bytes after shrink", len(c.entries), c.bytes)
	}
	if c.entries["new"] == nil || c.entries[fmt.Sprint(n-1)] == nil || c.entries["0"] != nil {
		t.Error("eviction did not keep the most recently used entries")
	}
}

// TestCacheHitSavesLazily: a hit alone schedules the save much later than a
// new entry does, and a new entry pulls a pending lazy save forward.
func TestCacheHitSavesLazily(t *testing.T) {
	dir := t.TempDir()
	var c responseCache
	c.load(dir, time.Hour, 10, 1<<20)
	c.put(cacheEntryOf("k"), time.Hour, 10, 1<<20)
	c.flush()
	if !c.saveAt().IsZero() {
		t.Fatal("a save is still pending after flush")
	}
	c.get("k", time.Hour)
	if d := time.Until(c.saveAt()); d < cacheHitSaveDelay-time.Second {
		t.Errorf("a hit scheduled the save in %v, want about %v", d, cacheHitSaveDelay)
	}
	c.put(cacheEntryOf("k2"), time.Hour, 10, 1<<20)
	if d := time.Until(c.saveAt()); d > cacheSaveDelay {
		t.Errorf("a new entry left the save %v away, want within %v", d, cacheSaveDelay)
	}
	c.flush()
}

// TestAppendDeltaMergesToolFragments: streamed argument fragments of one tool
// call merge into its delta, and the size estimate counts each stored delta.
func TestAppendDeltaMergesToolFragments(t *testing.T) {
	rec := appendDelta(nil, devin.Delta{ToolCall: &devin.ToolCallDelta{Index: 0, ID: "c1", Name: "t"}})
	for i := 0; i < 1000; i++ {
		rec = appendDelta(rec, devin.Delta{ToolCall: &devin.ToolCallDelta{Index: 0, Arguments: "ab"}})
	}
	if len(rec) > 8 {
		t.Errorf("1000 fragments stored as %d deltas", len(rec))
	}
	var args strings.Builder
	for _, d := range rec {
		if d.ToolCall == nil || d.ToolCall.Index != 0 {
			t.Fatalf("unexpected delta %+v", d)
		}
		args.WriteString(d.ToolCall.Arguments)
	}
	if rec[0].ToolCall.ID != "c1" || args.String() != strings.Repeat("ab", 1000) {
		t.Error("merged tool call lost its id or arguments")
	}
	rec = appendDelta(rec, devin.Delta{ToolCall: &devin.ToolCallDelta{Index: 1, ID: "c2", Name: "u"}})
	rec = appendDelta(rec, devin.Delta{ToolCall: &devin.ToolCallDelta{Index: 1, Arguments: "{}"}})
	if last := rec[len(rec)-1].ToolCall; last.Index != 1 || last.ID != "c2" || last.Arguments != "{}" {
		t.Errorf("second call = %+v", last)
	}
	if deltaSize(devin.Delta{Text: "ab"}) < 2+cacheDeltaOverhead {
		t.Error("deltaSize ignores the per-delta overhead")
	}
}

// TestSettingsPutKeepsCacheOnWithoutKey: a PUT in the shape of a settings
// file from before the cache existed leaves the cache on; an explicit false
// turns it off.
func TestSettingsPutKeepsCacheOnWithoutKey(t *testing.T) {
	srv := New(config.Config{Listen: ":0"}, mustAccounts(t), mustTokens(t))
	rec := putSettings(srv, `{"upstreams":[],"system_prompt":"x","system_prompt_enabled":true,"system_prompt_mode":"prepend"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if v := srv.settings.get(); !v.CacheEnabled || v.SystemPrompt != "x" {
		t.Errorf("after PUT without cache_enabled: %+v", v)
	}
	if rec := putSettings(srv, `{"cache_enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if srv.settings.get().CacheEnabled {
		t.Error("explicit false did not turn the cache off")
	}
}

// TestSettingsLoadClampsCacheLimits: an out-of-range cache limit in
// settings.json is clamped and the rest of the file is kept.
func TestSettingsLoadClampsCacheLimits(t *testing.T) {
	dir := t.TempDir()
	file := `{"upstreams":["https://a.example.com"],"system_prompt":"keep","system_prompt_enabled":true,"system_prompt_mode":"prepend","cache_enabled":false,"cache_ttl_minutes":-5,"cache_max_entries":1,"cache_max_mb":8192}`
	if err := os.WriteFile(dir+"/settings.json", []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	var st settingsStore
	st.load(dir)
	v := st.get()
	if len(v.Upstreams) != 1 || v.SystemPrompt != "keep" || !v.SystemPromptEnabled || v.CacheEnabled {
		t.Errorf("settings dropped on load: %+v", v)
	}
	if v.CacheTTLMinutes != 0 || v.CacheMaxEntries != 1 || v.CacheMaxMB != maxCacheMB {
		t.Errorf("cache limits not clamped: ttl %d entries %d mb %d", v.CacheTTLMinutes, v.CacheMaxEntries, v.CacheMaxMB)
	}
}

// TestCacheBytesFitInt: the byte limit never overflows the platform int.
func TestCacheBytesFitInt(t *testing.T) {
	if got := mbToBytes(4096, math.MaxInt32); got != math.MaxInt32 {
		t.Errorf("4096 MB on a 32-bit int = %d", got)
	}
	if got := mbToBytes(64, math.MaxInt64); got != 64<<20 {
		t.Errorf("64 MB = %d", got)
	}
}

// TestStopSequenceDoesNotFailOver: a stop sequence that cuts the whole answer
// ends the request; it is not an endpoint failure, so the next endpoint is
// not tried.
func TestStopSequenceDoesNotFailOver(t *testing.T) {
	first, firstBodies := recordingUpstream(t) // answers "ok"
	second, secondBodies := recordingUpstream(t)
	srv := settingsChatServer(t, runtimeSettings{Upstreams: []string{first.URL, second.URL}})
	rec := postChat(srv, `{"messages":[{"role":"user","content":"hi"}],"stop":"ok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if a, b := len(firstBodies()), len(secondBodies()); a != 1 || b != 0 {
		t.Errorf("endpoint calls: first %d, second %d; want 1 and 0", a, b)
	}
}

// TestUpstreamTestDoesNotFollowRedirects: the reachability check reports a
// redirect instead of following it to another host.
func TestUpstreamTestDoesNotFollowRedirects(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redirect.Close()
	srv := settingsChatServer(t, runtimeSettings{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodPost, "/admin/api/settings/upstream-test", strings.NewReader(`{"url":"`+redirect.URL+`"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":302`) {
		t.Errorf("redirecting endpoint: %d %s", rec.Code, rec.Body)
	}
	if targetCalls.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

// TestHourlyStatsCachedOnlyOnSuccess: an aborted cache replay counts as a
// failure, not as a cache hit with saved tokens.
func TestHourlyStatsCachedOnlyOnSuccess(t *testing.T) {
	var c chatStats
	now := time.Now()
	c.record(LogEntry{Time: now, Status: 499, Cached: true, InputTokens: 10, OutputTokens: 2})
	h := c.hourly(1, now, true)
	if len(h) != 1 || h[0].Failed != 1 || h[0].Cached != 0 || h[0].SavedInputTokens != 0 || h[0].InputTokens != 0 {
		t.Errorf("hourly after an aborted replay = %+v", h)
	}
}
