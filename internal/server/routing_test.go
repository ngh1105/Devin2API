package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/store"
)

func usage(daily, weekly int64, reset time.Time) *store.Usage {
	return &store.Usage{DailyRemainingPercent: daily, WeeklyRemainingPercent: weekly, DailyResetAt: reset, WeeklyResetAt: reset}
}

func TestQuotaHelpers(t *testing.T) {
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	cases := []struct {
		u         *store.Usage
		band      int
		exhausted bool
	}{
		{nil, 10, false},
		{usage(100, 100, future), 10, false},
		{usage(95, 35, future), 3, false}, // weekly is the tighter limit
		{usage(0, 50, future), 0, true},
		{usage(0, 50, past), 0, false}, // reset already passed: not known to be empty
	}
	for i, c := range cases {
		a := &store.Account{Usage: c.u}
		if b := quotaBand(a); b != c.band {
			t.Errorf("case %d: band %d, want %d", i, b, c.band)
		}
		if e := quotaExhausted(a, time.Now()); e != c.exhausted {
			t.Errorf("case %d: exhausted %v, want %v", i, e, c.exhausted)
		}
	}
}

func TestChatAccountsOrder(t *testing.T) {
	accs := mustAccounts(t)
	future := time.Now().Add(time.Hour)
	add := func(id string, u *store.Usage, usageErr string) {
		a := &store.Account{Schema: 2, ID: id, Enabled: true, CreatedAt: time.Now(), Usage: u, UsageErr: usageErr}
		a.Tokens.SessionToken = "devin-session-token$" + id
		if err := accs.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	add("acc_low", usage(30, 80, future), "")
	add("acc_dead", nil, tokenExpiredMsg)
	add("acc_empty", usage(0, 80, future), "")
	add("acc_high", usage(90, 95, future), "")
	add("acc_cool", usage(100, 100, future), "")
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.cooldown.set("acc_cool", "swe-2-medium", time.Minute)

	// Same order whatever the round-robin position.
	for i := 0; i < 5; i++ {
		var ids []string
		for _, a := range srv.chatAccounts("swe-2-medium") {
			ids = append(ids, a.ID)
		}
		if got := strings.Join(ids, ","); got != "acc_high,acc_low,acc_cool,acc_empty,acc_dead" {
			t.Fatalf("call %d: order %s", i, got)
		}
	}
	// The cooldown is per model: another model ranks acc_cool first (100%).
	if first := srv.chatAccounts("gpt-5-4-medium")[0].ID; first != "acc_cool" {
		t.Errorf("other model: first %s, want acc_cool", first)
	}
}

func TestChatAccountsRoundRobinWithinBand(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJa", "eyJb")
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		seen[srv.chatAccounts("m")[0].ID]++
	}
	if len(seen) != 2 || seen[srv.chatAccounts("m")[0].ID] != 2 {
		t.Errorf("first-choice counts = %v", seen)
	}
}

// TestRateLimitCooldown: after a 429 stating a wait, later requests for the
// same model start on another account instead of hitting the limited one.
func TestRateLimitCooldown(t *testing.T) {
	limit := `{"error":{"code":"resource_exhausted","message":"Reached free model rate limit. Your limit will reset in 2 minutes."}}`
	fake := &fakeDevin{trailers: map[string]string{"eyJlimited": limit}}
	srv := chatTestServer(t, fake, "eyJlimited", "eyJok")
	for i := 0; i < 4; i++ {
		if rec := postChat(srv, `{"messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	limitedCalls := 0
	for _, auth := range fake.calls {
		if strings.Contains(auth, "eyJlimited") {
			limitedCalls++
		}
	}
	if limitedCalls != 1 {
		t.Errorf("limited account called %d times, want 1", limitedCalls)
	}
	var limitedID string
	for _, a := range srv.usableAccounts() {
		if strings.Contains(a.Tokens.SessionToken, "eyJlimited") {
			limitedID = a.ID
		}
	}
	end := srv.cooldown.get(limitedID, "swe-2-high")
	if d := time.Until(end); d < 110*time.Second || d > 2*time.Minute {
		t.Errorf("cooldown ends in %v, want ~2m", d)
	}
}

func TestCooldownBounds(t *testing.T) {
	var c cooldowns
	c.set("a", "m", 0)
	if d := time.Until(c.get("a", "m")); d <= 0 || d > defaultCooldown {
		t.Errorf("default cooldown %v", d)
	}
	c.set("a", "m", 100*time.Hour)
	if d := time.Until(c.get("a", "m")); d > maxCooldown {
		t.Errorf("cooldown not capped: %v", d)
	}
	c.set("b", "m", time.Nanosecond)
	time.Sleep(time.Millisecond)
	if !c.get("b", "m").IsZero() {
		t.Error("expired cooldown still active")
	}
}

// TestRefreshAllUsageMarksExpiredToken: the background refresh flags an
// account whose token upstream rejects, which then ranks last.
func TestRefreshAllUsageMarksExpiredToken(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if strings.Contains(r.Header.Get("Authorization"), "eyJdead") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/proto")
	}))
	defer up.Close()
	accs := mustAccounts(t)
	for i, tok := range []string{"eyJdead", "eyJlive"} {
		accs.Save(store.AccountFromLogin("u", "usr_"+string(rune('a'+i)), "org", tok, "", nil))
	}
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.loginServerBase = up.URL
	srv.refreshAllUsage(context.Background())

	for _, a := range srv.usableAccounts() {
		dead := strings.Contains(a.Tokens.SessionToken, "eyJdead")
		if dead != (a.UsageErr == tokenExpiredMsg) {
			t.Errorf("%s: usage_error %q", a.ID, a.UsageErr)
		}
	}
	for i := 0; i < 3; i++ {
		order := srv.chatAccounts("m")
		if !strings.Contains(order[len(order)-1].Tokens.SessionToken, "eyJdead") {
			t.Fatal("expired account not ranked last")
		}
	}
}
