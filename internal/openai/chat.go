package openai

import (
	"encoding/json"
	"errors"
	"strings"
)

// Usage is the token accounting block of a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletion is a non-streaming /v1/chat/completions response.
type ChatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"` // "chat.completion"
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *Usage       `json:"usage,omitempty"`
}

// ChatChoice is one choice of a non-streaming completion.
type ChatChoice struct {
	Index        int           `json:"index"`
	Message      AssistantText `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// AssistantText is an assistant message with optional reasoning text.
type AssistantText struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ChatChunk is one SSE event of a streaming completion.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"` // "chat.completion.chunk"
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ChunkChoice is one choice of a streaming chunk.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the incremental content of a streaming chunk.
type Delta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ErrUnsupportedContent is returned by TextContent for non-text parts.
var ErrUnsupportedContent = errors.New("only text content is supported")

// TextContent flattens a message content value: a plain string, null, or an
// array of {"type":"text","text":...} parts (joined with newlines).
func TextContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("content must be a string or an array of parts")
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Type != "text" {
			return "", ErrUnsupportedContent
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n"), nil
}
