package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

// TestResponseStoreByteCap: histories with large images must not pile up
// past the size budget, and an oversized one is not kept at all.
func TestResponseStoreByteCap(t *testing.T) {
	var s responseStore
	img := strings.Repeat("A", responseStoreBytes/4)
	big := []devin.Prompt{{Source: devin.SourceUser, Images: []devin.Image{{MimeType: "image/png", Base64: img}}}}
	for i := 0; i < 10; i++ {
		s.put(fmt.Sprintf("resp_%d", i), big)
	}
	if s.bytes > responseStoreBytes {
		t.Errorf("stored %d bytes, budget %d", s.bytes, responseStoreBytes)
	}
	if _, ok := s.get("resp_9"); !ok {
		t.Error("newest entry evicted")
	}
	if _, ok := s.get("resp_0"); ok {
		t.Error("oldest entry kept past the budget")
	}
	huge := []devin.Prompt{{Source: devin.SourceUser, Text: strings.Repeat("x", responseStoreBytes+1)}}
	s.put("resp_huge", huge)
	if _, ok := s.get("resp_huge"); ok {
		t.Error("history larger than the budget was kept")
	}
}

// TestLoginLimitIsPerIP: one client exhausting its attempts does not lock
// out another.
func TestLoginLimitIsPerIP(t *testing.T) {
	srv := passwordServer(t)
	from := func(ip string) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = ip + ":40000" }
	}
	for i := 0; i < loginFailsPerMinute; i++ {
		adminDo(srv, http.MethodPost, "/admin/api/login", `{"password":"nope"}`, from("10.0.0.9"))
	}
	if rec := adminDo(srv, http.MethodPost, "/admin/api/login", `{"password":"s3cret"}`, from("10.0.0.9")); rec.Code != http.StatusTooManyRequests {
		t.Errorf("attacker IP: %d, want 429", rec.Code)
	}
	if rec := adminDo(srv, http.MethodPost, "/admin/api/login", `{"password":"s3cret"}`, from("10.0.0.2")); rec.Code != http.StatusOK {
		t.Errorf("other IP: %d, want 200", rec.Code)
	}
}

func TestQuotaResetAtUsesExhaustedWindow(t *testing.T) {
	now := time.Now()
	day, week := now.Add(time.Hour), now.Add(72*time.Hour)
	weeklyOut := &store.Account{Usage: &store.Usage{DailyRemainingPercent: 60, WeeklyRemainingPercent: 0, DailyResetAt: day, WeeklyResetAt: week}}
	if got := quotaResetAt(weeklyOut, now); !got.Equal(week) {
		t.Errorf("weekly exhausted: ready %v, want weekly reset", got)
	}
	dailyOut := &store.Account{Usage: &store.Usage{DailyRemainingPercent: 0, WeeklyRemainingPercent: 40, DailyResetAt: day, WeeklyResetAt: week}}
	if got := quotaResetAt(dailyOut, now); !got.Equal(day) {
		t.Errorf("daily exhausted: ready %v, want daily reset", got)
	}
}

func TestStopLimitsPerAPI(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	five := `["a","b","c","d","e"]`
	if rec := postChat(srv, `{"messages":[{"role":"user","content":"hi"}],"stop":`+five+`}`); rec.Code != http.StatusBadRequest {
		t.Errorf("OpenAI 5 stops: %d, want 400", rec.Code)
	}
	if rec := postV1(srv, "/v1/messages", `{"max_tokens":5,"messages":[{"role":"user","content":"hi"}],"stop_sequences":`+five+`}`); rec.Code != http.StatusOK {
		t.Errorf("Anthropic 5 stop_sequences: %d, want 200", rec.Code)
	}
}

// TestStatsConcurrentSaves: overlapping saves must not fail on the shared
// temp file.
func TestStatsConcurrentSaves(t *testing.T) {
	var c chatStats
	c.load(t.TempDir())
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() { c.save(); done <- struct{}{} }()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	var r chatStats
	r.load(strings.TrimSuffix(c.path, "stats.json"))
	if r.since.IsZero() {
		t.Error("stats file not readable after concurrent saves")
	}
}

// TestLoginLimitHoldsUnderParallelRequests: a burst of parallel wrong
// logins from one IP gets at most loginFailsPerMinute password checks.
func TestLoginLimitHoldsUnderParallelRequests(t *testing.T) {
	srv := passwordServer(t)
	codes := make(chan int, 40)
	for i := 0; i < 40; i++ {
		go func() {
			rec := adminDo(srv, http.MethodPost, "/admin/api/login", `{"password":"nope"}`, func(r *http.Request) { r.RemoteAddr = "10.1.1.1:5000" })
			codes <- rec.Code
		}()
	}
	checked := 0
	for i := 0; i < 40; i++ {
		if <-codes == http.StatusUnauthorized {
			checked++
		}
	}
	if checked > loginFailsPerMinute {
		t.Errorf("%d password checks, cap %d", checked, loginFailsPerMinute)
	}
}
