package server

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ngh1105/Devin2API/internal/devin"
)

// Stop sequence limits: OpenAI documents 4; Anthropic sets no small limit,
// so its cap only guards against abuse (each one is checked per delta).
// maxStopLen bounds one sequence: safeLen is quadratic in it per delta and
// holds back up to that much output.
const (
	maxOpenAIStops    = 4
	maxAnthropicStops = 32
	maxStopLen        = 1024
)

// errStopSequence aborts the upstream stream once a stop sequence appears.
var errStopSequence = errors.New("stop sequence reached")

// stopWatcher applies client stop sequences, which upstream does not take.
// It forwards answer text minus a held-back tail that could be the start of
// a stop sequence split across deltas; on a match it forwards the text
// before it and aborts the stream with errStopSequence. Thinking and tool
// call deltas pass through, after any held text so order is kept.
type stopWatcher struct {
	stops []string
	next  func(devin.Delta) error
	held  string
	hit   string // the stop sequence that matched
}

func newStopWatcher(stops []string, next func(devin.Delta) error) *stopWatcher {
	return &stopWatcher{stops: stops, next: next}
}

func (s *stopWatcher) feed(d devin.Delta) error {
	if d.Text == "" {
		if err := s.flush(); err != nil {
			return err
		}
		return s.next(d)
	}
	text := s.held + d.Text
	s.held = ""
	if i, stop := s.firstMatch(text); i >= 0 {
		s.hit = stop
		d.Text = text[:i]
		if d.Text != "" || d.Thinking != "" {
			if err := s.next(d); err != nil {
				return err
			}
		}
		return errStopSequence
	}
	cut := s.safeLen(text)
	s.held = text[cut:]
	d.Text = text[:cut]
	if d.Text == "" && d.Thinking == "" && d.ToolCall == nil {
		return nil
	}
	return s.next(d)
}

// flush forwards the held-back tail; call it when the stream ends normally.
func (s *stopWatcher) flush() error {
	if s.held == "" {
		return nil
	}
	t := s.held
	s.held = ""
	return s.next(devin.Delta{Text: t})
}

func (s *stopWatcher) firstMatch(text string) (int, string) {
	best, which := -1, ""
	for _, stop := range s.stops {
		if i := strings.Index(text, stop); i >= 0 && (best < 0 || i < best) {
			best, which = i, stop
		}
	}
	return best, which
}

// safeLen returns how much of text can be forwarded: everything except the
// longest suffix that is a prefix of some stop sequence, cut on a rune
// boundary.
func (s *stopWatcher) safeLen(text string) int {
	keep := 0
	for _, stop := range s.stops {
		for n := min(len(stop)-1, len(text)); n > keep; n-- {
			if strings.HasSuffix(text, stop[:n]) {
				keep = n
				break
			}
		}
	}
	cut := len(text) - keep
	for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return cut
}

// stopList drops empty stop sequences and enforces the API's limit.
func stopList(stops []string, limit int) ([]string, error) {
	var out []string
	for _, s := range stops {
		if len(s) > maxStopLen {
			return nil, fmt.Errorf("stop sequences may be at most %d bytes", maxStopLen)
		}
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) > limit {
		return nil, fmt.Errorf("at most %d stop sequences are supported", limit)
	}
	return out, nil
}
