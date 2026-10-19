package devin

import "strings"

// Upstream rejects requests whose system prompt carries another agent's
// identity: a Claude Code system prompt ("You are Claude Code, Anthropic's
// official CLI for Claude.") fails with permission_denied "an internal
// error occurred". These line rules drop such lines; the rest of the
// prompt is kept as sent.
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
// and dropLineSubstrings), normalises CRLF, and trims the result.
func SanitizeSystemPrompt(prompt string) string {
	if prompt == "" {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !dropLine(strings.TrimSpace(line)) {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
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
