package openai

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Message is a single chat message. Assistant messages may carry ToolCalls;
// "tool" messages answer one call via ToolCallID.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// ChatRequest is the body of a /v1/chat/completions POST.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int64    `json:"max_tokens,omitempty"`
	// MaxCompletionTokens replaces the deprecated max_tokens in current
	// OpenAI SDKs; it wins when both are set.
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"` // string or list of strings
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// OutputLimit returns the requested output token cap, or 0 if unset.
func (r ChatRequest) OutputLimit() int64 {
	for _, p := range []*int64{r.MaxCompletionTokens, r.MaxTokens} {
		if p != nil && *p > 0 {
			return *p
		}
	}
	return 0
}

// IncludeUsage reports whether a streaming client asked for the usage chunk.
func (r ChatRequest) IncludeUsage() bool {
	return r.StreamOptions != nil && r.StreamOptions.IncludeUsage
}

// StopSequences decodes the stop parameter, given as a string or a list.
func (r ChatRequest) StopSequences() ([]string, error) {
	if len(r.Stop) == 0 || string(r.Stop) == "null" {
		return nil, nil
	}
	var one string
	if json.Unmarshal(r.Stop, &one) == nil {
		return []string{one}, nil
	}
	var list []string
	if err := json.Unmarshal(r.Stop, &list); err != nil {
		return nil, errors.New("stop must be a string or an array of strings")
	}
	return list, nil
}

// Tool is a function the model may call.
type Tool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// ToolCall is a function call made by the assistant. Index is only set in
// streaming deltas, where it identifies which call a fragment extends.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name and JSON arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ErrorBody is the error object embedded in an Error response.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    *int   `json:"code,omitempty"`
}

// Error wraps the error body for JSON serialization.
type Error struct {
	Error ErrorBody `json:"error"`
}

// WriteError marshals an openai.Error and writes it as JSON to w with status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Error{Error: ErrorBody{
		Message: msg,
		Type:    ErrorType(status),
	}})
}

// ErrorType maps an HTTP status to the OpenAI error type SDKs classify on.
func ErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if status >= 500 {
			return "server_error"
		}
		return "invalid_request_error"
	}
}
