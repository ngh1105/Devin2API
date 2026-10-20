package server

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

const (
	// usageRefreshEvery is how often the background loop refreshes quota.
	usageRefreshEvery = 10 * time.Minute
	// usageRefreshDelay lets the server start before the first refresh.
	usageRefreshDelay = 5 * time.Second

	// defaultCooldown applies after a 429 whose message states no wait.
	defaultCooldown = time.Minute
	// maxCooldown caps a stated wait, so a misparsed message cannot park
	// an account for long.
	maxCooldown = 6 * time.Hour

	tokenExpiredMsg = "token expired or revoked"
)

// refreshUsage fetches an account's quota and stores it. A rejected token
// is recorded on the account and returned as devin.ErrTokenExpired.
func (s *Server) refreshUsage(ctx context.Context, id string) (*store.Account, error) {
	acct, err := s.accounts.Get(id)
	if err != nil {
		return nil, err
	}
	token := acct.Tokens.SessionToken
	usage, err := s.fetchUserStatus(ctx, token)
	// A re-login during the fetch replaces the token; the result then
	// describes the old one and must not be written over the new account.
	if errors.Is(err, devin.ErrTokenExpired) {
		s.accounts.Mutate(id, func(a *store.Account) {
			if a.Tokens.SessionToken != token {
				return
			}
			a.UsageErr = tokenExpiredMsg
			a.Usage = nil
		})
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if usage != nil {
		usage.CheckedAt = time.Now().Unix()
	}
	return s.accounts.Mutate(id, func(a *store.Account) {
		if a.Tokens.SessionToken != token {
			return
		}
		a.Usage = usage
		a.UsageErr = ""
	})
}

// refreshAllUsage refreshes every usable account, one at a time.
func (s *Server) refreshAllUsage(ctx context.Context) {
	for _, acc := range s.usableAccounts() {
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := s.refreshUsage(cctx, acc.ID)
		cancel()
		if err != nil {
			slog.Warn("usage refresh failed", "account", acc.ID, "error", err)
		}
	}
}

// usageLoop keeps quota data fresh so routing can prefer accounts with
// quota left and skip exhausted ones.
func (s *Server) usageLoop(ctx context.Context) {
	select {
	case <-time.After(usageRefreshDelay):
	case <-ctx.Done():
		return
	}
	t := time.NewTicker(usageRefreshEvery)
	defer t.Stop()
	for {
		s.refreshAllUsage(ctx)
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

// cooldowns parks an account for one model after a 429. Rate limits are
// often per model ("free model rate limit ... switch to a different
// model"), so the same account stays usable for other models.
type cooldowns struct {
	mu    sync.Mutex
	until map[string]time.Time // account id + "|" + model -> end
}

func cooldownKey(account, model string) string { return account + "|" + model }

func (c *cooldowns) set(account, model string, d time.Duration) {
	if d <= 0 {
		d = defaultCooldown
	}
	if d > maxCooldown {
		d = maxCooldown
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until == nil {
		c.until = map[string]time.Time{}
	}
	now := time.Now()
	// Sweep ended cooldowns here, so the map does not grow with every
	// account and model that was ever rate-limited.
	for k, end := range c.until {
		if now.After(end) {
			delete(c.until, k)
		}
	}
	c.until[cooldownKey(account, model)] = now.Add(d)
}

// get returns when the cooldown ends, or the zero time if none is active.
func (c *cooldowns) get(account, model string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cooldownKey(account, model)
	end, ok := c.until[k]
	if ok && time.Now().After(end) {
		delete(c.until, k)
		return time.Time{}
	}
	return end
}

// quotaExhausted reports whether the account's last known usage shows no
// daily or weekly quota left with the reset still ahead. Unknown usage
// counts as available.
func quotaExhausted(a *store.Account, now time.Time) bool {
	u := a.Usage
	if u == nil {
		return false
	}
	return (u.DailyRemainingPercent <= 0 && u.DailyResetAt.After(now)) ||
		(u.WeeklyRemainingPercent <= 0 && u.WeeklyResetAt.After(now))
}

// quotaResetAt is when an exhausted account gets quota back: the later
// reset among the windows (daily, weekly) that are empty.
func quotaResetAt(a *store.Account, now time.Time) time.Time {
	u := a.Usage
	var at time.Time
	if u.DailyRemainingPercent <= 0 && u.DailyResetAt.After(now) {
		at = u.DailyResetAt
	}
	if u.WeeklyRemainingPercent <= 0 && u.WeeklyResetAt.After(at) {
		at = u.WeeklyResetAt
	}
	return at
}

// quotaBand buckets the remaining quota into tenths (0..10), using the
// tighter of the daily and weekly limits. Unknown usage ranks highest so
// fresh accounts get tried and their usage gets learned.
func quotaBand(a *store.Account) int {
	u := a.Usage
	if u == nil {
		return 10
	}
	left := min(u.DailyRemainingPercent, u.WeeklyRemainingPercent)
	return int(max(0, min(left, 100)) / 10)
}

// chatAccounts orders the usable accounts for one chat request on model.
// Healthy accounts come first, the higher quota band first and round-robin
// within a band. Accounts cooling down for this model, out of quota or with
// a rejected token follow as a last resort, soonest-available first.
func (s *Server) chatAccounts(model string) []*store.Account {
	usable := s.usableAccounts()
	if len(usable) == 0 {
		return nil
	}
	// Rotate first so equal-band accounts take turns.
	start := int(s.chatRR.Add(1)-1) % len(usable)
	rotated := append(usable[start:len(usable):len(usable)], usable[:start]...)

	now := time.Now()
	type ranked struct {
		acc      *store.Account
		band     int
		degraded bool
		readyAt  time.Time
	}
	rs := make([]ranked, len(rotated))
	for i, a := range rotated {
		r := ranked{acc: a, band: quotaBand(a)}
		if end := s.cooldown.get(a.ID, model); !end.IsZero() {
			r.degraded, r.readyAt = true, end
		}
		if quotaExhausted(a, now) {
			r.degraded = true
			if reset := quotaResetAt(a, now); reset.After(r.readyAt) {
				r.readyAt = reset
			}
		}
		if a.UsageErr == tokenExpiredMsg {
			r.degraded = true
			r.readyAt = now.Add(maxCooldown)
		}
		rs[i] = r
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].degraded != rs[j].degraded {
			return !rs[i].degraded
		}
		if rs[i].degraded {
			return rs[i].readyAt.Before(rs[j].readyAt)
		}
		return rs[i].band > rs[j].band
	})
	out := make([]*store.Account, len(rs))
	for i, r := range rs {
		out[i] = r.acc
	}
	return out
}
