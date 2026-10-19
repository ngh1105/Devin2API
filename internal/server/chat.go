package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
	"github.com/ngh1105/Devin2API/internal/store"
)

// defaultMaxTokens is sent upstream when the client does not set max_tokens.
const defaultMaxTokens = 32000

// errNoAccount means no enabled account with a session token exists.
var errNoAccount = errors.New("no enabled Devin account with a session token; add one in the panel")

// toDevinRequest converts an OpenAI chat request into a Devin chat request.
// system/developer messages become the system prompt; assistant tool_calls
// and "tool" results are forwarded so multi-turn agents work. Non-text
// content parts are rejected.
func toDevinRequest(req openai.ChatRequest) (devin.ChatRequest, error) {
	out := devin.ChatRequest{Model: req.Model, MaxTokens: defaultMaxTokens, Temperature: 1.0}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out.MaxTokens = int(*req.MaxTokens)
	}
	if req.Temperature != nil {
		out.Temperature = *req.Temperature
	}

	tools, err := toDevinTools(req.Tools, req.ToolChoice)
	if err != nil {
		return out, err
	}
	out.Tools = tools

	var system []string
	for i, m := range req.Messages {
		text, err := openai.TextContent(m.Content)
		if err != nil {
			return out, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			out.Prompts = append(out.Prompts, devin.Prompt{Source: devin.SourceUser, Text: text})
		case "assistant":
			p := devin.Prompt{Source: devin.SourceAssistant, Text: text}
			for j, tc := range m.ToolCalls {
				if tc.ID == "" || tc.Function.Name == "" {
					return out, fmt.Errorf("messages[%d].tool_calls[%d]: id and function.name are required", i, j)
				}
				p.ToolCalls = append(p.ToolCalls, devin.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			out.Prompts = append(out.Prompts, p)
		case "tool":
			if m.ToolCallID == "" {
				return out, fmt.Errorf("messages[%d]: tool_call_id is required for role \"tool\"", i)
			}
			out.Prompts = append(out.Prompts, devin.Prompt{Source: devin.SourceToolResult, Text: text, ToolCallID: m.ToolCallID})
		default:
			return out, fmt.Errorf("messages[%d]: role %q is not supported", i, m.Role)
		}
	}
	out.System = strings.Join(system, "\n\n")

	// Upstream needs something to answer: a user message or a tool result.
	hasInput := false
	for _, p := range out.Prompts {
		if (p.Source == devin.SourceUser && strings.TrimSpace(p.Text) != "") || p.Source == devin.SourceToolResult {
			hasInput = true
			break
		}
	}
	if !hasInput {
		return out, errors.New("at least one non-empty user message is required")
	}
	return out, nil
}

// toDevinTools converts OpenAI tool definitions. tool_choice "none" drops
// the tools so the model answers in text; other choices are left to the
// model (upstream has no forced-choice option).
func toDevinTools(tools []openai.Tool, choice json.RawMessage) ([]devin.Tool, error) {
	if string(choice) == `"none"` {
		return nil, nil
	}
	var out []devin.Tool
	seen := map[string]bool{}
	for i, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("tools[%d]: type %q is not supported", i, t.Type)
		}
		name := t.Function.Name
		if name == "" {
			return nil, fmt.Errorf("tools[%d]: function.name is required", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("tools[%d]: duplicate function name %q", i, name)
		}
		seen[name] = true
		params := []byte(t.Function.Parameters)
		if len(params) > 0 && !json.Valid(params) {
			return nil, fmt.Errorf("tools[%d]: parameters must be a JSON schema object", i)
		}
		out = append(out, devin.Tool{Name: name, Description: t.Function.Description, Parameters: params})
	}
	return out, nil
}

// usableAccounts returns enabled accounts that hold a session token.
func (s *Server) usableAccounts() []*store.Account {
	accs, _ := s.accounts.List()
	usable := accs[:0]
	for _, a := range accs {
		if a.Enabled && a.Tokens.SessionToken != "" {
			usable = append(usable, a)
		}
	}
	return usable
}

// chatAccounts returns the usable accounts rotated round-robin so chat load
// spreads across accounts.
func (s *Server) chatAccounts() []*store.Account {
	usable := s.usableAccounts()
	if len(usable) == 0 {
		return nil
	}
	start := int(s.chatRR.Add(1)-1) % len(usable)
	return append(usable[start:len(usable):len(usable)], usable[:start]...)
}

// failoverable reports whether another account may succeed where this one
// failed: bad/expired token or exhausted quota, before any output was sent.
func failoverable(err error) bool {
	var ue *devin.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	return ue.Status == http.StatusUnauthorized || ue.Status == http.StatusTooManyRequests ||
		ue.Status == http.StatusForbidden
}

// streamWithFailover runs the chat on each usable account in turn until one
// succeeds, fails after producing output, or fails with a non-retryable error.
func (s *Server) streamWithFailover(ctx context.Context, req devin.ChatRequest, onDelta func(devin.Delta) error) (*devin.ChatResult, error) {
	accs := s.chatAccounts()
	if len(accs) == 0 {
		return nil, errNoAccount
	}
	var lastErr error
	for _, acc := range accs {
		started := false
		res, err := devin.StreamChat(ctx, s.loginServerBase, acc.Tokens.SessionToken, req, func(d devin.Delta) error {
			started = true
			return onDelta(d)
		})
		if err == nil {
			return res, nil
		}
		lastErr = err
		var ue *devin.UpstreamError
		if errors.As(err, &ue) && ue.Status == http.StatusUnauthorized {
			s.markTokenExpired(acc.ID)
		}
		if started || !failoverable(err) {
			break
		}
	}
	return nil, lastErr
}

// markTokenExpired records on the account that its session token was rejected,
// so the panel shows it needs a re-login.
func (s *Server) markTokenExpired(id string) {
	acc, err := s.accounts.Get(id)
	if err != nil {
		return
	}
	acc.UsageErr = "token expired or revoked"
	_ = s.accounts.Save(acc)
}

// finishReason maps a finished Devin result to an OpenAI finish_reason.
// Any tool call means the client must act, whatever stop code was sent.
func finishReason(res *devin.ChatResult) string {
	if len(res.ToolCalls) > 0 {
		return "tool_calls"
	}
	switch res.StopReason {
	case devin.StopToolCalls:
		return "tool_calls"
	case 1, 3:
		return "length"
	case 11:
		return "content_filter"
	default:
		return "stop"
	}
}

// upstreamStatus picks the HTTP status and message for a failed chat.
func upstreamStatus(err error) (int, string) {
	var ue *devin.UpstreamError
	switch {
	case errors.Is(err, errNoAccount):
		return http.StatusServiceUnavailable, err.Error()
	case errors.As(err, &ue):
		if ue.Status == 499 {
			return http.StatusBadGateway, "upstream canceled the request"
		}
		return ue.Status, "devin: " + ue.Message
	default:
		return http.StatusBadGateway, err.Error()
	}
}

// chatCompletions handles POST /v1/chat/completions, streaming or not.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		openai.WriteError(w, http.StatusBadRequest, "unable to read body")
		return
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		openai.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(req.Messages) == 0 {
		openai.WriteError(w, http.StatusBadRequest, "messages must not be empty")
		return
	}
	dreq, err := toDevinRequest(req)
	if err != nil {
		openai.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Map family names ("gpt-5-4") to a concrete catalog UID ("gpt-5-4-medium").
	// Uses the cache only, so a cold cache never delays a chat request.
	catalog := s.cachedModels()
	dreq.Model = devin.MatchModel(dreq.Model, catalog)
	// Upstream answers unknown models with a misleading permission_denied
	// "internal error"; reject them locally when the catalog is known.
	if len(catalog) > 0 && !devin.HasModel(dreq.Model, catalog) {
		openai.WriteError(w, http.StatusBadRequest, fmt.Sprintf("unknown model %q; see GET /v1/models", req.Model))
		return
	}

	s.stats.requests.Add(1)
	id := "chatcmpl-" + randID()
	created := time.Now().Unix()
	model := req.Model
	if model == "" {
		model = "devin"
	}

	if req.Stream {
		s.streamCompletion(w, r, dreq, id, model, created)
		return
	}

	var text, thinking strings.Builder
	res, err := s.streamWithFailover(r.Context(), dreq, func(d devin.Delta) error {
		text.WriteString(d.Text)
		thinking.WriteString(d.Thinking)
		return nil // tool calls are read whole from res.ToolCalls
	})
	if err != nil {
		s.stats.failed.Add(1)
		status, msg := upstreamStatus(err)
		openai.WriteError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, openai.ChatCompletion{
		ID: id, Object: "chat.completion", Created: created, Model: model,
		Choices: []openai.ChatChoice{{
			Message: openai.AssistantText{
				Role: "assistant", Content: text.String(), ReasoningContent: thinking.String(),
				ToolCalls: openAIToolCalls(res.ToolCalls),
			},
			FinishReason: finishReason(res),
		}},
		Usage: usageOf(res),
	})
}

// streamCompletion writes the chat as OpenAI SSE chunks. Headers are only
// sent once the first delta arrives, so failures before any output still
// return a proper HTTP error status (and allow account failover).
func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, dreq devin.ChatRequest, id, model string, created int64) {
	flusher, _ := w.(http.Flusher)
	headerSent := false
	send := func(c openai.ChatChunk) error {
		if !headerSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			headerSent = true
		}
		c.ID, c.Object, c.Created, c.Model = id, "chat.completion.chunk", created, model
		b, _ := json.Marshal(c)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err // client went away
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	first := true
	res, err := s.streamWithFailover(r.Context(), dreq, func(dd devin.Delta) error {
		d := openai.Delta{Content: dd.Text, ReasoningContent: dd.Thinking}
		if tc := dd.ToolCall; tc != nil {
			// First fragment of a call carries id/type/name; later ones only
			// append arguments, matched by index as OpenAI clients expect.
			idx := tc.Index
			call := openai.ToolCall{Index: &idx, ID: tc.ID, Function: openai.FunctionCall{Name: tc.Name, Arguments: tc.Arguments}}
			if tc.ID != "" {
				call.Type = "function"
			}
			d.ToolCalls = []openai.ToolCall{call}
		}
		if first {
			d.Role = "assistant"
			first = false
		}
		return send(openai.ChatChunk{Choices: []openai.ChunkChoice{{Delta: d}}})
	})
	if err != nil {
		s.stats.failed.Add(1)
		status, msg := upstreamStatus(err)
		if !headerSent {
			openai.WriteError(w, status, msg)
			return
		}
		// Mid-stream failure: report it in-band, then terminate the stream.
		b, _ := json.Marshal(openai.Error{Error: openai.ErrorBody{Message: msg, Type: "devin2api_error", Code: &status}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	reason := finishReason(res)
	if err := send(openai.ChatChunk{Choices: []openai.ChunkChoice{{Delta: openai.Delta{}, FinishReason: &reason}}, Usage: usageOf(res)}); err != nil {
		return
	}
	io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// openAIToolCalls converts complete upstream tool calls for a non-streaming
// response.
func openAIToolCalls(calls []devin.ToolCall) []openai.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]openai.ToolCall, len(calls))
	for i, c := range calls {
		args := c.Arguments
		if args == "" {
			args = "{}"
		}
		out[i] = openai.ToolCall{ID: c.ID, Type: "function", Function: openai.FunctionCall{Name: c.Name, Arguments: args}}
	}
	return out
}

func usageOf(res *devin.ChatResult) *openai.Usage {
	if res == nil || (res.PromptTokens == 0 && res.CompletionTokens == 0) {
		return nil
	}
	return &openai.Usage{
		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		TotalTokens:      res.PromptTokens + res.CompletionTokens,
	}
}
