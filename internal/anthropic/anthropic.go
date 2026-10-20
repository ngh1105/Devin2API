// Package anthropic holds the request/response shapes of the Anthropic
// Messages API (POST /v1/messages) used by Claude Code and Anthropic SDKs.
package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// MessagesRequest is the body of a POST /v1/messages.
type MessagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     *int64          `json:"max_tokens,omitempty"`
	System        json.RawMessage `json:"system,omitempty"` // string or text blocks
	Messages      []Message       `json:"messages"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Thinking      *Thinking       `json:"thinking,omitempty"`
}

// Message is one conversation turn; Content is a string or content blocks.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Tool is a client tool. Server tools (web search, code execution...) carry
// a versioned Type such as "web_search_20250305".
type Tool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// IsClientTool reports whether t is a plain client tool the proxy can
// forward (no type, or "custom").
func (t Tool) IsClientTool() bool { return t.Type == "" || t.Type == "custom" }

// ToolChoice is {"type": "auto" | "any" | "tool" | "none", "name": ...}.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// Thinking is {"type": "enabled" | "adaptive" | "disabled", "budget_tokens": n}.
type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// Enabled reports whether the client asked for thinking blocks.
func (t *Thinking) Enabled() bool { return t != nil && (t.Type == "enabled" || t.Type == "adaptive") }

// Block is an input content block. Only the fields of its Type are set.
type Block struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// image
	Source *ImageSource `json:"source,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ImageSource is {"type": "base64", "media_type": ..., "data": ...}.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url,omitempty"`
}

// Blocks decodes message content: a plain string becomes one text block.
func Blocks(raw json.RawMessage) ([]Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []Block{{Type: "text", Text: s}}, nil
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("content must be a string or an array of content blocks")
	}
	return blocks, nil
}

// SystemText flattens the system prompt (string or text blocks).
func SystemText(raw json.RawMessage) (string, error) {
	blocks, err := Blocks(raw)
	if err != nil {
		return "", fmt.Errorf("system: %w", err)
	}
	var texts []string
	for i, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("system[%d]: block type %q is not supported", i, b.Type)
		}
		if b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n"), nil
}

// Usage is the token accounting of a message. As in the Anthropic API,
// InputTokens excludes tokens read from the cache, which are reported in
// CacheReadInputTokens.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// MessageResponse is a non-streaming /v1/messages response. Content holds
// TextBlock, ThinkingBlock and ToolUseBlock values.
type MessageResponse struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"` // "message"
	Role         string  `json:"role"` // "assistant"
	Model        string  `json:"model"`
	Content      []any   `json:"content"`
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        Usage   `json:"usage"`
}

// TextBlock is an output text block.
type TextBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// ThinkingBlock is an output thinking block. Upstream gives no signature,
// so it is always empty.
type ThinkingBlock struct {
	Type      string `json:"type"` // "thinking"
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

// ToolUseBlock is an output tool call.
type ToolUseBlock struct {
	Type  string          `json:"type"` // "tool_use"
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ErrorType maps an HTTP status to an Anthropic error type.
func ErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// ErrorBody is {"type": "error", "error": {"type": ..., "message": ...}}.
type ErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// NewError builds an error body for status.
func NewError(status int, msg string) ErrorBody {
	var e ErrorBody
	e.Type = "error"
	e.Error.Type = ErrorType(status)
	e.Error.Message = msg
	return e
}

// WriteError writes an Anthropic-format error response.
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(NewError(status, msg))
}
