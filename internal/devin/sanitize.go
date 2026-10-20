package devin

import (
	"log/slog"
	"strings"
	"unicode/utf8"
)

// Upstream rejects requests whose system prompt carries another agent's
// identity: a Claude Code system prompt ("You are Claude Code, Anthropic's
// official CLI for Claude.") fails with permission_denied "an internal
// error occurred". The same was observed for the other lines listed here,
// which Claude Code sends alongside: its product blurb, fast-mode note,
// billing header and the two lines of its security policy. These line
// rules drop such lines; the rest of the prompt is kept as sent. They are
// substring matches, so a user's own line containing the same words is
// dropped too; SanitizeSystemPrompt logs each drop for that reason.
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

// SanitizeSystemPrompt removes lines upstream refuses (see dropLinePrefixes
// and dropLineSubstrings), normalises CRLF, and trims the result. Dropped
// lines are logged (start only), since a prompt of the user's own that
// happens to match would otherwise lose text silently.
func SanitizeSystemPrompt(prompt string) string {
	out, dropped := sanitizeSystemPrompt(prompt)
	if len(dropped) > 0 {
		slog.Warn("system prompt lines dropped (upstream refuses them)", "lines", len(dropped), "first", lineStart(dropped[0]))
	}
	return out
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

// lineStart shortens a line for the log, so prompt text is not dumped.
func lineStart(line string) string {
	const n = 60
	if len(line) <= n {
		return line
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "…"
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
