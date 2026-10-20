package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

// The response cache answers a chat request that is byte-for-byte the same
// as an earlier successful one (same model, system prompt, messages, tools
// and sampling parameters) from memory, so repeated requests spend no
// account quota. Every chat endpoint goes through streamWithFailover, so the
// cache serves all API formats, streamed or not, by replaying the recorded
// output deltas.
//
// It follows the Anthropic prompt cache conventions where they apply to a
// proxy: an entry's lifetime is refreshed on every hit (the "ephemeral"
// cache, 5 minutes by default); cache_control markers in a request do not
// change what is cached; and a hit reports its prompt tokens as read from
// the cache (cache_read_input_tokens on /v1/messages,
// prompt_tokens_details.cached_tokens on the OpenAI endpoints), so clients
// and cost trackers see what was not spent. Devin exposes no prompt cache
// of its own, so a prefix shared with an earlier request still costs the
// full prompt upstream; only a whole repeated request is saved.

const (
	// Defaults for the zero values of the cache settings.
	defaultCacheTTL     = 5 * time.Minute
	defaultCacheEntries = 1000
	defaultCacheMB      = 64

	// Upper bounds for the panel-editable cache settings.
	maxCacheTTLMinutes = 7 * 24 * 60
	maxCacheEntries    = 100000
	maxCacheMB         = 4096

	// cacheChunk bounds how far consecutive text deltas are merged for
	// storage; replay still streams in pieces of about this size.
	cacheChunk = 512

	// cacheEntryOverhead approximates the bookkeeping bytes of one entry.
	cacheEntryOverhead = 256

	// cacheSaveDelay batches entry changes into one write of cache.json.
	cacheSaveDelay = 2 * time.Second
)

// cacheEntry is one recorded chat answer.
type cacheEntry struct {
	Key     string           `json:"key"`
	Model   string           `json:"model"` // upstream model that answered
	Deltas  []devin.Delta    `json:"deltas"`
	Result  devin.ChatResult `json:"result"`
	Created time.Time        `json:"created"`
	LastHit time.Time        `json:"last_hit"`
	Hits    int              `json:"hits"`
	Size    int              `json:"size"` // approximate bytes held

	used uint64 // recency rank for eviction: higher is more recent
}

// responseCache holds the entries, persisted to <data>/cache.json so hits
// survive restarts.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	bytes   int
	seq     uint64 // last recency rank handed out

	path    string // "" keeps the cache in memory only (tests)
	pending bool
	writeMu sync.Mutex // one file write at a time
}

type cacheFile struct {
	Entries []*cacheEntry `json:"entries"`
}

// load restores entries from dataDir; a missing or unreadable file starts
// empty. Entries past ttl are dropped.
func (c *responseCache) load(dataDir string, ttl time.Duration) {
	if dataDir == "" {
		return
	}
	c.path = filepath.Join(dataDir, "cache.json")
	b, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var f cacheFile
	if json.Unmarshal(b, &f) != nil {
		slog.Warn("response cache: invalid cache.json, starting empty")
		return
	}
	now := time.Now()
	// Recency ranks restart from the saved hit times.
	sort.Slice(f.Entries, func(i, j int) bool {
		return f.Entries[i] != nil && f.Entries[j] != nil && f.Entries[i].LastHit.Before(f.Entries[j].LastHit)
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*cacheEntry, len(f.Entries))
	for _, e := range f.Entries {
		if e == nil || e.Key == "" || now.Sub(e.expiresFrom()) > ttl {
			continue
		}
		c.seq++
		e.used = c.seq
		c.entries[e.Key] = e
		c.bytes += e.Size
	}
}

// changed schedules a save unless one is already pending.
func (c *responseCache) changed() {
	if c.path == "" {
		return
	}
	if c.pending {
		return
	}
	c.pending = true
	time.AfterFunc(cacheSaveDelay, c.save)
}

func (c *responseCache) save() {
	c.mu.Lock()
	c.pending = false
	f := cacheFile{Entries: make([]*cacheEntry, 0, len(c.entries))}
	for _, e := range c.entries {
		cp := *e
		f.Entries = append(f.Entries, &cp)
	}
	c.mu.Unlock()
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Created.Before(f.Entries[j].Created) })
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	b, _ := json.Marshal(f)
	if err := store.WriteFileAtomic(c.path, b, 0o600); err != nil {
		slog.Warn("save response cache", "error", err)
	}
}

// flush writes a pending save now.
func (c *responseCache) flush() {
	c.mu.Lock()
	pending := c.pending
	c.mu.Unlock()
	if pending && c.path != "" {
		c.save()
	}
}

// expiresFrom is the time an entry's ttl counts from: its last hit, so an
// answer in use stays cached, as in Anthropic's ephemeral cache.
func (e *cacheEntry) expiresFrom() time.Time {
	if e.LastHit.After(e.Created) {
		return e.LastHit
	}
	return e.Created
}

// get returns the entry for key unless it is missing or unused for longer
// than ttl, and records the hit, which extends the entry's life by ttl.
func (c *responseCache) get(key string, ttl time.Duration) (*cacheEntry, bool) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return nil, false
	}
	if now.Sub(e.expiresFrom()) > ttl {
		delete(c.entries, key)
		c.bytes -= e.Size
		c.changed()
		return nil, false
	}
	e.Hits++
	e.LastHit = now
	c.seq++
	e.used = c.seq
	c.changed()
	cp := *e
	cp.Deltas = slices.Clone(e.Deltas)
	cp.Result.ToolCalls = slices.Clone(e.Result.ToolCalls)
	return &cp, true
}

// put stores e, evicting entries unused for ttl first and then the least
// recently hit ones until the entry count and byte size fit. An entry larger
// than maxBytes on its own is not stored.
func (c *responseCache) put(e *cacheEntry, ttl time.Duration, maxEntries, maxBytes int) {
	if e.Size > maxBytes {
		return
	}
	now := time.Now()
	e.Created, e.LastHit = now, now
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*cacheEntry{}
	}
	if old := c.entries[e.Key]; old != nil {
		c.bytes -= old.Size
		delete(c.entries, e.Key)
	}
	for k, o := range c.entries {
		if now.Sub(o.expiresFrom()) > ttl {
			c.bytes -= o.Size
			delete(c.entries, k)
		}
	}
	for len(c.entries) >= maxEntries || c.bytes+e.Size > maxBytes {
		var victim *cacheEntry
		for _, o := range c.entries {
			if victim == nil || o.used < victim.used {
				victim = o
			}
		}
		if victim == nil {
			break
		}
		c.bytes -= victim.Size
		delete(c.entries, victim.Key)
	}
	c.seq++
	e.used = c.seq
	c.entries[e.Key] = e
	c.bytes += e.Size
	c.changed()
}

// clear drops every entry, in memory and on disk.
func (c *responseCache) clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.bytes = nil, 0
	if c.path == "" {
		return nil
	}
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// cacheView is the admin API shape of the cache state.
type cacheView struct {
	Enabled           bool  `json:"enabled"`
	Entries           int   `json:"entries"`
	Bytes             int   `json:"bytes"`
	Hits              int64 `json:"hits"`
	SavedInputTokens  int64 `json:"saved_input_tokens"`
	SavedOutputTokens int64 `json:"saved_output_tokens"`
}

func (s *Server) cacheView() cacheView {
	s.cache.mu.Lock()
	n, b := len(s.cache.entries), s.cache.bytes
	s.cache.mu.Unlock()
	return cacheView{
		Enabled: s.settings.get().CacheEnabled, Entries: n, Bytes: b,
		Hits:             s.stats.cacheHits.Load(),
		SavedInputTokens: s.stats.savedIn.Load(), SavedOutputTokens: s.stats.savedOut.Load(),
	}
}

// cacheKey identifies a request by everything that shapes its answer: the
// resolved model, system prompt (with the global one applied), messages,
// tools, and sampling parameters. Images and tool schemas are hashed too.
func cacheKey(req devin.ChatRequest) string {
	req.Model = devin.ResolveModel(req.Model)
	h := sha256.New()
	h.Write([]byte("devin2api-cache-v1\n"))
	json.NewEncoder(h).Encode(req)
	return hex.EncodeToString(h.Sum(nil))
}

// appendDelta records d for replay, merging it into the previous delta when
// both are plain text (or both thinking) and the merged piece stays small,
// so a long answer does not store thousands of one-word deltas.
func appendDelta(rec []devin.Delta, d devin.Delta) []devin.Delta {
	if n := len(rec); n > 0 && d.ToolCall == nil {
		last := &rec[n-1]
		if last.ToolCall == nil && len(last.Text)+len(last.Thinking) < cacheChunk &&
			((d.Thinking == "" && last.Thinking == "") || (d.Text == "" && last.Text == "")) {
			last.Text += d.Text
			last.Thinking += d.Thinking
			return rec
		}
	}
	if tc := d.ToolCall; tc != nil {
		cp := *tc
		d.ToolCall = &cp
	}
	return append(rec, d)
}

// deltaSize approximates the bytes a delta holds.
func deltaSize(d devin.Delta) int {
	n := len(d.Text) + len(d.Thinking)
	if d.ToolCall != nil {
		n += len(d.ToolCall.ID) + len(d.ToolCall.Name) + len(d.ToolCall.Arguments)
	}
	return n
}

// streamWithFailover answers req from the response cache when it holds the
// same request, and otherwise runs it upstream (see streamUpstream) and
// records the answer. The global system prompt is applied here, so it is
// part of the cache key.
func (s *Server) streamWithFailover(ctx context.Context, req devin.ChatRequest, onDelta func(devin.Delta) error) (*devin.ChatResult, error) {
	cfg := s.settings.get()
	req.System = cfg.applySystemPrompt(req.System)
	if !cfg.CacheEnabled {
		return s.streamUpstream(ctx, req, onDelta)
	}
	key := cacheKey(req)
	if e, ok := s.cache.get(key, cfg.cacheTTL()); ok {
		tr := traceFrom(ctx)
		tr.cached()
		for _, d := range e.Deltas {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			tr.output()
			if err := onDelta(d); err != nil {
				return nil, err
			}
		}
		res := e.Result
		res.CachedPromptTokens = res.PromptTokens
		return &res, nil
	}
	var rec []devin.Delta
	size := cacheEntryOverhead
	res, err := s.streamUpstream(ctx, req, func(d devin.Delta) error {
		rec = appendDelta(rec, d)
		size += deltaSize(d)
		return onDelta(d)
	})
	if err != nil || res == nil {
		return res, err
	}
	e := &cacheEntry{Key: key, Model: res.Model, Deltas: rec, Result: *res, Size: size}
	if e.Model == "" {
		e.Model = devin.ResolveModel(req.Model)
	}
	e.Result.ToolCalls = slices.Clone(res.ToolCalls)
	for _, c := range res.ToolCalls {
		e.Size += len(c.ID) + len(c.Name) + len(c.Arguments)
	}
	s.cache.put(e, cfg.cacheTTL(), cfg.cacheMaxEntries(), cfg.cacheMaxBytes())
	return res, nil
}
