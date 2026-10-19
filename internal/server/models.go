package server

import (
	"context"
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

// fetchModelsUpstream asks each usable account in turn until one answers.
func (s *Server) fetchModelsUpstream(ctx context.Context) []devin.ModelInfo {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, acc := range s.chatAccounts() {
		models, err := devin.FetchModels(ctx, s.loginServerBase, acc.Tokens.SessionToken)
		if err == nil && len(models) > 0 {
			return models
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil
}

// cachedModels returns the catalog without fetching (nil if never loaded).
func (s *Server) cachedModels() []devin.ModelInfo {
	s.models.mu.Lock()
	defer s.models.mu.Unlock()
	return s.models.models
}
