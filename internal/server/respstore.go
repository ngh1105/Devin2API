package server

import (
	"context"
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
)

// Default bounds of the responses kept for previous_response_id. They live
// in memory only, so a restart forgets them; clients can always send the
// full history instead. Histories can carry base64 images (up to 32 MB per
// request), so the store is capped by total size as well as by count. The
// server configuration (DEVIN2API_RESPONSES_*) overrides them.
const (
	responseStoreSize  = 1024
	responseStoreBytes = 64 << 20
	responseTTL        = 24 * time.Hour
)

// responseStore maps a response id to the conversation it ended: the input
// prompts (including any earlier turns) plus the assistant's reply. Each
// entry remembers the API key that made it, so a follow-up from another
// key does not find it.
type responseStore struct {
	mu    sync.Mutex
	items map[string]storedResponse
	order []string // insertion order, oldest first
	bytes int      // total size of stored histories

	// Bounds; zero means the default above.
	maxItems int
	maxBytes int
	ttl      time.Duration
}

// conversation is what a stored response continues: the system text from
// input items and the prompts, ending with the assistant's reply.
type conversation struct {
	system  string
	prompts []devin.Prompt
}

type storedResponse struct {
	conv  conversation
	owner string // API key id, "" in keyless mode
	at    time.Time
	size  int
}

// configure sets the bounds; zero values keep the defaults.
func (s *responseStore) configure(keep int, ttl time.Duration, mb int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxItems, s.ttl, s.maxBytes = keep, ttl, mb<<20
}

func (s *responseStore) limits() (items, bytes int, ttl time.Duration) {
	items, bytes, ttl = s.maxItems, s.maxBytes, s.ttl
	if items <= 0 {
		items = responseStoreSize
	}
	if bytes <= 0 {
		bytes = responseStoreBytes
	}
	if ttl <= 0 {
		ttl = responseTTL
	}
	return items, bytes, ttl
}

// responseOwner identifies the caller a stored response belongs to: the API
// key id, or "" when /v1 runs without keys.
func responseOwner(ctx context.Context) string {
	if k, ok := apiKeyFrom(ctx); ok {
		return k.ID
	}
	return ""
}

// promptsSize approximates a history's memory use by its text payloads.
func promptsSize(prompts []devin.Prompt) int {
	n := 0
	for _, p := range prompts {
		n += len(p.Text) + len(p.ToolCallID)
		for _, tc := range p.ToolCalls {
			n += len(tc.ID) + len(tc.Name) + len(tc.Arguments)
		}
		for _, img := range p.Images {
			n += len(img.Base64)
		}
	}
	return n
}

// put stores a history for owner; one larger than the whole budget is not
// kept.
func (s *responseStore) put(id, owner string, conv conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maxItems, maxBytes, ttl := s.limits()
	size := len(conv.system) + promptsSize(conv.prompts)
	if size > maxBytes {
		return
	}
	if s.items == nil {
		s.items = map[string]storedResponse{}
	}
	s.items[id] = storedResponse{conv: conv, owner: owner, at: time.Now(), size: size}
	s.order = append(s.order, id)
	s.bytes += size
	// Drop expired entries and the oldest beyond the count or size cap.
	cutoff := time.Now().Add(-ttl)
	for len(s.order) > 0 {
		oldest := s.order[0]
		it, ok := s.items[oldest]
		if ok && len(s.items) <= maxItems && s.bytes <= maxBytes && it.at.After(cutoff) {
			break
		}
		if ok {
			s.bytes -= it.size
			delete(s.items, oldest)
		}
		s.order = s.order[1:]
	}
}

// get returns a copy of the stored conversation, if still kept and made by
// the same owner.
func (s *responseStore) get(id, owner string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, ttl := s.limits()
	it, ok := s.items[id]
	if !ok || it.owner != owner || time.Since(it.at) > ttl {
		return conversation{}, false
	}
	return conversation{system: it.conv.system, prompts: append([]devin.Prompt(nil), it.conv.prompts...)}, true
}

// assistantTurn rebuilds the assistant prompt from a finished response:
// its answer text and tool calls (with upstream ids, as upstream expects
// them back). Thinking is not replayed.
func assistantTurn(segs []*segment, res *devin.ChatResult) devin.Prompt {
	p := devin.Prompt{Source: devin.SourceAssistant}
	for _, s := range segs {
		if s.kind == segText {
			p.Text += s.text.String()
		}
	}
	if res != nil {
		p.ToolCalls = append(p.ToolCalls, res.ToolCalls...)
	}
	return p
}
