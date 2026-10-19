package openai

import (
	"encoding/json"
	"errors"
)

// CompletionRequest is the body of a legacy /v1/completions POST.
type CompletionRequest struct {
	Model         string          `json:"model"`
	Prompt        json.RawMessage `json:"prompt"` // string or list of one string
	Suffix        *string         `json:"suffix,omitempty"`
	Stream        bool            `json:"stream"`
	Echo          bool            `json:"echo,omitempty"`
	N             *int            `json:"n,omitempty"`
	BestOf        *int            `json:"best_of,omitempty"`
	Logprobs      *int            `json:"logprobs,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	MaxTokens     *int64          `json:"max_tokens,omitempty"`
	Stop          json.RawMessage `json:"stop,omitempty"` // string or list of strings
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// PromptText decodes the prompt: a string, or a list holding exactly one
// string. Token-id arrays and batches of several prompts are rejected.
func (r CompletionRequest) PromptText() (string, error) {
	if len(r.Prompt) == 0 || string(r.Prompt) == "null" {
		return "", errors.New("prompt is required")
	}
	var one string
	if json.Unmarshal(r.Prompt, &one) == nil {
		return one, nil
	}
	var list []string
	if err := json.Unmarshal(r.Prompt, &list); err != nil {
		return "", errors.New("prompt must be a string or an array of strings (token arrays are not supported)")
	}
	if len(list) != 1 {
		return "", errors.New("prompt must hold exactly one string (batched prompts are not supported)")
	}
	return list[0], nil
}

// AsChat returns the chat-request view of the shared sampling parameters,
// so stop and output-limit parsing is shared with /v1/chat/completions.
func (r CompletionRequest) AsChat() ChatRequest {
	return ChatRequest{
		Model: r.Model, Stream: r.Stream, Temperature: r.Temperature, TopP: r.TopP,
		MaxTokens: r.MaxTokens, Stop: r.Stop, StreamOptions: r.StreamOptions,
	}
}

// Completion is a /v1/completions response, or one streaming chunk of it.
type Completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"` // "text_completion"
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   *Usage             `json:"usage,omitempty"`
}

// CompletionChoice is one choice of a completion. FinishReason is null in
// streaming chunks until the last one.
type CompletionChoice struct {
	Text         string          `json:"text"`
	Index        int             `json:"index"`
	Logprobs     json.RawMessage `json:"logprobs"` // always null
	FinishReason *string         `json:"finish_reason"`
}
