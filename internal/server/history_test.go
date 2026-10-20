package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
	"github.com/ngh1105/Devin2API/internal/store"
)

// TestResponseStorePerKey: a response made with one API key cannot be
// continued with another, and the keyless owner is its own namespace.
func TestResponseStorePerKey(t *testing.T) {
	var s responseStore
	s.put("resp_a", "t_a", conversation{prompts: []devin.Prompt{{Source: devin.SourceUser, Text: "a"}}})
	s.put("resp_none", "", conversation{})
	if _, ok := s.get("resp_a", "t_a"); !ok {
		t.Error("owner cannot read its own response")
	}
	if _, ok := s.get("resp_a", "t_b"); ok {
		t.Error("another key read the response")
	}
	if _, ok := s.get("resp_a", ""); ok {
		t.Error("keyless caller read a key's response")
	}
	if _, ok := s.get("resp_none", "t_a"); ok {
		t.Error("key read a keyless response")
	}
}

// TestResponsesPreviousIDAcrossKeys runs the same check through the
// handler: key B gets 400 for key A's response id.
func TestResponsesPreviousIDAcrossKeys(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	a, _ := srv.tokens.Create(store.TokenOptions{Name: "a"})
	b, _ := srv.tokens.Create(store.TokenOptions{Name: "b"})
	rec := keyPost(srv, a.Token, "/v1/responses", `{"input":"first"}`)
	var first struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &first)
	if rec.Code != 200 || first.ID == "" {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	follow := `{"previous_response_id":"` + first.ID + `","input":"again"}`
	if rec := keyPost(srv, b.Token, "/v1/responses", follow); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "per API key") {
		t.Errorf("other key continued the response: %d %s", rec.Code, rec.Body)
	}
	if rec := keyPost(srv, a.Token, "/v1/responses", follow); rec.Code != 200 {
		t.Errorf("owner could not continue: %d %s", rec.Code, rec.Body)
	}
}

// TestResponseStoreConfigured: the limits come from the configuration,
// defaults applying where it is zero.
func TestResponseStoreConfigured(t *testing.T) {
	srv := New(config.Config{Listen: ":0", ResponsesKeep: 2, ResponsesTTL: time.Minute}, mustAccounts(t), mustTokens(t))
	items, bytes, ttl := srv.storedResponses.limits()
	if items != 2 || ttl != time.Minute || bytes != responseStoreBytes {
		t.Errorf("limits = %d %d %v", items, bytes, ttl)
	}
	for i, id := range []string{"r1", "r2", "r3"} {
		srv.storedResponses.put(id, "", conversation{prompts: []devin.Prompt{{Source: devin.SourceUser, Text: strings.Repeat("x", i+1)}}})
	}
	if _, ok := srv.storedResponses.get("r1", ""); ok {
		t.Error("oldest kept past the configured count")
	}
	if _, ok := srv.storedResponses.get("r3", ""); !ok {
		t.Error("newest missing")
	}
}

// TestResponsesAssistantTextAfterCall: an assistant message item that
// follows a function_call item of the same turn joins that turn instead
// of starting a second assistant prompt.
func TestResponsesAssistantTextAfterCall(t *testing.T) {
	var req openai.ResponsesRequest
	err := json.Unmarshal([]byte(`{"input":[
		{"role":"user","content":"weather?"},
		{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{}"},
		{"role":"assistant","content":"Checking the weather."},
		{"type":"function_call_output","call_id":"c1","output":"sunny"},
		{"role":"user","content":"thanks"}
	]}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	dreq, _, err := toDevinFromResponses(req, conversation{})
	if err != nil {
		t.Fatal(err)
	}
	sources := []devin.PromptSource{devin.SourceUser, devin.SourceAssistant, devin.SourceToolResult, devin.SourceUser}
	if len(dreq.Prompts) != len(sources) {
		t.Fatalf("got %d prompts: %+v", len(dreq.Prompts), dreq.Prompts)
	}
	for i, src := range sources {
		if dreq.Prompts[i].Source != src {
			t.Errorf("prompt %d source = %d, want %d", i, dreq.Prompts[i].Source, src)
		}
	}
	if a := dreq.Prompts[1]; a.Text != "Checking the weather." || len(a.ToolCalls) != 1 || a.ToolCalls[0].ID != "c1" {
		t.Errorf("assistant turn = %+v", a)
	}
}

// TestCooldownSweep: setting a cooldown drops the ones that have ended, so
// the map does not grow without bound.
func TestCooldownSweep(t *testing.T) {
	var c cooldowns
	c.set("acc1", "m", time.Hour)
	c.mu.Lock()
	c.until[cooldownKey("acc1", "m")] = time.Now().Add(-time.Second) // ended
	c.until[cooldownKey("acc2", "m")] = time.Now().Add(-time.Second)
	c.mu.Unlock()
	c.set("acc3", "m", time.Hour)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.until) != 1 {
		t.Errorf("map holds %d entries after sweep: %v", len(c.until), c.until)
	}
	if _, ok := c.until[cooldownKey("acc3", "m")]; !ok {
		t.Error("new cooldown missing")
	}
}
