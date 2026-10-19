package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
)

const (
	// modelsTTL is how long a fetched catalog is served before refetching.
	modelsTTL = 30 * time.Minute
	// modelsRetryAfter throttles refetches after a failed attempt, so a
	// broken account doesn't turn every /v1/models call into an upstream hit.
	modelsRetryAfter = time.Minute
)

// modelCache holds the upstream model catalog. Fetches are single-flight:
// concurrent callers wait for one request instead of each hitting upstream.
type modelCache struct {
	mu          sync.Mutex
	models      []devin.ModelInfo
	fetchedAt   time.Time
	lastAttempt time.Time
	inflight    chan struct{}
}

// fetchModels returns the cached catalog, refreshing it when stale. On
// fetch failure it keeps serving the previous catalog (possibly empty).
func (s *Server) fetchModels(ctx context.Context) []devin.ModelInfo {
	c := &s.models
	c.mu.Lock()
	fresh := c.models != nil && time.Since(c.fetchedAt) < modelsTTL
	throttled := time.Since(c.lastAttempt) < modelsRetryAfter
	if fresh || (throttled && c.inflight == nil) {
		m := c.models
		c.mu.Unlock()
		return m
	}
	if wait := c.inflight; wait != nil {
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		c.mu.Lock()
		m := c.models
		c.mu.Unlock()
		return m
	}
	done := make(chan struct{})
	c.inflight = done
	c.lastAttempt = time.Now()
	c.mu.Unlock()

	fetched := s.fetchModelsUpstream(ctx)

	c.mu.Lock()
	if fetched != nil {
		c.models, c.fetchedAt = fetched, time.Now()
	}
	c.inflight = nil
	m := c.models
	c.mu.Unlock()
	close(done)
	return m
}

// fetchModelsUpstream merges the catalogs of all usable accounts: accounts
// on different plans may see different models, and the unknown-model check
// must not reject a model that only some accounts can use. The fetch is
// detached from the caller's context so one client disconnecting cannot fail
// the shared fetch (and trigger the retry throttle) for everyone.
func (s *Server) fetchModelsUpstream(ctx context.Context) []devin.ModelInfo {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	var merged []devin.ModelInfo
	seen := make(map[string]bool)
	for _, acc := range s.usableAccounts() {
		models, err := s.fetchModelsFor(ctx, acc.Tokens.SessionToken)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		for _, m := range models {
			if k := strings.ToLower(m.UID); !seen[k] {
				seen[k] = true
				merged = append(merged, m)
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// cachedModels returns the catalog without fetching (nil if never loaded).
func (s *Server) cachedModels() []devin.ModelInfo {
	s.models.mu.Lock()
	defer s.models.mu.Unlock()
	return s.models.models
}
