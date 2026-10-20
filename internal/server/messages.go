package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ngh1105/Devin2API/internal/anthropic"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
)

// toDevinFromMessages converts an Anthropic Messages request. Server tools
// (web search, code execution...) run on Anthropic's side and are skipped;
// thinking blocks in the history are dropped because upstream takes none.
func toDevinFromMessages(req anthropic.MessagesRequest) (devin.ChatRequest, error) {
	out := devin.ChatRequest{Model: req.Model, MaxTokens: defaultMaxTokens, Temperature: 1.0}
	var err error
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out.MaxTokens = int(*req.MaxTokens)
	}
	if req.Temperature != nil {
		out.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		out.TopP = *req.TopP
	}
	if out.Stop, err = stopList(req.StopSequences, maxAnthropicStops); err != nil {
		return out, err
	}
	system, err := anthropic.SystemText(req.System)
	if err != nil {
		return out, err
	}
	out.System = system

	for _, t := range req.Tools {
		if t.IsClientTool() {
			out.Tools = append(out.Tools, devin.Tool{Name: t.Name, Description: t.Description, Parameters: []byte(t.InputSchema)})
		}
	}
	if err := validateTools(out.Tools); err != nil {
		return out, err
	}
	var tc toolChoice
	if req.ToolChoice != nil {
		if tc, err = anthropicToolChoice(req.ToolChoice.Type, req.ToolChoice.Name); err != nil {
			return out, err
		}
	}
	if err := applyToolChoice(&out, tc); err != nil {
		return out, err
	}

	for i, m := range req.Messages {
		blocks, err := anthropic.Blocks(m.Content)
		if err != nil {
			return out, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "user":
			prompts, err := userBlocks(blocks)
			if err != nil {
				return out, fmt.Errorf("messages[%d]: %w", i, err)
			}
			out.Prompts = append(out.Prompts, prompts...)
		case "assistant":
			p, err := assistantBlocks(blocks)
			if err != nil {
				return out, fmt.Errorf("messages[%d]: %w", i, err)
			}
			out.Prompts = append(out.Prompts, p)
		default:
			return out, fmt.Errorf("messages[%d]: role %q is not supported (put system prompts in \"system\")", i, m.Role)
		}
	}
	if !hasInput(out.Prompts) {
		return out, errors.New("at least one non-empty user message is required")
	}
	return out, nil
}

// userBlocks converts a user turn. tool_result blocks become tool result
// prompts, placed before the turn's own text and images.
func userBlocks(blocks []anthropic.Block) ([]devin.Prompt, error) {
	var out []devin.Prompt
	user := devin.Prompt{Source: devin.SourceUser}
	var texts []string
	for j, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			img, err := blockImage(b.Source)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", j, err)
			}
			user.Images = append(user.Images, img)
		case "tool_result":
			p, err := toolResultPrompt(b)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", j, err)
			}
			out = append(out, p)
		default:
			return nil, fmt.Errorf("content[%d]: block type %q is not supported", j, b.Type)
		}
	}
	user.Text = strings.Join(texts, "\n")
	if strings.TrimSpace(user.Text) != "" || len(user.Images) > 0 {
		out = append(out, user)
	}
	return out, nil
}

// toolResultPrompt converts a tool_result block. Upstream has no error flag,
// so failed results are marked in the text.
func toolResultPrompt(b anthropic.Block) (devin.Prompt, error) {
	if b.ToolUseID == "" {
		return devin.Prompt{}, errors.New("tool_result: tool_use_id is required")
	}
	p := devin.Prompt{Source: devin.SourceToolResult, ToolCallID: decodeToolCallID(b.ToolUseID)}
	inner, err := anthropic.Blocks(b.Content)
	if err != nil {
		return p, fmt.Errorf("tool_result: %w", err)
	}
	var texts []string
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			texts = append(texts, ib.Text)
		case "image":
			img, err := blockImage(ib.Source)
			if err != nil {
				return p, fmt.Errorf("tool_result: %w", err)
			}
			p.Images = append(p.Images, img)
		default:
			return p, fmt.Errorf("tool_result: block type %q is not supported", ib.Type)
		}
	}
	p.Text = strings.Join(texts, "\n")
	if b.IsError {
		p.Text = "Error: " + p.Text
	}
	return p, nil
}

// assistantBlocks converts an assistant turn: text plus tool_use calls.
func assistantBlocks(blocks []anthropic.Block) (devin.Prompt, error) {
	p := devin.Prompt{Source: devin.SourceAssistant}
	var texts []string
	for j, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "thinking", "redacted_thinking":
			// Upstream takes no thinking history.
		case "tool_use":
			if b.ID == "" || b.Name == "" {
				return p, fmt.Errorf("content[%d]: tool_use id and name are required", j)
			}
			args := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				args = string(b.Input)
			}
			p.ToolCalls = append(p.ToolCalls, devin.ToolCall{ID: decodeToolCallID(b.ID), Name: b.Name, Arguments: args})
		default:
			return p, fmt.Errorf("content[%d]: block type %q is not supported", j, b.Type)
		}
	}
	p.Text = strings.Join(texts, "\n")
	return p, nil
}

// blockImage converts a base64 image source.
func blockImage(src *anthropic.ImageSource) (devin.Image, error) {
	if src == nil {
		return devin.Image{}, errors.New("image source is required")
	}
	if src.Type != "base64" {
		return devin.Image{}, fmt.Errorf("image source type %q is not supported (base64 only)", src.Type)
	}
	img, err := openai.ParseDataURL("data:" + src.MediaType + ";base64," + src.Data)
	if err != nil {
		return devin.Image{}, err
	}
	return devin.Image{MimeType: img.MimeType, Base64: img.Base64}, nil
}

// anthropicStopReason maps a finished result to an Anthropic stop_reason.
func anthropicStopReason(res *devin.ChatResult) string {
	switch reason := finishReason(res); {
	case reason == "tool_calls":
		return "tool_use"
	case res.StopSequence != "":
		return "stop_sequence"
	case reason == "length":
		return "max_tokens"
	case reason == "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// stopSequence is the Anthropic stop_sequence value: the matched sequence,
// or null.
func stopSequence(res *devin.ChatResult) *string {
	if res == nil || res.StopSequence == "" {
		return nil
	}
	s := res.StopSequence
	return &s
}

func anthropicUsage(res *devin.ChatResult) anthropic.Usage {
	if res == nil {
		return anthropic.Usage{}
	}
	return anthropic.Usage{
		InputTokens:          res.PromptTokens - res.CachedPromptTokens,
		OutputTokens:         res.CompletionTokens,
		CacheReadInputTokens: res.CachedPromptTokens,
	}
}

// messages handles POST /v1/messages, streaming or not.
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	raw, status, msg := readBody(r)
	if status != 0 {
		anthropic.WriteError(w, status, msg)
		return
	}
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(req.Messages) == 0 {
		anthropic.WriteError(w, http.StatusBadRequest, "messages must not be empty")
		return
	}
	dreq, err := toDevinFromMessages(req)
	if err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if status, msg := s.checkModel(r.Context(), &dreq, req.Model); status != 0 {
		anthropic.WriteError(w, status, msg)
		return
	}

	ctx, tr := s.startTrace(r, "/v1/messages", req.Model, dreq, req.Stream)
	model := req.Model
	if model == "" {
		model = "devin"
	}
	mw := &messagesWriter{id: "msg_" + randID(), model: model}
	if req.Stream {
		mw.sse = newSSE(w)
	}
	seg := newSegmenter(mw, req.Thinking.Enabled())

	res, err := s.streamWithFailover(ctx, dreq, seg.feed)
	s.finishTrace(ctx, tr, res, err)
	if err != nil {
		status, msg := upstreamStatus(err)
		if mw.sse == nil || !mw.sse.started {
			setRetryAfter(w, status, err)
			anthropic.WriteError(w, status, msg)
			return
		}
		mw.sse.event("error", anthropic.NewError(status, msg)) // mid-stream: report in-band
		return
	}
	if err := seg.finish(); err != nil {
		return
	}
	stop := anthropicStopReason(res)
	if mw.sse == nil {
		writeJSON(w, http.StatusOK, mw.response(seg.segs, stop, res))
		return
	}
	mw.end(stop, res)
}

// messagesWriter renders segments as Anthropic content blocks: SSE events
// when streaming (sse != nil), otherwise only the final response.
type messagesWriter struct {
	id, model string
	sse       *sseWriter
}

// start sends message_start before the first block. Input tokens are only
// known at the end, so they are reported in message_delta.
func (m *messagesWriter) start() error {
	if m.sse.started {
		return nil
	}
	return m.sse.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": m.id, "type": "message", "role": "assistant", "model": m.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthropic.Usage{},
		},
	})
}

func (m *messagesWriter) open(i int, s *segment) error {
	if m.sse == nil {
		return nil
	}
	if err := m.start(); err != nil {
		return err
	}
	var block any
	switch s.kind {
	case segThinking:
		block = anthropic.ThinkingBlock{Type: "thinking"}
	case segText:
		block = anthropic.TextBlock{Type: "text"}
	default:
		block = anthropic.ToolUseBlock{Type: "tool_use", ID: encodeToolCallID(s.toolID), Name: s.toolName, Input: json.RawMessage("{}")}
	}
	return m.sse.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": block})
}

func (m *messagesWriter) delta(i int, s *segment, chunk string) error {
	if m.sse == nil {
		return nil
	}
	var d map[string]string
	switch s.kind {
	case segThinking:
		d = map[string]string{"type": "thinking_delta", "thinking": chunk}
	case segText:
		d = map[string]string{"type": "text_delta", "text": chunk}
	default:
		d = map[string]string{"type": "input_json_delta", "partial_json": chunk}
	}
	return m.sse.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": d})
}

func (m *messagesWriter) close(i int, _ *segment) error {
	if m.sse == nil {
		return nil
	}
	return m.sse.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
}

// end sends message_delta (stop reason and usage) and message_stop.
func (m *messagesWriter) end(stop string, res *devin.ChatResult) error {
	if err := m.start(); err != nil {
		return err
	}
	if err := m.sse.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": stopSequence(res)},
		"usage": anthropicUsage(res),
	}); err != nil {
		return err
	}
	return m.sse.event("message_stop", map[string]any{"type": "message_stop"})
}

// response builds the non-streaming message.
func (m *messagesWriter) response(segs []*segment, stop string, res *devin.ChatResult) anthropic.MessageResponse {
	content := []any{}
	for _, s := range segs {
		switch s.kind {
		case segThinking:
			content = append(content, anthropic.ThinkingBlock{Type: "thinking", Thinking: s.text.String()})
		case segText:
			content = append(content, anthropic.TextBlock{Type: "text", Text: s.text.String()})
		default:
			content = append(content, anthropic.ToolUseBlock{Type: "tool_use", ID: encodeToolCallID(s.toolID), Name: s.toolName, Input: toolInput(s.text.String())})
		}
	}
	return anthropic.MessageResponse{
		ID: m.id, Type: "message", Role: "assistant", Model: m.model,
		Content: content, StopReason: &stop, StopSequence: stopSequence(res), Usage: anthropicUsage(res),
	}
}

// countTokens handles POST /v1/messages/count_tokens with a rough estimate
// (about 4 bytes per token): upstream has no counting endpoint.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	raw, status, msg := readBody(r)
	if status != 0 {
		anthropic.WriteError(w, status, msg)
		return
	}
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content)
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": (n + 3) / 4})
}
