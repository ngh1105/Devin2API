package openai

import (
	"encoding/json"
	"net/http"
)

// Message is a single chat message.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ChatRequest is the body of a /v1/chat/completions POST.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int64    `json:"max_tokens,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
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
		Type:    "devin2api_error",
	}})
}
