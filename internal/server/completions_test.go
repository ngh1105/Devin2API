package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
	"github.com/ngh1105/Devin2API/internal/store"
)

// TestCompletionNonStream: the legacy format wraps the chat reply, and the
// prompt reaches upstream as a user message under the continuation system
// prompt.
func TestCompletionNonStream(t *testing.T) {
	var body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		usage := devin.PwFieldBytes(7, append(devin.PwFieldVarint(2, 10), devin.PwFieldVarint(3, 2)...))
		w.Write(connectFrame(0, append(devin.PwFieldString(3, " there"), usage...)))
		w.Write(connectFrame(2, []byte("{}")))
	}))
	defer up.Close()
	accs := mustAccounts(t)
	accs.Save(store.AccountFromLogin("u", "usr_c", "org", "eyJtok", "", nil))
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.loginServerBase = up.URL

	rec := postV1(srv, "/v1/completions", `{"model":"devin","prompt":"Hello","max_tokens":20}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		openai.Completion
		Choices []map[string]json.RawMessage `json:"choices"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Object != "text_completion" || !strings.HasPrefix(resp.ID, "cmpl-") || len(resp.Choices) != 1 {
		t.Fatalf("body %s", rec.Body)
	}
	c := resp.Choices[0]
	if string(c["text"]) != `" there"` || string(c["finish_reason"]) != `"stop"` || string(c["logprobs"]) != "null" {
		t.Errorf("choice %s", rec.Body)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 12 {
		t.Errorf("usage %+v", resp.Usage)
	}
	if !strings.Contains(body, "text completion engine") || !strings.Contains(body, "Hello") {
		t.Error("prompt or continuation instruction not sent upstream")
	}

	rec = postV1(srv, "/v1/completions", `{"prompt":["Hello"],"echo":true}`)
	if !strings.Contains(rec.Body.String(), `"text":"Hello there"`) {
		t.Errorf("echo: %s", rec.Body)
	}
}

// TestCompletionStream: text chunks, a final chunk with finish_reason, a
// usage chunk with empty choices when asked, then [DONE].
func TestCompletionStream(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postV1(srv, "/v1/completions", `{"prompt":"p","stream":true,"echo":true,"stream_options":{"include_usage":true}}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var text strings.Builder
	var finish string
	usageChunks := 0
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
	if lines[len(lines)-1] != "data: [DONE]" {
		t.Errorf("stream does not end with [DONE]: %q", lines[len(lines)-1])
	}
	for _, l := range lines[:len(lines)-1] {
		var c openai.Completion
		if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &c); err != nil {
			t.Fatalf("bad chunk %q", l)
		}
		if c.Object != "text_completion" {
			t.Errorf("object %q", c.Object)
		}
		if c.Usage != nil {
			usageChunks++
			if len(c.Choices) != 0 || c.Usage.TotalTokens != 12 {
				t.Errorf("usage chunk %s", l)
			}
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Text)
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if text.String() != "ppong" || finish != "stop" || usageChunks != 1 {
		t.Errorf("text %q finish %q usage chunks %d", text.String(), finish, usageChunks)
	}
}

func TestCompletionStop(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postV1(srv, "/v1/completions", `{"prompt":"p","stop":"n"}`)
	if !strings.Contains(rec.Body.String(), `"text":"po"`) || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Errorf("body %s", rec.Body)
	}
}

// TestCompletionRejects: unsupported options get a local 400 and never
// reach upstream.
func TestCompletionRejects(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	for _, body := range []string{
		`{}`,
		`{"prompt":"  "}`,
		`{"prompt":[1,2,3]}`,
		`{"prompt":["a","b"]}`,
		`{"prompt":"a","n":2}`,
		`{"prompt":"a","suffix":"z"}`,
		`{"prompt":"a","best_of":3}`,
		`{"prompt":"a","logprobs":5}`,
		`{"prompt":"a","stop":5}`,
		`{bad`,
	} {
		rec := postV1(srv, "/v1/completions", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
			t.Errorf("%s: status %d body %s", body, rec.Code, rec.Body)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("%d rejected requests reached upstream", len(fake.calls))
	}
}

// TestEmbeddingsNotSupported: a clear OpenAI-shaped error, not a bare 404.
func TestEmbeddingsNotSupported(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	rec := postV1(srv, "/v1/embeddings", `{"model":"x","input":"hi"}`)
	var e openai.Error
	if rec.Code != http.StatusNotImplemented || json.Unmarshal(rec.Body.Bytes(), &e) != nil || !strings.Contains(e.Error.Message, "embeddings are not supported") {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
	if len(fake.calls) != 0 {
		t.Error("embeddings reached upstream")
	}
}
