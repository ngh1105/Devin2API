package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
)

// TestCacheHitSkipsUpstream: a repeated request is answered from the cache
// with the same body and usage, reaches no account, and is logged as cached
// with its tokens counted as saved.
func TestCacheHitSkipsUpstream(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	body := `{"model":"devin","messages":[{"role":"user","content":"ping"}]}`
	var first, second openai.ChatCompletion
	for i, out := range []*openai.ChatCompletion{&first, &second} {
		rec := postChat(srv, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d: %s", i, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.calls) != 1 {
		t.Fatalf("upstream called %d times, want 1", len(fake.calls))
	}
	if strOf(second.Choices[0].Message.Content) != "pong" || second.Usage == nil || second.Usage.TotalTokens != 12 {
		t.Errorf("cached completion = %+v usage %+v", second.Choices[0], second.Usage)
	}
	logs := srv.reqLog.recent(2)
	if len(logs) != 2 || !logs[0].Cached || logs[0].Attempts != 0 || logs[1].Cached {
		t.Errorf("log entries = %+v", logs)
	}
	if srv.stats.cacheHits.Load() != 1 || srv.stats.savedIn.Load() != 10 || srv.stats.savedOut.Load() != 2 {
		t.Errorf("cache stats: hits %d saved %d/%d", srv.stats.cacheHits.Load(), srv.stats.savedIn.Load(), srv.stats.savedOut.Load())
	}
	hours := srv.stats.hourly(1, time.Now(), false)
	if len(hours) != 1 || hours[0].Cached != 1 || hours[0].InputTokens != 10 || hours[0].SavedInputTokens != 10 {
		t.Errorf("hourly = %+v", hours)
	}
	// A different message is a miss.
	if rec := postChat(srv, `{"model":"devin","messages":[{"role":"user","content":"ping!"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(fake.calls) != 2 {
		t.Errorf("upstream called %d times, want 2", len(fake.calls))
	}
}

// TestCacheReplayStreams: a cached answer streams as SSE chunks like a live
// one, and the cache also serves the Anthropic endpoint for the same chat.
func TestCacheReplayStreams(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	if rec := postChat(srv, `{"messages":[{"role":"user","content":"ping"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	rec := postChat(srv, `{"messages":[{"role":"user","content":"ping"}],"stream":true,"stream_options":{"include_usage":true}}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d ct %q body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	var text strings.Builder
	var usage *openai.Usage
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c openai.ChatChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("chunk %q: %v", data, err)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if text.String() != "pong" || usage == nil || usage.TotalTokens != 12 {
		t.Errorf("streamed text %q usage %+v", text.String(), usage)
	}
	if len(fake.calls) != 1 {
		t.Errorf("upstream called %d times, want 1", len(fake.calls))
	}
}

// TestCacheDisabledAndCleared: turning the cache off goes upstream every
// time; DELETE /admin/api/cache drops the stored answers.
func TestCacheDisabledAndCleared(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	body := `{"model":"devin","messages":[{"role":"user","content":"ping"}]}`
	postChat(srv, body)
	postChat(srv, body)
	if len(fake.calls) != 1 {
		t.Fatalf("upstream called %d times, want 1", len(fake.calls))
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodDelete, "/admin/api/cache", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if v := srv.cacheView(); v.Entries != 0 || v.Hits != 1 {
		t.Errorf("after clear: %+v", v)
	}
	postChat(srv, body)
	if len(fake.calls) != 2 {
		t.Errorf("upstream called %d times after clear, want 2", len(fake.calls))
	}
	cfg := srv.settings.get()
	cfg.CacheEnabled = false
	if _, err := srv.settings.set(cfg); err != nil {
		t.Fatal(err)
	}
	postChat(srv, body)
	postChat(srv, body)
	if len(fake.calls) != 4 {
		t.Errorf("upstream called %d times with the cache off, want 4", len(fake.calls))
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodGet, "/admin/api/stats", nil))
	var stats struct {
		Cache cacheView `json:"cache"`
	}
	json.Unmarshal(rec.Body.Bytes(), &stats)
	if stats.Cache.Enabled || stats.Cache.Hits != 1 || stats.Cache.SavedInputTokens != 10 {
		t.Errorf("stats cache = %+v", stats.Cache)
	}
}

// TestCacheHitUsageFollowsAnthropic: a hit reports its prompt as read from
// the cache: cache_read_input_tokens on /v1/messages with input_tokens 0,
// prompt_tokens_details.cached_tokens on the OpenAI endpoints. A request's
// cache_control markers do not change the entry it maps to.
func TestCacheHitUsageFollowsAnthropic(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	post := func(path, body string) map[string]any {
		req := v1Request(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body)
		}
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	usage := func(m map[string]any) map[string]any { return m["usage"].(map[string]any) }
	miss := post("/v1/messages", `{"model":"devin","max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`)
	if u := usage(miss); u["input_tokens"].(float64) != 10 || u["cache_read_input_tokens"].(float64) != 0 {
		t.Errorf("miss usage = %v", u)
	}
	// Same chat with cache_control markers: a hit.
	hit := post("/v1/messages", `{"model":"devin","max_tokens":100,"system":[],"messages":[{"role":"user","content":[{"type":"text","text":"ping","cache_control":{"type":"ephemeral"}}]}]}`)
	if u := usage(hit); u["input_tokens"].(float64) != 0 || u["cache_read_input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 2 {
		t.Errorf("hit usage = %v", u)
	}
	// The OpenAI shape of the same chat.
	oa := post("/v1/chat/completions", `{"model":"devin","max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`)
	u := usage(oa)
	det, _ := u["prompt_tokens_details"].(map[string]any)
	if u["prompt_tokens"].(float64) != 10 || det == nil || det["cached_tokens"].(float64) != 10 {
		t.Errorf("openai hit usage = %v", u)
	}
	rs := post("/v1/responses", `{"model":"devin","max_output_tokens":100,"input":"ping"}`)
	u = usage(rs)
	det, _ = u["input_tokens_details"].(map[string]any)
	if u["input_tokens"].(float64) != 10 || det == nil || det["cached_tokens"].(float64) != 10 {
		t.Errorf("responses hit usage = %v", u)
	}
	if len(fake.calls) != 1 {
		t.Errorf("upstream called %d times, want 1", len(fake.calls))
	}
	// A miss carries no cached_tokens detail.
	if u := usage(post("/v1/chat/completions", `{"model":"devin","messages":[{"role":"user","content":"pong?"}]}`)); u["prompt_tokens_details"] != nil {
		t.Errorf("miss usage = %v", u)
	}
}

// TestCacheKeyCoversRequestShape: the key changes with anything that shapes
// the answer and not with the order-free parts of the request.
func TestCacheKeyCoversRequestShape(t *testing.T) {
	base := devin.ChatRequest{Model: "devin", System: "s", MaxTokens: 100, Temperature: 1,
		Prompts: []devin.Prompt{{Source: devin.SourceUser, Text: "hi"}},
		Tools:   []devin.Tool{{Name: "t", Parameters: []byte(`{"type":"object"}`)}}}
	key := cacheKey(base)
	if cacheKey(base) != key {
		t.Fatal("key is not stable")
	}
	alias := base
	alias.Model = "swe-2-high" // what "devin" resolves to
	if cacheKey(alias) != key {
		t.Error("model alias and its target hash differently")
	}
	for name, mut := range map[string]func(r *devin.ChatRequest){
		"system":      func(r *devin.ChatRequest) { r.System = "S" },
		"prompt":      func(r *devin.ChatRequest) { r.Prompts[0].Text = "hi!" },
		"max_tokens":  func(r *devin.ChatRequest) { r.MaxTokens = 101 },
		"temperature": func(r *devin.ChatRequest) { r.Temperature = 0 },
		"tools":       func(r *devin.ChatRequest) { r.Tools = nil },
		"stop":        func(r *devin.ChatRequest) { r.Stop = []string{"x"} },
		"image": func(r *devin.ChatRequest) {
			r.Prompts[0].Images = []devin.Image{{MimeType: "image/png", Base64: "AAAA"}}
		},
	} {
		r := base
		r.Prompts = []devin.Prompt{base.Prompts[0]}
		mut(&r)
		if cacheKey(r) == key {
			t.Errorf("%s: change did not change the key", name)
		}
	}
}

// TestCacheEvictionAndTTL: entries past the TTL are not served, the entry
// and byte limits evict the least recently hit entries first, and an entry
// too big for the cache is skipped.
func TestCacheEvictionAndTTL(t *testing.T) {
	var c responseCache
	entry := func(key string, size int) *cacheEntry {
		return &cacheEntry{Key: key, Deltas: []devin.Delta{{Text: strings.Repeat("x", size)}}, Size: size}
	}
	c.put(entry("a", 10), time.Hour, 2, 100)
	c.put(entry("b", 10), time.Hour, 2, 100)
	if _, ok := c.get("a", time.Hour); !ok { // a is now the most recently hit
		t.Fatal("a missing")
	}
	c.put(entry("c", 10), time.Hour, 2, 100)
	if _, ok := c.get("b", time.Hour); ok {
		t.Error("b should have been evicted as least recently hit")
	}
	if _, ok := c.get("a", time.Hour); !ok {
		t.Error("a should have survived")
	}
	c.put(entry("big", 101), time.Hour, 2, 100)
	if _, ok := c.get("big", time.Hour); ok {
		t.Error("an entry over the byte limit was stored")
	}
	c.put(entry("d", 90), time.Hour, 10, 100) // bytes: a(10)+c(10)+d(90) > 100 evicts
	if len(c.entries) != 2 || c.bytes > 100 {
		t.Errorf("entries %d bytes %d after byte-limit eviction", len(c.entries), c.bytes)
	}
	c.put(entry("old", 1), time.Hour, 10, 100)
	c.entries["old"].Created = time.Now().Add(-2 * time.Hour)
	c.entries["old"].LastHit = c.entries["old"].Created
	if _, ok := c.get("old", time.Hour); ok {
		t.Error("an expired entry was served")
	}
	// A hit extends the life of an entry (ephemeral-cache semantics).
	c.put(entry("used", 1), time.Hour, 10, 100)
	c.entries["used"].Created = time.Now().Add(-2 * time.Hour)
	c.entries["used"].LastHit = time.Now().Add(-30 * time.Minute)
	if _, ok := c.get("used", time.Hour); !ok {
		t.Error("an entry hit 30 minutes ago expired under a 1 h ttl")
	}
}

// TestCachePersists: entries written to cache.json are served after a
// restart; expired ones are dropped on load.
func TestCachePersists(t *testing.T) {
	dir := t.TempDir()
	var c responseCache
	c.load(dir, time.Hour)
	c.put(&cacheEntry{Key: "k", Deltas: []devin.Delta{{Text: "pong"}}, Result: devin.ChatResult{PromptTokens: 3}, Size: 4}, time.Hour, 10, 1<<20)
	c.put(&cacheEntry{Key: "stale", Deltas: []devin.Delta{{Text: "old"}}, Size: 3}, time.Hour, 10, 1<<20)
	c.entries["stale"].Created = time.Now().Add(-2 * time.Hour)
	c.entries["stale"].LastHit = c.entries["stale"].Created
	c.flush()
	var again responseCache
	again.load(dir, time.Hour)
	e, ok := again.get("k", time.Hour)
	if !ok || e.Deltas[0].Text != "pong" || e.Result.PromptTokens != 3 {
		t.Fatalf("reloaded entry = %+v ok %v", e, ok)
	}
	if _, ok := again.get("stale", time.Hour); ok {
		t.Error("expired entry survived the reload")
	}
	// The server loads it too and reports it.
	srv := New(config.Config{Listen: ":0", DataDir: dir}, mustAccounts(t), mustTokens(t))
	if v := srv.cacheView(); v.Entries != 1 || !v.Enabled {
		t.Errorf("server cache view = %+v", v)
	}
}

// TestCacheSettingsDefaultOn: a settings.json saved before the cache existed
// leaves it on; an explicit false turns it off; limits are validated.
func TestCacheSettingsDefaultOn(t *testing.T) {
	dir := t.TempDir()
	srv := makeTestServer(t, dir)
	if !srv.settings.get().CacheEnabled {
		t.Error("fresh server: cache off")
	}
	var st settingsStore
	st.path = dir + "/settings.json"
	if _, err := st.set(runtimeSettings{CacheEnabled: false}); err != nil {
		t.Fatal(err)
	}
	var reload settingsStore
	reload.load(dir)
	if reload.get().CacheEnabled {
		t.Error("explicit false was not kept")
	}
	// Old file without the key.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodPut, "/admin/api/settings",
		strings.NewReader(`{"upstreams":[],"system_prompt":"","system_prompt_enabled":false,"system_prompt_mode":"prepend","cache_enabled":true,"cache_ttl_minutes":-1,"cache_max_entries":0,"cache_max_mb":0}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cache_ttl_minutes") {
		t.Errorf("bad ttl: %d %s", rec.Code, rec.Body)
	}
	v := runtimeSettings{}
	if v.cacheTTL() != defaultCacheTTL || v.cacheMaxEntries() != defaultCacheEntries || v.cacheMaxBytes() != defaultCacheMB<<20 {
		t.Error("zero limits do not default")
	}
}

// TestAppendDeltaMerges: small consecutive text deltas merge up to the chunk
// size; thinking, text and tool-call deltas stay apart.
func TestAppendDeltaMerges(t *testing.T) {
	var rec []devin.Delta
	for i := 0; i < 10; i++ {
		rec = appendDelta(rec, devin.Delta{Text: "ab"})
	}
	if len(rec) != 1 || rec[0].Text != strings.Repeat("ab", 10) {
		t.Errorf("text deltas = %+v", rec)
	}
	rec = appendDelta(rec, devin.Delta{Thinking: "hm"})
	rec = appendDelta(rec, devin.Delta{Thinking: "m"})
	rec = appendDelta(rec, devin.Delta{ToolCall: &devin.ToolCallDelta{ID: "c1", Name: "t"}})
	rec = appendDelta(rec, devin.Delta{ToolCall: &devin.ToolCallDelta{Arguments: "{}"}})
	rec = appendDelta(rec, devin.Delta{Text: "end"})
	if len(rec) != 5 || rec[1].Thinking != "hmm" || rec[2].ToolCall.ID != "c1" || rec[4].Text != "end" {
		t.Errorf("mixed deltas = %+v", rec)
	}
	big := appendDelta(nil, devin.Delta{Text: strings.Repeat("x", cacheChunk)})
	big = appendDelta(big, devin.Delta{Text: "y"})
	if len(big) != 2 {
		t.Errorf("chunk limit not applied: %d deltas", len(big))
	}
}
