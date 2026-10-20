package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

// requestLogSize is how many recent chat requests the panel can show.
const requestLogSize = 1000

// LogEntry records one chat request. It holds metadata only: no prompt,
// output or credential ever goes into the log.
type LogEntry struct {
	Time          time.Time `json:"time"`
	Endpoint      string    `json:"endpoint"`
	Key           string    `json:"key,omitempty"`  // API key name (or masked value) that made the request
	Model         string    `json:"model"`          // as requested
	UpstreamModel string    `json:"upstream_model"` // catalog UID sent upstream
	Stream        bool      `json:"stream"`
	Account       string    `json:"account,omitempty"` // account id that answered (or last tried)
	AccountLabel  string    `json:"account_label,omitempty"`
	Attempts      int       `json:"attempts"` // accounts tried, >1 after failover
	Status        int       `json:"status"`
	Error         string    `json:"error,omitempty"`
	DurationMs    int64     `json:"duration_ms"`
	FirstOutputMs int64     `json:"first_output_ms,omitempty"`
	InputTokens   int       `json:"input_tokens,omitempty"`
	OutputTokens  int       `json:"output_tokens,omitempty"`
	ToolCalls     int       `json:"tool_calls,omitempty"`
	Finish        string    `json:"finish,omitempty"`
}

// requestLog is a fixed-size ring of recent entries, also appended to
// <data>/logs.jsonl so they survive restarts. The file is compacted to the
// ring's contents once it holds twice as many lines.
type requestLog struct {
	mu      sync.Mutex
	entries []LogEntry
	next    int
	full    bool
	path    string // "" keeps the log in memory only (tests)
	lines   int    // lines in the file
}

// load restores the newest entries from dataDir; a missing file starts
// empty and unreadable lines are skipped.
func (l *requestLog) load(dataDir string) {
	if dataDir == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.path = filepath.Join(dataDir, "logs.jsonl")
	f, err := os.Open(l.path)
	if err != nil {
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l.lines++
		var e LogEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			l.push(e)
		}
	}
	// Close before compact may rename over the file: Windows refuses to
	// replace a file that is still open.
	f.Close()
	if err := sc.Err(); err != nil {
		// An over-long or unreadable line stops the scan; the rest of the
		// file is unknown, so rewrite it from what was read.
		slog.Warn("request log damaged, rewriting it", "file", l.path, "error", err)
		l.compact()
		return
	}
	if l.lines > 2*requestLogSize {
		l.compact()
	}
}

func (l *requestLog) add(e LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.push(e)
	if l.path == "" {
		return
	}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
		f.Close()
	}
	if err != nil {
		slog.Warn("write request log", "error", err)
		return
	}
	l.lines++
	if l.lines > 2*requestLogSize {
		l.compact()
	}
}

// push stores e in the ring. Caller must hold l.mu.
func (l *requestLog) push(e LogEntry) {
	if l.entries == nil {
		l.entries = make([]LogEntry, requestLogSize)
	}
	l.entries[l.next] = e
	l.next = (l.next + 1) % requestLogSize
	if l.next == 0 {
		l.full = true
	}
}

// compact rewrites the file with the ring's entries, oldest first.
// Caller must hold l.mu.
func (l *requestLog) compact() {
	recent := l.recentLocked(0)
	var buf []byte
	for i := len(recent) - 1; i >= 0; i-- {
		b, _ := json.Marshal(recent[i])
		buf = append(append(buf, b...), '\n')
	}
	if err := store.WriteFileAtomic(l.path, buf, 0o600); err != nil {
		slog.Warn("compact request log", "error", err)
		return
	}
	l.lines = len(recent)
}

// clear drops every entry, in memory and on disk.
func (l *requestLog) clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries, l.next, l.full, l.lines = nil, 0, false, 0
	if l.path == "" {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// recent returns up to limit entries, newest first.
func (l *requestLog) recent(limit int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recentLocked(limit)
}

// recentLocked is recent for callers holding l.mu.
func (l *requestLog) recentLocked(limit int) []LogEntry {
	n := l.next
	if l.full {
		n = requestLogSize
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]LogEntry, 0, limit)
	for i := 1; i <= limit; i++ {
		out = append(out, l.entries[(l.next-i+requestLogSize)%requestLogSize])
	}
	return out
}

// chatTrace follows one chat request from the handler into the failover
// loop, which records the accounts it tries and when output starts.
type chatTrace struct {
	start    time.Time
	entry    LogEntry
	firstOut time.Time
}

type traceKey struct{}

func traceFrom(ctx context.Context) *chatTrace {
	tr, _ := ctx.Value(traceKey{}).(*chatTrace)
	return tr
}

// startTrace begins logging a chat request that is about to go upstream.
func (s *Server) startTrace(r *http.Request, endpoint, model string, dreq devin.ChatRequest, stream bool) (context.Context, *chatTrace) {
	s.stats.requests.Add(1)
	if model == "" {
		model = "devin"
	}
	tr := &chatTrace{start: time.Now(), entry: LogEntry{
		Endpoint: endpoint, Model: model, UpstreamModel: devin.ResolveModel(dreq.Model), Stream: stream,
	}}
	if k, ok := apiKeyFrom(r.Context()); ok {
		tr.entry.Key = keyLabel(k)
	}
	keepCharge(r.Context(), true)
	return context.WithValue(r.Context(), traceKey{}, tr), tr
}

// attempt records that the failover loop is trying an account.
func (tr *chatTrace) attempt(id, label string) {
	if tr == nil {
		return
	}
	tr.entry.Attempts++
	tr.entry.Account, tr.entry.AccountLabel = id, label
}

// output records the first streamed output.
func (tr *chatTrace) output() {
	if tr != nil && tr.firstOut.IsZero() {
		tr.firstOut = time.Now()
	}
}

// finishTrace completes the entry, counts failures, stores the entry and
// writes one console line.
func (s *Server) finishTrace(ctx context.Context, tr *chatTrace, res *devin.ChatResult, err error) {
	e := tr.entry
	e.Time = tr.start
	e.DurationMs = time.Since(tr.start).Milliseconds()
	if !tr.firstOut.IsZero() {
		e.FirstOutputMs = tr.firstOut.Sub(tr.start).Milliseconds()
	}
	if res != nil {
		e.InputTokens, e.OutputTokens = res.PromptTokens, res.CompletionTokens
		e.ToolCalls = len(res.ToolCalls)
		if err == nil {
			e.Finish = finishReason(res)
		}
	}
	switch {
	case err == nil:
		e.Status = http.StatusOK
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
		e.Status, e.Error = 499, "client closed the request"
	default:
		e.Status, e.Error = upstreamStatus(err)
	}
	if errors.Is(err, errNoAccount) {
		keepCharge(ctx, false) // never reached upstream
	}
	if e.Status != http.StatusOK {
		s.stats.failed.Add(1)
	}
	s.stats.record(e)
	s.reqLog.add(e)
	s.stats.changed() // covers this request's count from startTrace too

	attrs := []any{
		"endpoint", e.Endpoint, "model", e.UpstreamModel, "stream", e.Stream,
		"account", e.Account, "attempts", e.Attempts, "status", e.Status,
		"ms", e.DurationMs, "first_ms", e.FirstOutputMs,
		"in", e.InputTokens, "out", e.OutputTokens,
	}
	if e.ToolCalls > 0 {
		attrs = append(attrs, "tool_calls", e.ToolCalls)
	}
	if e.Error != "" {
		slog.Warn("chat failed", append(attrs, "error", e.Error)...)
		return
	}
	slog.Info("chat", append(attrs, "finish", e.Finish)...)
}

// handleLogs serves GET /admin/api/logs?limit=N (newest first) and
// DELETE, which clears the log.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		writeJSON(w, http.StatusOK, s.reqLog.recent(limit))
	case http.MethodDelete:
		if err := s.reqLog.clear(); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to clear the request log")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
