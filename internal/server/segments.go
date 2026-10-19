package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ngh1105/Devin2API/internal/devin"
)

// segKind is the kind of an output segment.
type segKind int

const (
	segThinking segKind = iota
	segText
	segTool
)

// segment is one contiguous piece of assistant output: a run of thinking
// text, a run of answer text, or one tool call. text holds the thinking or
// answer text, or the tool call's JSON arguments.
type segment struct {
	kind     segKind
	text     strings.Builder
	toolID   string // upstream tool call id (encode before sending)
	toolName string
	closed   bool
}

// blockWriter receives segment events in order: open, any number of
// delta, close. i is the segment's position in the output.
type blockWriter interface {
	open(i int, s *segment) error
	delta(i int, s *segment, chunk string) error
	close(i int, s *segment) error
}

// segmenter splits upstream deltas into ordered output segments, the shape
// both the Anthropic Messages and OpenAI Responses formats use: a segment
// starts whenever the output kind changes or a new tool call begins.
type segmenter struct {
	w        blockWriter
	thinking bool // keep thinking text; otherwise it is dropped
	segs     []*segment
	cur      int         // open segment, -1 if none
	toolSeg  map[int]int // upstream tool call index -> segment index
}

func newSegmenter(w blockWriter, thinking bool) *segmenter {
	return &segmenter{w: w, thinking: thinking, cur: -1, toolSeg: map[int]int{}}
}

// feed takes one upstream delta.
func (s *segmenter) feed(d devin.Delta) error {
	if d.Thinking != "" && s.thinking {
		if err := s.appendRun(segThinking, d.Thinking); err != nil {
			return err
		}
	}
	if d.Text != "" {
		if err := s.appendRun(segText, d.Text); err != nil {
			return err
		}
	}
	if d.ToolCall != nil {
		return s.appendTool(d.ToolCall)
	}
	return nil
}

// appendRun adds thinking or answer text, continuing the open segment when
// it has the same kind.
func (s *segmenter) appendRun(kind segKind, chunk string) error {
	if s.cur < 0 || s.segs[s.cur].kind != kind {
		if err := s.start(&segment{kind: kind}); err != nil {
			return err
		}
	}
	seg := s.segs[s.cur]
	seg.text.WriteString(chunk)
	return s.w.delta(s.cur, seg, chunk)
}

// appendTool adds a tool call fragment. The first fragment of a call starts
// its segment; later fragments extend it even if another segment started
// since (upstream rarely interleaves calls, so this is best effort).
func (s *segmenter) appendTool(tc *devin.ToolCallDelta) error {
	i, ok := s.toolSeg[tc.Index]
	if !ok {
		if err := s.start(&segment{kind: segTool, toolID: tc.ID, toolName: tc.Name}); err != nil {
			return err
		}
		i = s.cur
		s.toolSeg[tc.Index] = i
	}
	seg := s.segs[i]
	if seg.toolName == "" {
		seg.toolName = tc.Name
	}
	if tc.Arguments == "" {
		return nil
	}
	seg.text.WriteString(tc.Arguments)
	return s.w.delta(i, seg, tc.Arguments)
}

func (s *segmenter) start(seg *segment) error {
	if err := s.closeCur(); err != nil {
		return err
	}
	s.segs = append(s.segs, seg)
	s.cur = len(s.segs) - 1
	return s.w.open(s.cur, seg)
}

func (s *segmenter) closeCur() error {
	if s.cur < 0 {
		return nil
	}
	i := s.cur
	s.cur = -1
	s.segs[i].closed = true
	return s.w.close(i, s.segs[i])
}

// finish closes the open segment once the stream has ended.
func (s *segmenter) finish() error { return s.closeCur() }

// sseWriter writes named server-sent events. Response headers go out with
// the first event, so a failure before any output can still be answered
// with a proper HTTP status (and lets account failover run).
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
}

func newSSE(w http.ResponseWriter) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: f}
}

func (e *sseWriter) event(name string, v any) error {
	if !e.started {
		h := e.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		e.w.WriteHeader(http.StatusOK)
		e.started = true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err // client went away
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}
