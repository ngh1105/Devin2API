package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ResponsesRequest is the body of a POST /v1/responses.
type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"` // string or items
	Instructions       string          `json:"instructions,omitempty"`
	Stream             bool            `json:"stream"`
	Tools              []ResponsesTool `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	MaxOutputTokens    *int64          `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Store              *bool           `json:"store,omitempty"` // default true
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
}

// ShouldStore reports whether the response may be kept for a later
// previous_response_id (the API default is to store).
func (r ResponsesRequest) ShouldStore() bool { return r.Store == nil || *r.Store }

// WantsReasoning reports whether the client set the reasoning option.
func (r ResponsesRequest) WantsReasoning() bool {
	return len(r.Reasoning) > 0 && string(r.Reasoning) != "null"
}

// ResponsesTool is a tool definition. Only type "function" maps upstream;
// built-in tools (web_search, file_search...) have other types.
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// InputItem is one item of a Responses input array. Only the fields of its
// Type are set: messages use Role/Content, function_call uses CallID/Name/
// Arguments, function_call_output uses CallID/Output.
type InputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

// InputItems decodes input: a plain string becomes one user message.
func InputItems(raw json.RawMessage) ([]InputItem, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("input is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []InputItem{{Type: "message", Role: "user", Content: raw}}, nil
	}
	var items []InputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("input must be a string or an array of items")
	}
	return items, nil
}

// InputContent flattens Responses message content or tool output: a plain
// string, or parts of type input_text/output_text/text (joined with
// newlines) and input_image with a base64 data URL.
func InputContent(raw json.RawMessage) (string, []Image, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"` // string, or {"url": ...}
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("content must be a string or an array of parts")
	}
	var texts []string
	var images []Image
	for i, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			texts = append(texts, p.Text)
		case "input_image":
			url := imageURL(p.ImageURL)
			if url == "" {
				return "", nil, fmt.Errorf("content[%d]: input_image needs a base64 data URL in image_url (file_id is not supported)", i)
			}
			img, err := ParseDataURL(url)
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

// imageURL reads image_url given as a string or as {"url": ...}.
func imageURL(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		URL string `json:"url"`
	}
	json.Unmarshal(raw, &o)
	return o.URL
}
