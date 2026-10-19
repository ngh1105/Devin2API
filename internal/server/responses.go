package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
)

// toDevinFromResponses converts an OpenAI Responses request. prior holds the
// conversation of previous_response_id (empty when not set); the new input
// continues it. Built-in tools (web_search, file_search...) are skipped
// since they run on OpenAI's side. It also returns the conversation's
// system text from input items (excluding top-level instructions, which
// the API does not carry over to follow-ups) for storing with the history.
func toDevinFromResponses(req openai.ResponsesRequest, prior conversation) (devin.ChatRequest, string, error) {
	out := devin.ChatRequest{Model: req.Model, MaxTokens: defaultMaxTokens, Temperature: 1.0, Prompts: prior.prompts}
	var convSystem []string
	if prior.system != "" {
		convSystem = append(convSystem, prior.system)
	}
	fail := func(err error) (devin.ChatRequest, string, error) { return out, "", err }
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		out.MaxTokens = int(*req.MaxOutputTokens)
	}
	if req.Temperature != nil {
		out.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		out.TopP = *req.TopP
	}
	for _, t := range req.Tools {
		if t.Type == "function" {
			out.Tools = append(out.Tools, devin.Tool{Name: t.Name, Description: t.Description, Parameters: []byte(t.Parameters)})
		}
	}
	if err := validateTools(out.Tools); err != nil {
		return fail(err)
	}

	items, err := openai.InputItems(req.Input)
	if err != nil {
		return fail(err)
	}
	for i, it := range items {
		typ := it.Type
		if typ == "" && it.Role != "" {
			typ = "message"
		}
		switch typ {
		case "message":
			text, images, err := openai.InputContent(it.Content)
			if err != nil {
				return fail(fmt.Errorf("input[%d]: %w", i, err))
			}
			switch it.Role {
			case "system", "developer":
				if text != "" {
					convSystem = append(convSystem, text)
				}
			case "user":
				p := devin.Prompt{Source: devin.SourceUser, Text: text}
				for _, img := range images {
					p.Images = append(p.Images, devin.Image{MimeType: img.MimeType, Base64: img.Base64})
				}
				out.Prompts = append(out.Prompts, p)
			case "assistant":
				out.Prompts = appendAssistantText(out.Prompts, text)
			default:
				return out, "", fmt.Errorf("input[%d]: role %q is not supported", i, it.Role)
			}
		case "function_call":
			if it.CallID == "" || it.Name == "" {
				return out, "", fmt.Errorf("input[%d]: function_call call_id and name are required", i)
			}
			args := it.Arguments
			if args == "" {
				args = "{}"
			}
			out.Prompts = appendAssistantCall(out.Prompts, devin.ToolCall{ID: decodeToolCallID(it.CallID), Name: it.Name, Arguments: args})
		case "function_call_output":
			if it.CallID == "" {
				return out, "", fmt.Errorf("input[%d]: function_call_output call_id is required", i)
			}
			text, images, err := openai.InputContent(it.Output)
			if err != nil {
				return out, "", fmt.Errorf("input[%d]: output: %w", i, err)
			}
			p := devin.Prompt{Source: devin.SourceToolResult, Text: text, ToolCallID: decodeToolCallID(it.CallID)}
			for _, img := range images {
				p.Images = append(p.Images, devin.Image{MimeType: img.MimeType, Base64: img.Base64})
			}
			out.Prompts = append(out.Prompts, p)
		case "reasoning":
			// Upstream takes no reasoning history.
		default:
			return out, "", fmt.Errorf("input[%d]: item type %q is not supported", i, typ)
		}
	}
	system := convSystem
	if req.Instructions != "" {
		system = append([]string{req.Instructions}, convSystem...)
	}
	out.System = strings.Join(system, "\n\n")
	tc, err := responsesToolChoice(req.ToolChoice)
	if err != nil {
		return out, "", err
	}
	if err := applyToolChoice(&out, tc); err != nil {
		return out, "", err
	}
	if !hasInput(out.Prompts) {
		return out, "", errors.New("input needs at least one non-empty user message")
	}
	return out, strings.Join(convSystem, "\n\n"), nil
}

// appendAssistantText adds assistant text, merging into a directly
// preceding assistant turn. Responses splits one turn into separate
// message and function_call items; upstream wants them in one prompt.
func appendAssistantText(prompts []devin.Prompt, text string) []devin.Prompt {
	if n := len(prompts); n > 0 && prompts[n-1].Source == devin.SourceAssistant && len(prompts[n-1].ToolCalls) == 0 {
		if prompts[n-1].Text != "" && text != "" {
			prompts[n-1].Text += "\n"
		}
		prompts[n-1].Text += text
		return prompts
	}
	return append(prompts, devin.Prompt{Source: devin.SourceAssistant, Text: text})
}

// appendAssistantCall adds a tool call to the directly preceding assistant
// turn, or starts one.
func appendAssistantCall(prompts []devin.Prompt, tc devin.ToolCall) []devin.Prompt {
	if n := len(prompts); n > 0 && prompts[n-1].Source == devin.SourceAssistant {
		prompts[n-1].ToolCalls = append(prompts[n-1].ToolCalls, tc)
		return prompts
	}
	return append(prompts, devin.Prompt{Source: devin.SourceAssistant, ToolCalls: []devin.ToolCall{tc}})
}

// responses handles POST /v1/responses, streaming or not.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	raw, status, msg := readBody(r)
	if status != 0 {
		openai.WriteError(w, status, msg)
		return
	}
	var req openai.ResponsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		openai.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var prior conversation
	if req.PreviousResponseID != "" {
		var ok bool
		if prior, ok = s.storedResponses.get(req.PreviousResponseID); !ok {
			openai.WriteError(w, http.StatusBadRequest, fmt.Sprintf("previous_response_id %q not found: responses are kept in memory for %v and lost on restart; send the full conversation in input instead", req.PreviousResponseID, responseTTL))
			return
		}
	}
	dreq, convSystem, err := toDevinFromResponses(req, prior)
	if err != nil {
		openai.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if status, msg := s.checkModel(&dreq, req.Model); status != 0 {
		openai.WriteError(w, status, msg)
		return
	}

	ctx, tr := s.startTrace(r, "/v1/responses", req.Model, dreq, req.Stream)
	model := req.Model
	if model == "" {
		model = "devin"
	}
	rw := &responsesWriter{id: "resp_" + randID(), model: model, created: time.Now().Unix(), previous: req.PreviousResponseID}
	if req.Stream {
		rw.sse = newSSE(w)
	}
	seg := newSegmenter(rw, true)

	res, err := s.streamWithFailover(ctx, dreq, seg.feed)
	s.finishTrace(ctx, tr, res, err)
	if err != nil {
		status, msg := upstreamStatus(err)
		if rw.sse == nil || !rw.sse.started {
			setRetryAfter(w, status, err)
			openai.WriteError(w, status, msg)
			return
		}
		failed := rw.object(seg.segs, "failed", nil)
		failed["error"] = map[string]any{"code": fmt.Sprintf("upstream_%d", status), "message": msg}
		rw.emit("response.failed", map[string]any{"response": failed})
		return
	}
	if err := seg.finish(); err != nil {
		return
	}
	if req.ShouldStore() {
		history := append(append([]devin.Prompt(nil), dreq.Prompts...), assistantTurn(seg.segs, res))
		s.storedResponses.put(rw.id, conversation{system: convSystem, prompts: history})
	}
	statusStr, incompleteReason := "completed", ""
	switch finishReason(res) {
	case "length":
		statusStr, incompleteReason = "incomplete", "max_output_tokens"
	case "content_filter":
		statusStr, incompleteReason = "incomplete", "content_filter"
	}
	rw.incompleteReason = incompleteReason
	final := rw.object(seg.segs, statusStr, res)
	if rw.sse == nil {
		writeJSON(w, http.StatusOK, final)
		return
	}
	rw.begin()
	// A response cut short by max_output_tokens ends with response.incomplete.
	event := "response.completed"
	if statusStr == "incomplete" {
		event = "response.incomplete"
	}
	rw.emit(event, map[string]any{"response": final})
}

// responsesWriter renders segments as Responses output items. Thinking
// becomes a reasoning item with a summary, text a message item, and each
// tool call a function_call item.
type responsesWriter struct {
	id, model        string
	previous         string // previous_response_id echoed in the response object
	incompleteReason string // set when the response ends incomplete
	created          int64
	sse              *sseWriter
	seq              int
	itemIDs          []string
}

func (rw *responsesWriter) emit(name string, fields map[string]any) error {
	fields["type"] = name
	fields["sequence_number"] = rw.seq
	rw.seq++
	return rw.sse.event(name, fields)
}

// begin sends response.created and response.in_progress once.
func (rw *responsesWriter) begin() error {
	if rw.sse.started {
		return nil
	}
	if err := rw.emit("response.created", map[string]any{"response": rw.object(nil, "in_progress", nil)}); err != nil {
		return err
	}
	return rw.emit("response.in_progress", map[string]any{"response": rw.object(nil, "in_progress", nil)})
}

func (rw *responsesWriter) itemID(i int, s *segment) string {
	for len(rw.itemIDs) <= i {
		rw.itemIDs = append(rw.itemIDs, "")
	}
	if rw.itemIDs[i] == "" {
		prefix := map[segKind]string{segThinking: "rs_", segText: "msg_", segTool: "fc_"}[s.kind]
		rw.itemIDs[i] = prefix + randID()
	}
	return rw.itemIDs[i]
}

// item renders segment i; done selects the final (completed) form.
func (rw *responsesWriter) item(i int, s *segment, done bool) map[string]any {
	id := rw.itemID(i, s)
	text := ""
	if done {
		text = s.text.String()
	}
	switch s.kind {
	case segThinking:
		summary := []any{}
		if done {
			summary = append(summary, map[string]any{"type": "summary_text", "text": text})
		}
		return map[string]any{"type": "reasoning", "id": id, "summary": summary}
	case segText:
		content := []any{}
		status := "in_progress"
		if done {
			content = append(content, outputText(text))
			status = "completed"
		}
		return map[string]any{"type": "message", "id": id, "status": status, "role": "assistant", "content": content}
	default:
		status := "in_progress"
		if done {
			status = "completed"
		}
		return map[string]any{
			"type": "function_call", "id": id, "status": status,
			"call_id": encodeToolCallID(s.toolID), "name": s.toolName, "arguments": text,
		}
	}
}

func outputText(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
}

func (rw *responsesWriter) open(i int, s *segment) error {
	if rw.sse == nil {
		return nil
	}
	if err := rw.begin(); err != nil {
		return err
	}
	if err := rw.emit("response.output_item.added", map[string]any{"output_index": i, "item": rw.item(i, s, false)}); err != nil {
		return err
	}
	id := rw.itemID(i, s)
	switch s.kind {
	case segThinking:
		return rw.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": id, "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""},
		})
	case segText:
		return rw.emit("response.content_part.added", map[string]any{
			"item_id": id, "output_index": i, "content_index": 0, "part": outputText(""),
		})
	}
	return nil
}

func (rw *responsesWriter) delta(i int, s *segment, chunk string) error {
	if rw.sse == nil {
		return nil
	}
	id := rw.itemID(i, s)
	switch s.kind {
	case segThinking:
		return rw.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": id, "output_index": i, "summary_index": 0, "delta": chunk})
	case segText:
		return rw.emit("response.output_text.delta", map[string]any{"item_id": id, "output_index": i, "content_index": 0, "delta": chunk})
	default:
		return rw.emit("response.function_call_arguments.delta", map[string]any{"item_id": id, "output_index": i, "delta": chunk})
	}
}

func (rw *responsesWriter) close(i int, s *segment) error {
	if rw.sse == nil {
		return nil
	}
	id := rw.itemID(i, s)
	text := s.text.String()
	var err error
	switch s.kind {
	case segThinking:
		if err = rw.emit("response.reasoning_summary_text.done", map[string]any{"item_id": id, "output_index": i, "summary_index": 0, "text": text}); err == nil {
			err = rw.emit("response.reasoning_summary_part.done", map[string]any{
				"item_id": id, "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text},
			})
		}
	case segText:
		if err = rw.emit("response.output_text.done", map[string]any{"item_id": id, "output_index": i, "content_index": 0, "text": text}); err == nil {
			err = rw.emit("response.content_part.done", map[string]any{"item_id": id, "output_index": i, "content_index": 0, "part": outputText(text)})
		}
	default:
		err = rw.emit("response.function_call_arguments.done", map[string]any{"item_id": id, "output_index": i, "arguments": text})
	}
	if err != nil {
		return err
	}
	return rw.emit("response.output_item.done", map[string]any{"output_index": i, "item": rw.item(i, s, true)})
}

// object builds the response object with the given output segments.
func (rw *responsesWriter) object(segs []*segment, status string, res *devin.ChatResult) map[string]any {
	output := []any{}
	text := ""
	for i, s := range segs {
		output = append(output, rw.item(i, s, true))
		if s.kind == segText {
			text += s.text.String()
		}
	}
	obj := map[string]any{
		"id": rw.id, "object": "response", "created_at": rw.created, "status": status,
		"model": rw.model, "output": output, "output_text": text,
		"error": nil, "incomplete_details": nil, "usage": nil,
		"previous_response_id": nilIfEmpty(rw.previous),
	}
	if status == "incomplete" {
		obj["incomplete_details"] = map[string]any{"reason": rw.incompleteReason}
	}
	if res != nil {
		obj["usage"] = map[string]int{
			"input_tokens": res.PromptTokens, "output_tokens": res.CompletionTokens,
			"total_tokens": res.PromptTokens + res.CompletionTokens,
		}
	}
	return obj
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
