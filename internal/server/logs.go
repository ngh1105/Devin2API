package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
)

// requestLogSize is how many recent chat requests the panel can show.
const requestLogSize = 200

// LogEntry records one chat request. It holds metadata only: no prompt,
// output or credential ever goes into the log.
type LogEntry struct {
	Time          time.Time `json:"time"`
	Endpoint      string    `json:"endpoint"`
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

// requestLog is a fixed-size ring of recent entries.
type requestLog struct {
	mu      sync.Mutex
	entries []LogEntry
	next    int
	full    bool
}

func (l *requestLog) add(e LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make([]LogEntry, requestLogSize)
	}
	l.entries[l.next] = e
	l.next = (l.next + 1) % requestLogSize
	if l.next == 0 {
		l.full = true
	}
}

// recent returns up to limit entries, newest first.
func (l *requestLog) recent(limit int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	if e.Status != http.StatusOK {
		s.stats.failed.Add(1)
	}
	s.reqLog.add(e)

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

// handleLogs serves GET /admin/api/logs?limit=N (newest first).
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, http.StatusOK, s.reqLog.recent(limit))
}
