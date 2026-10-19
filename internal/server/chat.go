package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
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
	if req.TopP != nil {
		out.TopP = *req.TopP
	}
	stops, err := req.StopSequences()
	if err != nil {
		return out, err
	}
	if out.Stop, err = stopList(stops, maxOpenAIStops); err != nil {
		return out, err
	}

	tools, err := toDevinTools(req.Tools)
	if err != nil {
		return out, err
	}
	out.Tools = tools

	var system []string
	for i, m := range req.Messages {
		var text string
		var images []openai.Image
		var err error
		if m.Role == "user" {
			text, images, err = openai.UserContent(m.Content)
		} else {
			text, err = openai.TextContent(m.Content)
		}
		if err != nil {
			return out, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			p := devin.Prompt{Source: devin.SourceUser, Text: text}
			for _, img := range images {
				p.Images = append(p.Images, devin.Image{MimeType: img.MimeType, Base64: img.Base64})
			}
			out.Prompts = append(out.Prompts, p)
		case "assistant":
			p := devin.Prompt{Source: devin.SourceAssistant, Text: text}
			for j, tc := range m.ToolCalls {
				if tc.ID == "" || tc.Function.Name == "" {
					return out, fmt.Errorf("messages[%d].tool_calls[%d]: id and function.name are required", i, j)
				}
				p.ToolCalls = append(p.ToolCalls, devin.ToolCall{ID: decodeToolCallID(tc.ID), Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			out.Prompts = append(out.Prompts, p)
		case "tool":
			if m.ToolCallID == "" {
				return out, fmt.Errorf("messages[%d]: tool_call_id is required for role \"tool\"", i)
			}
			out.Prompts = append(out.Prompts, devin.Prompt{Source: devin.SourceToolResult, Text: text, ToolCallID: decodeToolCallID(m.ToolCallID)})
		default:
			return out, fmt.Errorf("messages[%d]: role %q is not supported", i, m.Role)
		}
	}
	out.System = strings.Join(system, "\n\n")

	tc, err := openAIToolChoice(req.ToolChoice)
	if err != nil {
		return out, err
	}
	if err := applyToolChoice(&out, tc); err != nil {
		return out, err
	}

	if !hasInput(out.Prompts) {
		return out, errors.New("at least one non-empty user message is required")
	}
	return out, nil
}

// hasInput reports whether upstream has something to answer: a non-empty
// user message (text or images) or a tool result.
func hasInput(prompts []devin.Prompt) bool {
	for _, p := range prompts {
		if p.Source == devin.SourceToolResult {
			return true
		}
		if p.Source == devin.SourceUser && (strings.TrimSpace(p.Text) != "" || len(p.Images) > 0) {
			return true
		}
	}
	return false
}

// toDevinTools converts OpenAI tool definitions; tool_choice is applied
// separately by applyToolChoice.
func toDevinTools(tools []openai.Tool) ([]devin.Tool, error) {
	var out []devin.Tool
	for i, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("tools[%d]: type %q is not supported", i, t.Type)
		}
		out = append(out, devin.Tool{Name: t.Function.Name, Description: t.Function.Description, Parameters: []byte(t.Function.Parameters)})
	}
	if err := validateTools(out); err != nil {
		return nil, err
	}
	return out, nil
}

// validateTools checks that tool names are present and unique and that
// parameter schemas are JSON objects.
func validateTools(tools []devin.Tool) error {
	seen := make(map[string]bool, len(tools))
	for i, t := range tools {
		if t.Name == "" {
			return fmt.Errorf("tools[%d]: name is required", i)
		}
		if seen[t.Name] {
			return fmt.Errorf("tools[%d]: duplicate tool name %q", i, t.Name)
		}
		seen[t.Name] = true
		if len(t.Parameters) > 0 && !isJSONObject(t.Parameters) {
			return fmt.Errorf("tools[%d]: parameters must be a JSON schema object", i)
		}
	}
	return nil
}

func isJSONObject(b []byte) bool {
	return json.Valid(b) && strings.HasPrefix(strings.TrimSpace(string(b)), "{")
}

// toolInput returns tool call arguments as a JSON object, for formats that
// carry them as objects (Anthropic tool_use input). Arguments that are not
// a JSON object are wrapped so nothing is lost.
func toolInput(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}")
	}
	if isJSONObject([]byte(args)) {
		return json.RawMessage(args)
	}
	b, _ := json.Marshal(map[string]string{"raw_arguments": args})
	return b
}

// readBody reads a /v1 request body. On failure it returns the status and
// message to send: 413 for an over-limit body, 400 otherwise.
func readBody(r *http.Request) ([]byte, int, string) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d MB", tooBig.Limit>>20)
		}
		return nil, http.StatusBadRequest, "unable to read body"
	}
	return raw, 0, ""
}

// checkModel maps dreq.Model onto the cached catalog ("gpt-5-4" ->
// "gpt-5-4-medium") and rejects unknown models and images sent to text-only
// models. It uses the cache only, so a cold cache never delays a request;
// requested is the client's model name for error messages. A zero status
// means the request may proceed.
func (s *Server) checkModel(dreq *devin.ChatRequest, requested string) (int, string) {
	catalog := s.cachedModels()
	dreq.Model = devin.MatchModel(dreq.Model, catalog)
	if len(catalog) == 0 {
		return 0, ""
	}
	// Upstream answers unknown models with a misleading permission_denied
	// "internal error", so reject them locally.
	if !devin.HasModel(dreq.Model, catalog) {
		return http.StatusBadRequest, fmt.Sprintf("unknown model %q; see GET /v1/models", requested)
	}
	if hasImages(*dreq) && !devin.IsMultimodal(dreq.Model, catalog) {
		return http.StatusBadRequest, fmt.Sprintf("model %q does not accept images; pick one with \"multimodal\": true in GET /v1/models", dreq.Model)
	}
	return 0, ""
}

// collectChat runs a chat to completion and returns the whole answer and
// thinking text; tool calls are in res.ToolCalls.
func (s *Server) collectChat(ctx context.Context, dreq devin.ChatRequest) (text, thinking string, res *devin.ChatResult, err error) {
	var tb, thb strings.Builder
	res, err = s.streamWithFailover(ctx, dreq, func(d devin.Delta) error {
		tb.WriteString(d.Text)
		thb.WriteString(d.Thinking)
		return nil
	})
	return tb.String(), thb.String(), res, err
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
	model := devin.ResolveModel(req.Model)
	accs := s.chatAccounts(model)
	if len(accs) == 0 {
		return nil, errNoAccount
	}
	tr := traceFrom(ctx)
	var lastErr error
	for _, acc := range accs {
		tr.attempt(acc.ID, accountLabel(acc))
		started := false
		deliver := func(d devin.Delta) error {
			tr.output()
			return onDelta(d)
		}
		var sw *stopWatcher
		if len(req.Stop) > 0 {
			sw = newStopWatcher(req.Stop, deliver)
			deliver = sw.feed
		}
		res, err := devin.StreamChat(ctx, s.loginServerBase, acc.Tokens.SessionToken, req, func(d devin.Delta) error {
			started = true
			return deliver(d)
		})
		if sw != nil {
			switch {
			case errors.Is(err, errStopSequence):
				res.StopSequence = sw.hit
				return res, nil
			case err == nil:
				if err := sw.flush(); err != nil {
					return nil, err
				}
			}
		}
		if err == nil {
			return res, nil
		}
		lastErr = err
		var ue *devin.UpstreamError
		if errors.As(err, &ue) {
			switch ue.Status {
			case http.StatusUnauthorized:
				s.markTokenExpired(acc.ID)
			case http.StatusTooManyRequests:
				// Park this account for this model until the stated reset.
				s.cooldown.set(acc.ID, model, time.Duration(retryAfterSeconds(ue.Message))*time.Second)
			}
		}
		if started || !failoverable(err) {
			break
		}
	}
	return nil, lastErr
}

// accountLabel names an account for logs: nickname, email, user name or id.
func accountLabel(a *store.Account) string {
	for _, s := range []string{a.Nickname, a.Email, a.UserName} {
		if s != "" {
			return s
		}
	}
	return a.ID
}

// markTokenExpired records on the account that its session token was rejected,
// so the panel shows it needs a re-login.
func (s *Server) markTokenExpired(id string) {
	s.accounts.Mutate(id, func(a *store.Account) { a.UsageErr = tokenExpiredMsg })
}

// finishReason maps a finished Devin result to an OpenAI finish_reason.
// Any tool call means the client must act, whatever stop code was sent.
func finishReason(res *devin.ChatResult) string {
	if len(res.ToolCalls) > 0 {
		return "tool_calls"
	}
	if res.StopSequence != "" {
		return "stop"
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

// hasImages reports whether any prompt carries an image.
func hasImages(req devin.ChatRequest) bool {
	for _, p := range req.Prompts {
		if len(p.Images) > 0 {
			return true
		}
	}
	return false
}

// resetInRe matches the wait Devin states in rate-limit messages, e.g.
// "Your limit will reset in 22 minutes" or "reset in 1 hour and 5 minutes".
var (
	resetInRe   = regexp.MustCompile(`(?i)reset in ([^.(]+)`)
	resetUnitRe = regexp.MustCompile(`(?i)(\d+)\s*(hour|minute|min|second|sec)`)
)

// retryAfterSeconds extracts the stated wait from an upstream message, in
// seconds, or 0 when it states none.
func retryAfterSeconds(msg string) int {
	m := resetInRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	total := 0
	for _, u := range resetUnitRe.FindAllStringSubmatch(m[1], -1) {
		n, err := strconv.Atoi(u[1])
		if err != nil {
			continue
		}
		switch strings.ToLower(u[2])[0] {
		case 'h':
			total += n * 3600
		case 'm':
			total += n * 60
		default:
			total += n
		}
	}
	return total
}

// setRetryAfter adds a Retry-After header to a 429 when upstream said how
// long to wait, so clients can back off instead of retrying immediately.
func setRetryAfter(w http.ResponseWriter, status int, err error) {
	var ue *devin.UpstreamError
	if status != http.StatusTooManyRequests || !errors.As(err, &ue) {
		return
	}
	if secs := retryAfterSeconds(ue.Message); secs > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(secs))
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
	raw, status, msg := readBody(r)
	if status != 0 {
		openai.WriteError(w, status, msg)
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
	if status, msg := s.checkModel(&dreq, req.Model); status != 0 {
		openai.WriteError(w, status, msg)
		return
	}

	ctx, tr := s.startTrace(r, "/v1/chat/completions", req.Model, dreq, req.Stream)
	id := "chatcmpl-" + randID()
	created := time.Now().Unix()
	model := req.Model
	if model == "" {
		model = "devin"
	}

	if req.Stream {
		s.streamCompletion(ctx, tr, w, dreq, id, model, created)
		return
	}

	text, thinking, res, err := s.collectChat(ctx, dreq)
	s.finishTrace(ctx, tr, res, err)
	if err != nil {
		status, msg := upstreamStatus(err)
		setRetryAfter(w, status, err)
		openai.WriteError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, openai.ChatCompletion{
		ID: id, Object: "chat.completion", Created: created, Model: model,
		Choices: []openai.ChatChoice{{
			Message: openai.AssistantText{
				Role: "assistant", Content: text, ReasoningContent: thinking,
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
func (s *Server) streamCompletion(ctx context.Context, tr *chatTrace, w http.ResponseWriter, dreq devin.ChatRequest, id, model string, created int64) {
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
	res, err := s.streamWithFailover(ctx, dreq, func(dd devin.Delta) error {
		d := openai.Delta{Content: dd.Text, ReasoningContent: dd.Thinking}
		if tc := dd.ToolCall; tc != nil {
			// First fragment of a call carries id/type/name; later ones only
			// append arguments, matched by index as OpenAI clients expect.
			idx := tc.Index
			call := openai.ToolCall{Index: &idx, Function: openai.FunctionCall{Name: tc.Name, Arguments: tc.Arguments}}
			if tc.ID != "" {
				call.ID, call.Type = encodeToolCallID(tc.ID), "function"
			}
			d.ToolCalls = []openai.ToolCall{call}
		}
		if first {
			d.Role = "assistant"
			first = false
		}
		return send(openai.ChatChunk{Choices: []openai.ChunkChoice{{Delta: d}}})
	})
	s.finishTrace(ctx, tr, res, err)
	if err != nil {
		status, msg := upstreamStatus(err)
		if !headerSent {
			setRetryAfter(w, status, err)
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

// Upstream tool call ids can contain '#' and ':' (e.g. "get_weather:0#ab12"),
// which clients that require [A-Za-z0-9_-] ids (Anthropic-format bridges)
// reject. Such ids are sent to clients as "dv_" + base64url(raw) and decoded
// back when the client returns them, so upstream always sees its own id.
const toolIDPrefix = "dv_"

var safeToolID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// encodeToolCallID makes an upstream id safe for clients. Ids that are
// already safe pass through unchanged, unless they could be mistaken for an
// encoded id.
func encodeToolCallID(raw string) string {
	if safeToolID.MatchString(raw) && !strings.HasPrefix(raw, toolIDPrefix) {
		return raw
	}
	return toolIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeToolCallID reverses encodeToolCallID. Ids it did not produce are
// returned unchanged.
func decodeToolCallID(id string) string {
	enc, ok := strings.CutPrefix(id, toolIDPrefix)
	if !ok {
		return id
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || len(raw) == 0 {
		return id
	}
	return string(raw)
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
		out[i] = openai.ToolCall{ID: encodeToolCallID(c.ID), Type: "function", Function: openai.FunctionCall{Name: c.Name, Arguments: args}}
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
