package server

import (
	"sync"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
)

// Responses kept for previous_response_id. They live in memory only, so a
// restart forgets them; clients can always send the full history instead.
const (
	responseStoreSize = 256
	responseTTL       = time.Hour
)

// responseStore maps a response id to the conversation it ended: the input
// prompts (including any earlier turns) plus the assistant's reply.
type responseStore struct {
	mu    sync.Mutex
	items map[string]storedResponse
	order []string // insertion order, oldest first
}

type storedResponse struct {
	prompts []devin.Prompt
	at      time.Time
}

func (s *responseStore) put(id string, prompts []devin.Prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = map[string]storedResponse{}
	}
	s.items[id] = storedResponse{prompts: prompts, at: time.Now()}
	s.order = append(s.order, id)
	// Drop expired entries and the oldest beyond the size cap.
	cutoff := time.Now().Add(-responseTTL)
	for len(s.order) > 0 {
		oldest := s.order[0]
		it, ok := s.items[oldest]
		if ok && len(s.items) <= responseStoreSize && it.at.After(cutoff) {
			break
		}
		delete(s.items, oldest)
		s.order = s.order[1:]
	}
}

// get returns a copy of the stored conversation, if still kept.
func (s *responseStore) get(id string) ([]devin.Prompt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok || time.Since(it.at) > responseTTL {
		return nil, false
	}
	return append([]devin.Prompt(nil), it.prompts...), true
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
