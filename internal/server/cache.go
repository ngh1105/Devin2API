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
	// cacheHitSaveDelay is the write delay when only hit times changed.
	cacheHitSaveDelay = time.Minute

	// cacheDeltaOverhead approximates the bytes one stored delta costs
	// beyond its text, in memory and in cache.json.
	cacheDeltaOverhead = 96
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

	path      string // "" keeps the cache in memory only (tests)
	pending   bool
	saveDue   time.Time   // when the pending save runs
	saveTimer *time.Timer // the pending save
	writeMu   sync.Mutex  // one file write at a time

	gen uint64 // bumped by clear, so older work does not store or write
}

type cacheFile struct {
	Entries []*cacheEntry `json:"entries"`
}

// load restores entries from dataDir; a missing or unreadable file starts
// empty. Entries past ttl are dropped, and when the file holds more than
// the limits allow (they were lowered since it was written), only the most
// recently hit entries that fit are kept.
func (c *responseCache) load(dataDir string, ttl time.Duration, maxEntries, maxBytes int) {
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
	valid := make([]*cacheEntry, 0, len(f.Entries))
	seen := make(map[string]bool, len(f.Entries))
	for _, e := range f.Entries {
		if e == nil || e.Key == "" || seen[e.Key] || now.Sub(e.expiresFrom()) > ttl {
			continue
		}
		seen[e.Key] = true
		valid = append(valid, e)
	}
	// Recency ranks restart from the saved hit times.
	sort.Slice(valid, func(i, j int) bool { return valid[i].expiresFrom().Before(valid[j].expiresFrom()) })
	// Keep the newest entries that fit the limits.
	start, bytes := len(valid), 0
	for i := len(valid) - 1; i >= 0; i-- {
		if len(valid)-i > maxEntries || bytes+valid[i].Size > maxBytes {
			break
		}
		bytes += valid[i].Size
		start = i
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*cacheEntry, len(valid)-start)
	for _, e := range valid[start:] {
		c.seq++
		e.used = c.seq
		c.entries[e.Key] = e
		c.bytes += e.Size
	}
}

// changed schedules a save in delay, unless one is already due sooner. A
// new or evicted entry asks for cacheSaveDelay; a hit, which only moves an
// entry's hit time, asks for cacheHitSaveDelay so a busy cache is not
// rewritten every few seconds. Caller holds c.mu.
func (c *responseCache) changed(delay time.Duration) {
	if c.path == "" {
		return
	}
	due := time.Now().Add(delay)
	if c.pending && !due.Before(c.saveDue) {
		return
	}
	if c.saveTimer != nil {
		c.saveTimer.Stop()
	}
	c.pending, c.saveDue = true, due
	c.saveTimer = time.AfterFunc(delay, c.save)
}

func (c *responseCache) save() {
	f, gen := c.snapshot()
	c.write(f, gen)
}

// snapshot copies the entries for writing, with the generation they belong
// to, and takes over the pending save.
func (c *responseCache) snapshot() (cacheFile, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = false
	if c.saveTimer != nil {
		c.saveTimer.Stop()
		c.saveTimer = nil
	}
	f := cacheFile{Entries: make([]*cacheEntry, 0, len(c.entries))}
	for _, e := range c.entries {
		cp := *e
		f.Entries = append(f.Entries, &cp)
	}
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Created.Before(f.Entries[j].Created) })
	return f, c.gen
}

// write stores a snapshot in cache.json, unless the cache was cleared since
// the snapshot was taken: the clear removed the file, and writing the old
// entries back would resurrect them on the next start.
func (c *responseCache) write(f cacheFile, gen uint64) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	stale := c.gen != gen
	c.mu.Unlock()
	if stale {
		return
	}
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

// generation identifies the cache's contents since the last clear.
func (c *responseCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// saveAt is when the pending save is due, or the zero time if none is.
func (c *responseCache) saveAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pending {
		return time.Time{}
	}
	return c.saveDue
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
		c.changed(cacheSaveDelay)
		return nil, false
	}
	e.Hits++
	e.LastHit = now
	c.seq++
	e.used = c.seq
	c.changed(cacheHitSaveDelay)
	cp := *e
	cp.Deltas = slices.Clone(e.Deltas)
	cp.Result.ToolCalls = slices.Clone(e.Result.ToolCalls)
	return &cp, true
}

// put stores e, evicting entries unused for ttl first and then the least
// recently hit ones until the entry count and byte size fit. An entry larger
// than maxBytes on its own is not stored.
func (c *responseCache) put(e *cacheEntry, ttl time.Duration, maxEntries, maxBytes int) {
	c.store(e, ttl, maxEntries, maxBytes, nil)
}

// putSince is put for an answer whose request started when the cache was
// at generation gen: it is dropped if the cache was cleared since.
func (c *responseCache) putSince(gen uint64, e *cacheEntry, ttl time.Duration, maxEntries, maxBytes int) {
	c.store(e, ttl, maxEntries, maxBytes, &gen)
}

func (c *responseCache) store(e *cacheEntry, ttl time.Duration, maxEntries, maxBytes int, gen *uint64) {
	if e.Size > maxBytes {
		return
	}
	now := time.Now()
	e.Created, e.LastHit = now, now
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != nil && *gen != c.gen {
		return
	}
	if c.entries == nil {
		c.entries = map[string]*cacheEntry{}
	}
	if old := c.entries[e.Key]; old != nil {
		c.bytes -= old.Size
		delete(c.entries, e.Key)
	}
	if len(c.entries) >= maxEntries || c.bytes+e.Size > maxBytes {
		// Make room: expired entries first (get drops them lazily, so a
		// full cache may still hold some), then the least recently used.
		// Ranking once covers a shrink of the limits that has to drop many
		// entries at a time.
		for k, o := range c.entries {
			if now.Sub(o.expiresFrom()) > ttl {
				c.bytes -= o.Size
				delete(c.entries, k)
			}
		}
		ranked := make([]*cacheEntry, 0, len(c.entries))
		for _, o := range c.entries {
			ranked = append(ranked, o)
		}
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].used < ranked[j].used })
		for _, o := range ranked {
			if len(c.entries) < maxEntries && c.bytes+e.Size <= maxBytes {
				break
			}
			c.bytes -= o.Size
			delete(c.entries, o.Key)
		}
	}
	c.seq++
	e.used = c.seq
	c.entries[e.Key] = e
	c.bytes += e.Size
	c.changed(cacheSaveDelay)
}

// clear drops every entry, in memory and on disk. It waits for a write in
// progress and bumps the generation, so a snapshot taken or a request
// started before the clear does not bring entries back.
func (c *responseCache) clear() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.entries, c.bytes = nil, 0
	c.pending = false
	if c.saveTimer != nil {
		c.saveTimer.Stop()
		c.saveTimer = nil
	}
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
// both are plain text (or both thinking), or both are fragments of the same
// tool call, and the merged piece stays small, so a long answer does not
// store thousands of one-word deltas.
func appendDelta(rec []devin.Delta, d devin.Delta) []devin.Delta {
	n := len(rec)
	if n > 0 && d.ToolCall == nil {
		last := &rec[n-1]
		if last.ToolCall == nil && len(last.Text)+len(last.Thinking) < cacheChunk &&
			((d.Thinking == "" && last.Thinking == "") || (d.Text == "" && last.Text == "")) {
			last.Text += d.Text
			last.Thinking += d.Thinking
			return rec
		}
	}
	if tc := d.ToolCall; tc != nil {
		// A fragment that only adds arguments to the call recorded last
		// (the recorded copy is ours to extend) merges into it.
		if n > 0 {
			if lt := rec[n-1].ToolCall; lt != nil && lt.Index == tc.Index && tc.ID == "" && tc.Name == "" && len(lt.Arguments) < cacheChunk {
				lt.Arguments += tc.Arguments
				return rec
			}
		}
		cp := *tc
		d.ToolCall = &cp
	}
	return append(rec, d)
}

// deltaSize approximates the bytes a stored delta holds, including its
// bookkeeping.
func deltaSize(d devin.Delta) int {
	n := cacheDeltaOverhead + len(d.Text) + len(d.Thinking)
	if d.ToolCall != nil {
		n += len(d.ToolCall.ID) + len(d.ToolCall.Name) + len(d.ToolCall.Arguments)
	}
	return n
}

// cacheable reports whether a finished answer is worth replaying: one with
// some output that the model did not refuse. An empty answer or a refusal
// is more likely a transient upstream state than the answer to the prompt,
// so a retry should reach upstream.
func cacheable(rec []devin.Delta, res *devin.ChatResult) bool {
	if len(rec) == 0 && len(res.ToolCalls) == 0 {
		return false
	}
	return finishReason(res) != "content_filter"
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
	gen := s.cache.generation()
	var rec []devin.Delta
	res, err := s.streamUpstream(ctx, req, func(d devin.Delta) error {
		rec = appendDelta(rec, d)
		return onDelta(d)
	})
	if err != nil || res == nil || !cacheable(rec, res) {
		return res, err
	}
	e := &cacheEntry{Key: key, Model: res.Model, Deltas: rec, Result: *res, Size: cacheEntryOverhead}
	if e.Model == "" {
		e.Model = devin.ResolveModel(req.Model)
	}
	for _, d := range rec {
		e.Size += deltaSize(d)
	}
	e.Result.ToolCalls = slices.Clone(res.ToolCalls)
	for _, c := range res.ToolCalls {
		e.Size += len(c.ID) + len(c.Name) + len(c.Arguments)
	}
	s.cache.putSince(gen, e, cfg.cacheTTL(), cfg.cacheMaxEntries(), cfg.cacheMaxBytes())
	return res, nil
}
