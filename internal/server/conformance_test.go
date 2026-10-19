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
	"github.com/ngh1105/Devin2API/internal/store"
)

// TestUpstreamAuthErrorIsNot401: an upstream token rejection must not look
// like a bad client API key (Claude Code would tell the user to /login).
func TestUpstreamAuthErrorIsNot401(t *testing.T) {
	dead := `{"error":{"code":"unauthenticated","message":"bad token"}}`
	srv := chatTestServer(t, &fakeDevin{trailers: map[string]string{"eyJdead": dead}}, "eyJdead")
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		body := `{"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
		rec := postV1(srv, path, body)
		if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "authentication_error") {
			t.Errorf("%s: status %d body %s", path, rec.Code, rec.Body)
		}
	}
}

// TestMaxCompletionTokens: the newer OpenAI parameter caps output.
func TestMaxCompletionTokens(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Write(connectFrame(0, devin.PwFieldString(3, "ok")))
		w.Write(connectFrame(2, []byte("{}")))
	}))
	defer up.Close()
	accs := mustAccounts(t)
	accs.Save(store.AccountFromLogin("u", "usr_m", "org", "eyJtok", "", nil))
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.loginServerBase = up.URL
	postChat(srv, `{"max_completion_tokens":77,"messages":[{"role":"user","content":"hi"}]}`)
	// Config field 8.2 (max tokens) = 77: tag 0x10, varint 77.
	if !strings.Contains(string(got), string([]byte{0x10, 77})) {
		t.Error("max_completion_tokens not sent upstream")
	}
}

// TestToolOnlyReplyHasNullContent: a reply with only tool calls has
// "content": null, as in the OpenAI API.
func TestToolOnlyReplyHasNullContent(t *testing.T) {
	srv, _ := toolUpstream(t)
	rec := postChat(srv, `{"messages":[{"role":"user","content":"weather?"}],`+weatherTools+`}`)
	var raw struct {
		Choices []struct {
			Message map[string]json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(rec.Body.Bytes(), &raw)
	if c := string(raw.Choices[0].Message["content"]); c != "null" {
		t.Errorf("content = %s, want null", c)
	}
}

// TestOpenAIErrorTypes: errors carry the type SDKs classify on.
func TestOpenAIErrorTypes(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postChat(srv, `{bad`)
	if !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Errorf("400 body %s", rec.Body)
	}
	limit := `{"error":{"code":"resource_exhausted","message":"slow down"}}`
	srv = chatTestServer(t, &fakeDevin{trailers: map[string]string{"eyJrl": limit}}, "eyJrl")
	if rec := postChat(srv, `{"messages":[{"role":"user","content":"hi"}]}`); !strings.Contains(rec.Body.String(), `"type":"rate_limit_error"`) {
		t.Errorf("429 body %s", rec.Body)
	}
}

// TestStreamUsageOnlyWhenRequested: usage arrives in its own chunk with
// empty choices when stream_options.include_usage is set, and not otherwise.
func TestStreamUsageOnlyWhenRequested(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	count := func(body string) (usageChunks, emptyChoiceUsage int) {
		for _, line := range strings.Split(body, "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var c struct {
				Choices []json.RawMessage `json:"choices"`
				Usage   json.RawMessage   `json:"usage"`
			}
			json.Unmarshal([]byte(data), &c)
			if c.Usage != nil {
				usageChunks++
				if len(c.Choices) == 0 {
					emptyChoiceUsage++
				}
			}
		}
		return
	}
	rec := postChat(srv, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n, _ := count(rec.Body.String()); n != 0 {
		t.Errorf("usage sent without include_usage: %d chunks", n)
	}
	rec = postChat(srv, `{"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if n, empty := count(rec.Body.String()); n != 1 || empty != 1 {
		t.Errorf("include_usage: %d usage chunks, %d with empty choices", n, empty)
	}
}

// TestResponsesKeepsDeveloperMessages: system/developer input items carry
// over to previous_response_id follow-ups; top-level instructions do not.
func TestResponsesKeepsDeveloperMessages(t *testing.T) {
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Write(connectFrame(0, devin.PwFieldString(3, "ok")))
		w.Write(connectFrame(2, []byte("{}")))
	}))
	defer up.Close()
	accs := mustAccounts(t)
	accs.Save(store.AccountFromLogin("u", "usr_r", "org", "eyJtok", "", nil))
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.loginServerBase = up.URL

	rec := postV1(srv, "/v1/responses", `{"instructions":"TOP-LEVEL","input":[{"role":"developer","content":"Answer in French"},{"role":"user","content":"hi"}]}`)
	var first struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &first)
	postV1(srv, "/v1/responses", `{"previous_response_id":"`+first.ID+`","input":"again"}`)
	second := bodies[len(bodies)-1]
	if !strings.Contains(second, "Answer in French") {
		t.Error("developer message lost on follow-up")
	}
	if strings.Contains(second, "TOP-LEVEL") {
		t.Error("top-level instructions carried over")
	}
}

func TestResponsesContentFilterIncomplete(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	srv.loginServerBase = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(connectFrame(0, devin.PwFieldString(3, "partial")))
		w.Write(connectFrame(0, devin.PwFieldVarint(5, 11)))
		w.Write(connectFrame(2, []byte("{}")))
	})).URL
	rec := postV1(srv, "/v1/responses", `{"input":"hi"}`)
	if !strings.Contains(rec.Body.String(), `"status":"incomplete"`) || !strings.Contains(rec.Body.String(), `"reason":"content_filter"`) {
		t.Errorf("body %s", rec.Body)
	}
}

func TestRetryAfterFormats(t *testing.T) {
	for msg, want := range map[string]int{
		"Your quota resets in 2 days.":  172800,
		"Limit will reset in 5h 30m.":   19800,
		"resets in 45 secs":             45,
		"reset in 1 hour and 5 minutes": 3900,
	} {
		if got := retryAfterSeconds(msg); got != want {
			t.Errorf("%q: %d, want %d", msg, got, want)
		}
	}
}
