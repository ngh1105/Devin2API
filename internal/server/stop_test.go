package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
)

// collect feeds text deltas through a stopWatcher and returns what it
// forwarded and the stop sequence that matched, if any.
func collect(stops []string, chunks ...string) (string, string) {
	var out strings.Builder
	w := newStopWatcher(stops, func(d devin.Delta) error { out.WriteString(d.Text); return nil })
	for _, c := range chunks {
		if err := w.feed(devin.Delta{Text: c}); err == errStopSequence {
			return out.String(), w.hit
		}
	}
	w.flush()
	return out.String(), ""
}

func TestStopWatcher(t *testing.T) {
	cases := []struct {
		stops  []string
		chunks []string
		want   string
		hit    string
	}{
		{[]string{"END"}, []string{"HELLO E", "ND world"}, "HELLO ", "END"}, // split across deltas
		{[]string{"END"}, []string{"abc E"}, "abc E", ""},                   // held tail flushed at the end
		{[]string{"\n\n", "x"}, []string{"ab", "x\n\nc"}, "ab", "x"},        // earliest match wins
		{[]string{"é!"}, []string{"caf", "é", "!"}, "caf", "é!"},            // multi-byte
		{[]string{"zz"}, []string{"Xin chào", " 👋"}, "Xin chào 👋", ""},      // no match, UTF-8 intact
	}
	for i, c := range cases {
		got, hit := collect(c.stops, c.chunks...)
		if got != c.want || hit != c.hit {
			t.Errorf("case %d: got %q hit %q, want %q hit %q", i, got, hit, c.want, c.hit)
		}
	}
}

func TestChatStopSequence(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1") // upstream answers "po" + "ng"
	rec := postChat(srv, `{"messages":[{"role":"user","content":"hi"}],"stop":"ng"}`)
	var resp openai.ChatCompletion
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 200 || resp.Choices[0].Message.Content != "po" || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}],"stop":42}`,
		`{"messages":[{"role":"user","content":"hi"}],"stop":["a","b","c","d","e"]}`,
	} {
		if rec := postChat(srv, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, rec.Code)
		}
	}
}

func TestMessagesStopSequence(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postV1(srv, "/v1/messages", `{"max_tokens":10,"stream":true,"stop_sequences":["ng"],"messages":[{"role":"user","content":"hi"}]}`)
	names, data := sseEvents(t, rec.Body.String())
	text := ""
	var delta map[string]any
	for i, n := range names {
		if n == "content_block_delta" {
			text += data[i]["delta"].(map[string]any)["text"].(string)
		}
		if n == "message_delta" {
			delta = data[i]["delta"].(map[string]any)
		}
	}
	if text != "po" || delta["stop_reason"] != "stop_sequence" || delta["stop_sequence"] != "ng" {
		t.Errorf("text %q delta %v", text, delta)
	}
}

// --- tool_choice ---

func lastBody(bodies *[][]byte) string { return string((*bodies)[len(*bodies)-1]) }

func TestToolChoiceForms(t *testing.T) {
	srv, bodies := toolUpstream(t)
	twoTools := `"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}},{"type":"function","function":{"name":"get_time","parameters":{"type":"object"}}}]`

	postChat(srv, `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"get_time"}},`+twoTools+`}`)
	if b := lastBody(bodies); strings.Contains(b, "get_weather") || !strings.Contains(b, `calling the "get_time" tool`) {
		t.Error("named choice not applied")
	}

	postChat(srv, `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"required",`+twoTools+`}`)
	if b := lastBody(bodies); !strings.Contains(b, "get_weather") || !strings.Contains(b, "get_time") || !strings.Contains(b, "Tool use is required") {
		t.Error("required choice not applied")
	}

	postV1(srv, "/v1/messages", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"any"},"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`)
	if !strings.Contains(lastBody(bodies), "Tool use is required") {
		t.Error("anthropic any not applied")
	}
	postV1(srv, "/v1/responses", `{"input":"hi","tool_choice":{"type":"function","name":"get_weather"},"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)
	if !strings.Contains(lastBody(bodies), "Tool use is required") {
		t.Error("responses named choice not applied")
	}

	n := len(*bodies)
	for path, body := range map[string]string{
		"/v1/chat/completions": `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`,
		"/v1/messages":         `{"max_tokens":5,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"tool","name":"nope"},"tools":[{"name":"a","input_schema":{"type":"object"}}]}`,
		"/v1/responses":        `{"input":"hi","tool_choice":"sometimes"}`,
	} {
		if rec := postV1(srv, path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", path, rec.Code)
		}
	}
	if len(*bodies) != n {
		t.Error("invalid tool_choice reached upstream")
	}
}

// --- previous_response_id ---

func TestResponsesPreviousResponseID(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1") // upstream answers "pong"
	rec := postV1(srv, "/v1/responses", `{"input":"first question"}`)
	var first struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &first)

	rec = postV1(srv, "/v1/responses", `{"previous_response_id":"`+first.ID+`","input":"second question"}`)
	var second struct {
		ID                 string `json:"id"`
		PreviousResponseID string `json:"previous_response_id"`
		OutputText         string `json:"output_text"`
	}
	json.Unmarshal(rec.Body.Bytes(), &second)
	if rec.Code != 200 || second.PreviousResponseID != first.ID || second.OutputText != "pong" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}

	// The stored conversation is the first exchange plus the new turn.
	prompts, ok := srv.storedResponses.get(second.ID)
	want := []string{"first question", "pong", "second question", "pong"}
	if !ok || len(prompts) != len(want) {
		t.Fatalf("stored %d prompts", len(prompts))
	}
	for i, p := range prompts {
		if p.Text != want[i] {
			t.Errorf("prompt %d = %q, want %q", i, p.Text, want[i])
		}
	}

	// store:false is not kept, so it cannot be continued.
	rec = postV1(srv, "/v1/responses", `{"input":"x","store":false}`)
	var third struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &third)
	if rec := postV1(srv, "/v1/responses", `{"previous_response_id":"`+third.ID+`","input":"y"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("continuing an unstored response: status %d", rec.Code)
	}
}

func TestResponseStoreEviction(t *testing.T) {
	var s responseStore
	for i := 0; i < responseStoreSize+10; i++ {
		s.put(fmt.Sprintf("resp_%d", i), nil)
	}
	if len(s.items) != responseStoreSize {
		t.Errorf("store holds %d, cap %d", len(s.items), responseStoreSize)
	}
	if _, ok := s.get("resp_0"); ok {
		t.Error("oldest entry not evicted")
	}
	if _, ok := s.get(fmt.Sprintf("resp_%d", responseStoreSize+9)); !ok {
		t.Error("newest entry missing")
	}
}
