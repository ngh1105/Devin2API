package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
)

// completionSystem turns a chat model into a text continuer: upstream only
// offers chat models, so the legacy prompt is sent as a user message with
// this instruction.
const completionSystem = "You are a text completion engine. The user message is a text prefix. " +
	"Output only its continuation, starting exactly where the prefix stops. " +
	"Do not repeat the prefix and do not add any commentary or formatting."

// toDevinCompletion converts a legacy completion request. It returns the
// prompt text so echo can prepend it.
func toDevinCompletion(req openai.CompletionRequest) (devin.ChatRequest, string, error) {
	prompt, err := req.PromptText()
	if err != nil {
		return devin.ChatRequest{}, "", err
	}
	switch {
	case strings.TrimSpace(prompt) == "":
		return devin.ChatRequest{}, "", fmt.Errorf("prompt must not be empty")
	case req.Suffix != nil && *req.Suffix != "":
		return devin.ChatRequest{}, "", fmt.Errorf("suffix is not supported")
	case req.N != nil && *req.N != 1:
		return devin.ChatRequest{}, "", fmt.Errorf("only n=1 is supported")
	case req.BestOf != nil && *req.BestOf > 1:
		return devin.ChatRequest{}, "", fmt.Errorf("best_of is not supported")
	case req.Logprobs != nil && *req.Logprobs > 0:
		return devin.ChatRequest{}, "", fmt.Errorf("logprobs are not supported")
	}
	chat := req.AsChat()
	chat.Messages = []openai.Message{
		{Role: "system", Content: json.RawMessage(mustJSON(completionSystem))},
		{Role: "user", Content: json.RawMessage(mustJSON(prompt))},
	}
	dreq, err := toDevinRequest(chat)
	if err != nil {
		return dreq, "", err
	}
	return dreq, prompt, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// completions handles POST /v1/completions, the legacy text API, on top of
// the chat upstream. Thinking text is dropped: the format has no field for it.
func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
	raw, status, msg := readBody(r)
	if status != 0 {
		openai.WriteError(w, status, msg)
		return
	}
	var req openai.CompletionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		openai.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	dreq, prompt, err := toDevinCompletion(req)
	if err != nil {
		openai.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if status, msg := s.checkModel(r.Context(), &dreq, req.Model); status != 0 {
		openai.WriteError(w, status, msg)
		return
	}

	ctx, tr := s.startTrace(r, "/v1/completions", req.Model, dreq, req.Stream)
	base := openai.Completion{ID: "cmpl-" + randID(), Object: "text_completion", Created: time.Now().Unix(), Model: req.Model}
	if base.Model == "" {
		base.Model = "devin"
	}
	echo := ""
	if req.Echo {
		echo = prompt
	}

	if req.Stream {
		s.streamTextCompletion(ctx, tr, w, dreq, base, echo, req.AsChat().IncludeUsage())
		return
	}

	text, _, res, err := s.collectChat(ctx, dreq)
	s.finishTrace(ctx, tr, res, err)
	if err != nil {
		status, msg := upstreamStatus(err)
		setRetryAfter(w, status, err)
		openai.WriteError(w, status, msg)
		return
	}
	reason := finishReason(res)
	base.Choices = []openai.CompletionChoice{{Text: echo + text, FinishReason: &reason}}
	base.Usage = usageOf(res)
	writeJSON(w, http.StatusOK, base)
}

// streamTextCompletion writes the completion as SSE chunks. As in
// streamCompletion, headers wait for the first output so early failures
// keep a real HTTP status and may fail over.
func (s *Server) streamTextCompletion(ctx context.Context, tr *chatTrace, w http.ResponseWriter, dreq devin.ChatRequest, base openai.Completion, echo string, includeUsage bool) {
	flusher, _ := w.(http.Flusher)
	headerSent := false
	send := func(choices []openai.CompletionChoice, usage *openai.Usage) error {
		if !headerSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			headerSent = true
		}
		c := base
		c.Choices, c.Usage = choices, usage
		b, _ := json.Marshal(c)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err // client went away
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	pending := echo // echoed prompt goes out with the first output
	res, err := s.streamWithFailover(ctx, dreq, func(d devin.Delta) error {
		if d.Text == "" {
			return nil
		}
		text := pending + d.Text
		pending = ""
		return send([]openai.CompletionChoice{{Text: text}}, nil)
	})
	s.finishTrace(ctx, tr, res, err)
	if err != nil {
		status, msg := upstreamStatus(err)
		if !headerSent {
			setRetryAfter(w, status, err)
			openai.WriteError(w, status, msg)
			return
		}
		b, _ := json.Marshal(openai.Error{Error: openai.ErrorBody{Message: msg, Type: openai.ErrorType(status), Code: &status}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	reason := finishReason(res)
	if err := send([]openai.CompletionChoice{{Text: pending, FinishReason: &reason}}, nil); err != nil {
		return
	}
	if includeUsage {
		u := usageOf(res)
		if u == nil {
			u = &openai.Usage{}
		}
		if err := send([]openai.CompletionChoice{}, u); err != nil {
			return
		}
	}
	io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// embeddings answers POST /v1/embeddings: Devin offers no embedding model,
// so the proxy says so in the OpenAI error shape instead of a bare 404.
func (s *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	openai.WriteError(w, http.StatusNotImplemented, "embeddings are not supported: Devin offers no embedding models; use a separate embeddings provider")
}
