package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Usage is the token accounting block of a completion. As in the OpenAI
// API, PromptTokens includes cached tokens, which are also reported in
// PromptTokensDetails.CachedTokens.
type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails breaks down the prompt tokens.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
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

// AssistantText is an assistant message with optional reasoning text and
// tool calls.
type AssistantText struct {
	Role string `json:"role"`
	// Content is null (nil) when the reply is only tool calls, as in the
	// OpenAI API.
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
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
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ErrUnsupportedContent is returned by TextContent for non-text parts.
var ErrUnsupportedContent = errors.New("only text content is supported")

// Image is an inline image from a content part.
type Image struct {
	MimeType string // e.g. "image/png"
	Base64   string // standard base64, no data: prefix
}

// allowedImageTypes are the image formats forwarded upstream.
var allowedImageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true,
}

// UserContent flattens a user message: text parts are joined with newlines
// and image_url parts must be base64 data URLs (remote URLs are rejected,
// the proxy does not fetch them).
func UserContent(raw json.RawMessage) (string, []Image, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("content must be a string or an array of parts")
	}
	var texts []string
	var images []Image
	for i, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "image_url":
			if p.ImageURL == nil {
				return "", nil, fmt.Errorf("content[%d]: image_url.url is required", i)
			}
			img, err := ParseDataURL(p.ImageURL.URL)
			if err != nil {
				return "", nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			images = append(images, img)
		default:
			return "", nil, fmt.Errorf("content[%d]: part type %q is not supported", i, p.Type)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

// ParseDataURL decodes "data:<mime>;base64,<data>" and checks the payload
// is valid base64 of an allowed image type.
func ParseDataURL(u string) (Image, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(u), "data:")
	if !ok {
		return Image{}, errors.New("only base64 data URLs are supported for images (data:image/png;base64,...)")
	}
	meta, data, ok := strings.Cut(rest, ",")
	mime, isB64 := strings.CutSuffix(meta, ";base64")
	if !ok || !isB64 {
		return Image{}, errors.New("image data URL must be base64-encoded")
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "image/jpg" { // common non-standard spelling
		mime = "image/jpeg"
	}
	if !allowedImageTypes[mime] {
		return Image{}, fmt.Errorf("image type %q is not supported (png, jpeg, webp, gif)", mime)
	}
	data = strings.TrimSpace(data)
	// Accept unpadded base64 too, but forward the padded standard form.
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(data, "="))
	}
	if err != nil || len(decoded) == 0 {
		return Image{}, errors.New("image data is not valid base64")
	}
	return Image{MimeType: mime, Base64: base64.StdEncoding.EncodeToString(decoded)}, nil
}

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
