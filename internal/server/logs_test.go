package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestLogRing(t *testing.T) {
	var l requestLog
	for i := 0; i < requestLogSize+5; i++ {
		l.add(LogEntry{Status: i})
	}
	got := l.recent(0)
	if len(got) != requestLogSize || got[0].Status != requestLogSize+4 || got[len(got)-1].Status != 5 {
		t.Fatalf("len %d first %d last %d", len(got), got[0].Status, got[len(got)-1].Status)
	}
	if n := len(l.recent(3)); n != 3 {
		t.Errorf("limit 3 gave %d", n)
	}
	var empty requestLog
	if got := empty.recent(10); len(got) != 0 {
		t.Errorf("empty log gave %d", len(got))
	}
}

func getLogs(t *testing.T, srv *Server) []LogEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodGet, "/admin/api/logs", nil))
	var out []LogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("logs: %s", rec.Body)
	}
	return out
}

// TestLogsRecordChat: each endpoint logs metadata (never content), the
// failover attempt count and the failing status.
func TestLogsRecordChat(t *testing.T) {
	fake := &fakeDevin{trailers: map[string]string{
		"eyJdead": `{"error":{"code":"unauthenticated","message":"bad token"}}`,
	}}
	srv := chatTestServer(t, fake, "eyJdead", "eyJlive")
	postChat(srv, `{"model":"devin","messages":[{"role":"user","content":"secret prompt text"}]}`)
	postV1(srv, "/v1/messages", `{"max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	postV1(srv, "/v1/responses", `{"input":"hi"}`)

	logs := getLogs(t, srv)
	if len(logs) != 3 {
		t.Fatalf("got %d entries", len(logs))
	}
	if logs[0].Endpoint != "/v1/responses" || logs[1].Endpoint != "/v1/messages" || logs[2].Endpoint != "/v1/chat/completions" {
		t.Errorf("order = %s, %s, %s", logs[0].Endpoint, logs[1].Endpoint, logs[2].Endpoint)
	}
	for _, e := range logs {
		if e.Status != 200 || e.Account == "" || e.InputTokens != 10 || e.OutputTokens != 2 || e.Finish != "stop" || e.UpstreamModel != "swe-2-high" {
			t.Errorf("entry = %+v", e)
		}
	}
	if !logs[1].Stream {
		t.Error("stream flag not recorded")
	}
	// Round-robin hits the dead account at least once across the three calls.
	failovers := 0
	for _, e := range logs {
		if e.Attempts == 2 {
			failovers++
		}
	}
	if failovers == 0 {
		t.Error("no entry recorded a failover attempt")
	}
	raw, _ := json.Marshal(logs)
	if strings.Contains(string(raw), "secret prompt") || strings.Contains(string(raw), "eyJ") {
		t.Error("log leaked prompt text or a session token")
	}
}

func TestLogsRecordFailure(t *testing.T) {
	limit := `{"error":{"code":"resource_exhausted","message":"Your limit will reset in 2 minutes."}}`
	srv := chatTestServer(t, &fakeDevin{trailers: map[string]string{"eyJrl": limit}}, "eyJrl")
	postChat(srv, `{"messages":[{"role":"user","content":"hi"}]}`)
	logs := getLogs(t, srv)
	if len(logs) != 1 || logs[0].Status != 429 || !strings.Contains(logs[0].Error, "reset in 2 minutes") {
		t.Errorf("logs = %+v", logs)
	}
}

// TestStatsPersist: counters are saved to <data>/stats.json and restored
// by the next server on the same data dir.
func TestStatsPersist(t *testing.T) {
	dir := t.TempDir()
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	srv.stats.load(dir) // chatTestServer has no data dir; point it at one
	postChat(srv, `{"messages":[{"role":"user","content":"hi"}]}`)
	srv.stats.failed.Add(1) // as if a second request had failed
	srv.stats.save()

	var c chatStats
	c.load(dir)
	if c.requests.Load() != 1 || c.failed.Load() != 1 || !c.since.Equal(srv.stats.since) {
		t.Errorf("reloaded requests=%d failed=%d since=%v", c.requests.Load(), c.failed.Load(), c.since)
	}
}

// TestStatsSaveIsBatched: many changes schedule one delayed write.
func TestStatsSaveIsBatched(t *testing.T) {
	dir := t.TempDir()
	var c chatStats
	c.load(dir)
	for i := 0; i < 50; i++ {
		c.requests.Add(1)
		c.changed()
	}
	if _, err := os.Stat(filepath.Join(dir, "stats.json")); err == nil {
		t.Fatal("saved before the batching delay")
	}
	time.Sleep(statsSaveDelay + 500*time.Millisecond)
	var r chatStats
	r.load(dir)
	if r.requests.Load() != 50 {
		t.Errorf("reloaded %d, want 50", r.requests.Load())
	}
}
