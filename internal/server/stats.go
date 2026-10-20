package server

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ngh1105/Devin2API/internal/store"
)

// statsHours is how many hours of per-hour counts are kept.
const statsHours = 7 * 24

// hourStat counts the chat requests that finished in one hour.
type hourStat struct {
	Hour         int64 `json:"hour"` // unix time of the hour's start
	Requests     int64 `json:"requests"`
	Failed       int64 `json:"failed"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// Cached counts requests answered from the response cache; their tokens
	// were not spent upstream and are counted here instead of above.
	Cached            int64 `json:"cached,omitempty"`
	SavedInputTokens  int64 `json:"saved_input_tokens,omitempty"`
	SavedOutputTokens int64 `json:"saved_output_tokens,omitempty"`
}

// statsSaveDelay batches counter updates into one write: a save is
// scheduled on the first change and covers every change made before it.
const statsSaveDelay = 2 * time.Second

// chatStats counts chat requests and failures and persists them to
// <data>/stats.json, so the panel totals survive restarts.
type chatStats struct {
	requests, failed atomic.Int64
	since            time.Time // when counting started (first run)
	// cacheHits counts requests answered from the response cache; savedIn
	// and savedOut total the tokens those answers cost when first made.
	cacheHits, savedIn, savedOut atomic.Int64

	path    string // "" disables persistence (tests)
	mu      sync.Mutex
	pending bool
	writeMu sync.Mutex // one file write at a time

	hourMu sync.Mutex
	hours  map[int64]*hourStat // by Hour
}

type statsFile struct {
	Requests          int64      `json:"requests"`
	Failed            int64      `json:"failed"`
	CacheHits         int64      `json:"cache_hits,omitempty"`
	SavedInputTokens  int64      `json:"saved_input_tokens,omitempty"`
	SavedOutputTokens int64      `json:"saved_output_tokens,omitempty"`
	Since             time.Time  `json:"since"`
	Hours             []hourStat `json:"hours,omitempty"`
}

// load restores counters from dataDir; a missing or unreadable file starts
// from zero.
func (c *chatStats) load(dataDir string) {
	c.since = time.Now()
	if dataDir == "" {
		return
	}
	c.path = filepath.Join(dataDir, "stats.json")
	b, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var f statsFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	c.requests.Store(f.Requests)
	c.failed.Store(f.Failed)
	c.cacheHits.Store(f.CacheHits)
	c.savedIn.Store(f.SavedInputTokens)
	c.savedOut.Store(f.SavedOutputTokens)
	c.hourMu.Lock()
	c.hours = make(map[int64]*hourStat, len(f.Hours))
	for _, h := range f.Hours {
		c.hours[h.Hour] = &h
	}
	c.hourMu.Unlock()
	if !f.Since.IsZero() {
		c.since = f.Since
	}
}

// changed schedules a save unless one is already pending.
func (c *chatStats) changed() {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending {
		return
	}
	c.pending = true
	time.AfterFunc(statsSaveDelay, c.save)
}

func (c *chatStats) save() {
	c.mu.Lock()
	c.pending = false
	c.mu.Unlock()
	// A save scheduled while this one writes must not race it on the same
	// temp file.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	b, _ := json.MarshalIndent(statsFile{
		Requests: c.requests.Load(), Failed: c.failed.Load(), Since: c.since,
		CacheHits: c.cacheHits.Load(), SavedInputTokens: c.savedIn.Load(), SavedOutputTokens: c.savedOut.Load(),
		Hours: c.hourly(statsHours, time.Now(), true),
	}, "", "  ")
	if err := store.WriteFileAtomic(c.path, b, 0o600); err != nil {
		slog.Warn("save stats", "error", err)
	}
}

// record adds a finished request to its hour and drops hours older than
// statsHours.
func (c *chatStats) record(e LogEntry) {
	hour := e.Time.Truncate(time.Hour).Unix()
	oldest := time.Now().Truncate(time.Hour).Unix() - (statsHours-1)*3600
	if hour < oldest {
		return
	}
	c.hourMu.Lock()
	defer c.hourMu.Unlock()
	if c.hours == nil {
		c.hours = map[int64]*hourStat{}
	}
	h := c.hours[hour]
	if h == nil {
		h = &hourStat{Hour: hour}
		c.hours[hour] = h
		for k := range c.hours {
			if k < oldest {
				delete(c.hours, k)
			}
		}
	}
	h.Requests++
	if e.Status != 200 {
		h.Failed++
	}
	if e.Cached {
		h.Cached++
		h.SavedInputTokens += int64(e.InputTokens)
		h.SavedOutputTokens += int64(e.OutputTokens)
		return
	}
	h.InputTokens += int64(e.InputTokens)
	h.OutputTokens += int64(e.OutputTokens)
}

// hourly returns the n hours up to and including now's, oldest first;
// sparse leaves out hours without requests.
func (c *chatStats) hourly(n int, now time.Time, sparse bool) []hourStat {
	c.hourMu.Lock()
	defer c.hourMu.Unlock()
	last := now.Truncate(time.Hour).Unix()
	out := make([]hourStat, 0, n)
	for i := n - 1; i >= 0; i-- {
		hour := last - int64(i)*3600
		if h := c.hours[hour]; h != nil {
			out = append(out, *h)
		} else if !sparse {
			out = append(out, hourStat{Hour: hour})
		}
	}
	return out
}
