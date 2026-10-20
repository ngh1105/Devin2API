package devin

import (
	"log/slog"
	"strings"
	"sync"
)

// Upstream rejects requests whose system prompt carries another agent's
// identity: a Claude Code system prompt ("You are Claude Code, Anthropic's
// official CLI for Claude.") fails with permission_denied "an internal
// error occurred". The same was observed for the other lines listed here,
// which Claude Code sends alongside: its product blurb, fast-mode note,
// billing header and the two lines of its security policy. These line
// rules drop such lines; the rest of the prompt is kept as sent. They are
// substring matches, so a user's own line containing the same words is
// dropped too; SanitizeSystemPrompt logs each distinct dropped line once
// for that reason.
var (
	// dropLinePrefixes match the start of a trimmed line.
	dropLinePrefixes = []string{
		"You are Claude Code",
		"x-anthropic-billing-header:", // Claude Code billing attribution
	}
	// dropLineSubstrings match anywhere in a line.
	dropLineSubstrings = []string{
		"Claude Code is available as a CLI",
		"Fast mode for Claude Code",
		"authorized security testing",
		"destructive techniques, DoS attacks",
	}
)

// droppedSeen remembers which dropped lines were already logged, so the
// lines Claude Code sends with every request are reported once, not on
// every call. It is bounded; when full it starts over.
var droppedSeen = struct {
	sync.Mutex
	lines map[string]struct{}
}{lines: map[string]struct{}{}}

const droppedSeenMax = 256

// SanitizeSystemPrompt removes lines upstream refuses (see dropLinePrefixes
// and dropLineSubstrings), normalises CRLF, and trims the result. Each
// distinct dropped line is logged once (its start only), since a prompt of
// the user's own that happens to match would otherwise lose text silently.
func SanitizeSystemPrompt(prompt string) string {
	out, dropped := sanitizeSystemPrompt(prompt)
	for _, line := range dropped {
		if firstDrop(line) {
			slog.Warn("system prompt line dropped (upstream refuses it)", "line", Excerpt(line, 60))
		}
	}
	return out
}

// firstDrop reports whether line has not been logged before.
func firstDrop(line string) bool {
	droppedSeen.Lock()
	defer droppedSeen.Unlock()
	if _, ok := droppedSeen.lines[line]; ok {
		return false
	}
	if len(droppedSeen.lines) >= droppedSeenMax {
		droppedSeen.lines = map[string]struct{}{}
	}
	droppedSeen.lines[line] = struct{}{}
	return true
}

// sanitizeSystemPrompt is SanitizeSystemPrompt returning the dropped lines.
func sanitizeSystemPrompt(prompt string) (string, []string) {
	if prompt == "" {
		return "", nil
	}
	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	kept := lines[:0]
	var dropped []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if dropLine(trimmed) {
			dropped = append(dropped, trimmed)
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), dropped
}

func dropLine(trimmed string) bool {
	for _, p := range dropLinePrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	for _, s := range dropLineSubstrings {
		if strings.Contains(trimmed, s) {
			return true
		}
	}
	return false
}
