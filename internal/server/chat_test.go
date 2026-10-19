package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
	"github.com/ngh1105/Devin2API/internal/store"
)

func connectFrame(flag byte, payload []byte) []byte {
	out := make([]byte, 5, 5+len(payload))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}

// fakeDevin answers GetChatMessage per session token: tokens listed in
// trailers get that end-of-stream JSON, all others stream "pong".
type fakeDevin struct {
	mu       sync.Mutex
	calls      []string // chat Authorization headers seen, in order
	modelCalls int
	trailers   map[string]string
}

// TestModelsFromUpstream: /v1/models lists the live catalog after the
// "devin" alias, caches it, and chat maps a family name onto a catalog UID.
func TestModelsFromUpstream(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, v1Request(http.MethodGet, "/v1/models", nil))
		var resp struct {
			Data []struct {
				ID            string `json:"id"`
				ContextLength int    `json:"context_length"`
			} `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.Data) != 2 || resp.Data[0].ID != "devin" || resp.Data[1].ID != "gpt-5-4-medium" || resp.Data[1].ContextLength != 400000 {
			t.Fatalf("models = %+v", resp.Data)
		}
	}
	if fake.modelCalls != 1 {
		t.Errorf("upstream catalog fetched %d times, want 1 (cached)", fake.modelCalls)
	}
	if got := devin.MatchModel("gpt-5.4", srv.cachedModels()); got != "gpt-5-4-medium" {
		t.Errorf("MatchModel = %q", got)
	}
}

// TestChatRejectsUnknownModel: with a loaded catalog, unknown models get a
// local 400 without spending an upstream call; known family names pass.
func TestChatRejectsUnknownModel(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	srv.fetchModels(context.Background())

	rec := postChat(srv, `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown model") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if len(fake.calls) != 0 {
		t.Error("unknown model reached upstream")
	}
	if rec := postChat(srv, `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Errorf("family name: status %d body %s", rec.Code, rec.Body)
	}
}

// TestModelsWithoutAccount lists only the alias and never calls upstream.
func TestModelsWithoutAccount(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, v1Request(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"devin"`) || fake.modelCalls != 0 {
		t.Errorf("status %d body %s calls %d", rec.Code, rec.Body, fake.modelCalls)
	}
}

func (f *fakeDevin) handler(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	if strings.HasSuffix(r.URL.Path, "/GetCliModelConfigs") {
		f.mu.Lock()
		f.modelCalls++
		f.mu.Unlock()
		cfg := append(devin.PwFieldString(1, "GPT-5.4 Medium"), devin.PwFieldVarint(18, 400000)...)
		cfg = append(cfg, devin.PwFieldString(22, "gpt-5-4-medium")...)
		w.Write(devin.PwFieldBytes(1, cfg))
		return
	}
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	f.calls = append(f.calls, auth)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/connect+proto")
	for tok, trailer := range f.trailers {
		if strings.Contains(auth, tok) {
			w.Write(connectFrame(2, []byte(trailer)))
			return
		}
	}
	usage := devin.PwFieldBytes(7, append(devin.PwFieldVarint(2, 10), devin.PwFieldVarint(3, 2)...))
	w.Write(connectFrame(0, devin.PwFieldString(3, "po")))
	w.Write(connectFrame(0, append(devin.PwFieldString(3, "ng"), usage...)))
	w.Write(connectFrame(0, devin.PwFieldVarint(5, 2)))
	w.Write(connectFrame(2, []byte("{}")))
}

// chatTestServer wires a Server to a fake upstream with the given accounts.
func chatTestServer(t *testing.T, fake *fakeDevin, sessionTokens ...string) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(up.Close)
	accs := mustAccounts(t)
	for i, tok := range sessionTokens {
		a := store.AccountFromLogin("u", "usr_"+string(rune('a'+i)), "org", tok, "", nil)
		if err := accs.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.loginServerBase = up.URL
	return srv
}

func postChat(srv *Server, body string) *httptest.ResponseRecorder {
	req := v1Request(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestChatCompletionNonStream(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postChat(srv, `{"model":"devin","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp openai.ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	c := resp.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content != "pong" || c.FinishReason != "stop" {
		t.Errorf("choice = %+v", c)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 12 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestChatCompletionStream(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postChat(srv, `{"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}],"stream":true}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d ct %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var text strings.Builder
	var finish string
	sawRole, sawDone := false, false
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var c openai.ChatChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		d := c.Choices[0]
		sawRole = sawRole || d.Delta.Role == "assistant"
		text.WriteString(d.Delta.Content)
		if d.FinishReason != nil {
			finish = *d.FinishReason
		}
	}
	if text.String() != "pong" || finish != "stop" || !sawRole || !sawDone {
		t.Errorf("text=%q finish=%q role=%v done=%v", text.String(), finish, sawRole, sawDone)
	}
}

// TestChatFailoverOnExpiredToken: the first account's token is rejected, the
// request succeeds on the second, and the first is flagged for re-login.
func TestChatFailoverOnExpiredToken(t *testing.T) {
	fake := &fakeDevin{trailers: map[string]string{
		"eyJdead": `{"error":{"code":"unauthenticated","message":"bad token"}}`,
	}}
	srv := chatTestServer(t, fake, "eyJdead", "eyJlive")
	for i := 0; i < 2; i++ { // both round-robin starting points
		rec := postChat(srv, `{"messages":[{"role":"user","content":"ping"}]}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
			t.Fatalf("attempt %d: status %d body %s", i, rec.Code, rec.Body)
		}
	}
	accs, _ := srv.accounts.List()
	for _, a := range accs {
		dead := strings.Contains(a.Tokens.SessionToken, "eyJdead")
		if dead != (a.UsageErr != "") {
			t.Errorf("account %s: usage_error=%q", a.Tokens.SessionToken, a.UsageErr)
		}
	}
}

func TestChatQuotaExhaustedAllAccounts(t *testing.T) {
	quota := `{"error":{"code":"failed_precondition","message":"daily quota exhausted"}}`
	srv := chatTestServer(t, &fakeDevin{trailers: map[string]string{"eyJa": quota, "eyJb": quota}}, "eyJa", "eyJb")
	rec := postChat(srv, `{"messages":[{"role":"user","content":"ping"}],"stream":true}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429: %s", rec.Code, rec.Body)
	}
	stats := httptest.NewRecorder()
	srv.Handler().ServeHTTP(stats, adminRequest(http.MethodGet, "/admin/api/stats", nil))
	if !strings.Contains(stats.Body.String(), `"failed":1`) || !strings.Contains(stats.Body.String(), `"requests":1`) {
		t.Errorf("stats = %s", stats.Body)
	}
}

func TestChatSkipsDisabledAccounts(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJoff")
	accs, _ := srv.accounts.List()
	accs[0].Enabled = false
	srv.accounts.Save(accs[0])
	if rec := postChat(srv, `{"messages":[{"role":"user","content":"ping"}]}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", rec.Code)
	}
	if len(fake.calls) != 0 {
		t.Error("disabled account was used")
	}
}

func TestChatRejectsUnsupportedInput(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	for _, body := range []string{
		`{"messages":[{"role":"tool","content":"x"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		`{"messages":[{"role":"system","content":"only system"}]}`,
	} {
		if rec := postChat(srv, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
}

// TestModelsMergedAcrossAccounts: each account may see a different catalog;
// the cache holds the union so no account's models are rejected as unknown.
func TestModelsMergedAcrossAccounts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		uid := "model-a"
		if strings.Contains(r.Header.Get("Authorization"), "eyJb") {
			uid = "model-b"
		}
		config := func(id string) []byte {
			return devin.PwFieldBytes(1, append(devin.PwFieldString(1, id), devin.PwFieldString(22, id)...))
		}
		w.Write(append(config(uid), config("shared")...))
	}))
	defer up.Close()
	accs := mustAccounts(t)
	for i, tok := range []string{"eyJa", "eyJb"} {
		accs.Save(store.AccountFromLogin("u", "usr_"+string(rune('a'+i)), "org", tok, "", nil))
	}
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.loginServerBase = up.URL

	got := map[string]int{}
	for _, m := range srv.fetchModels(context.Background()) {
		got[m.UID]++
	}
	if got["model-a"] != 1 || got["model-b"] != 1 || got["shared"] != 1 {
		t.Errorf("merged catalog = %v", got)
	}
}

// TestModelsFetchIgnoresCallerCancel: a client that disconnects mid-fetch
// must not leave the shared cache empty and throttled.
func TestModelsFetchIgnoresCallerCancel(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if models := srv.fetchModels(ctx); len(models) == 0 {
		t.Fatal("cancelled caller left the catalog empty")
	}
}

// toolUpstream answers GetChatMessage with one tool call split across two
// frames, and records each raw request body.
func toolUpstream(t *testing.T) (*Server, *[][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/GetCliModelConfigs") {
			return
		}
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		call := func(id, name, args string) []byte {
			var c []byte
			if id != "" {
				c = append(c, devin.PwFieldString(1, id)...)
			}
			if name != "" {
				c = append(c, devin.PwFieldString(2, name)...)
			}
			c = append(c, devin.PwFieldString(3, args)...)
			return connectFrame(0, devin.PwFieldBytes(6, c))
		}
		w.Write(call("up_1", "get_weather", `{"city":`))
		w.Write(call("", "", `"Hue"}`))
		w.Write(connectFrame(0, devin.PwFieldVarint(5, devin.StopToolCalls)))
		w.Write(connectFrame(2, []byte("{}")))
	}))
	t.Cleanup(up.Close)
	accs := mustAccounts(t)
	accs.Save(store.AccountFromLogin("u", "usr_tool", "org", "eyJtok", "", nil))
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.loginServerBase = up.URL
	return srv, &bodies
}

const weatherTools = `"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`

func TestChatToolCallNonStream(t *testing.T) {
	srv, bodies := toolUpstream(t)
	rec := postChat(srv, `{"messages":[{"role":"user","content":"weather in Hue?"}],`+weatherTools+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp openai.ChatCompletion
	json.Unmarshal(rec.Body.Bytes(), &resp)
	c := resp.Choices[0]
	if c.FinishReason != "tool_calls" || len(c.Message.ToolCalls) != 1 {
		t.Fatalf("choice = %+v", c)
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "up_1" || tc.Type != "function" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Hue"}` || tc.Index != nil {
		t.Errorf("tool call = %+v", tc)
	}
	if !strings.Contains(string((*bodies)[0]), `"required":["city"]`) {
		t.Error("tool schema not forwarded upstream")
	}
}

func TestChatToolCallStream(t *testing.T) {
	srv, _ := toolUpstream(t)
	rec := postChat(srv, `{"stream":true,"messages":[{"role":"user","content":"weather?"}],`+weatherTools+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var args strings.Builder
	var id, name, finish string
	fragments := 0
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c openai.ChatChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q", data)
		}
		ch := c.Choices[0]
		for _, tc := range ch.Delta.ToolCalls {
			fragments++
			if tc.Index == nil || *tc.Index != 0 {
				t.Errorf("fragment index = %v", tc.Index)
			}
			if tc.ID != "" {
				id, name = tc.ID, tc.Function.Name
			}
			args.WriteString(tc.Function.Arguments)
		}
		if ch.FinishReason != nil {
			finish = *ch.FinishReason
		}
	}
	if fragments != 2 || id != "up_1" || name != "get_weather" || args.String() != `{"city":"Hue"}` || finish != "tool_calls" {
		t.Errorf("fragments=%d id=%q name=%q args=%q finish=%q", fragments, id, name, args.String(), finish)
	}
}

// TestChatToolHistoryForwarded: the second agent turn (assistant tool_calls
// + tool result) reaches upstream with the call id intact.
func TestChatToolHistoryForwarded(t *testing.T) {
	srv, bodies := toolUpstream(t)
	rec := postChat(srv, `{"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Hue\"}"}}]},
		{"role":"tool","tool_call_id":"call_9","content":"sunny, 31C"}],`+weatherTools+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	b := string((*bodies)[0])
	for _, want := range []string{"call_9", `{"city":"Hue"}`, "sunny, 31C"} {
		if !strings.Contains(b, want) {
			t.Errorf("upstream request missing %q", want)
		}
	}
}

func TestChatToolChoiceNoneDropsTools(t *testing.T) {
	srv, bodies := toolUpstream(t)
	postChat(srv, `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"none",`+weatherTools+`}`)
	if strings.Contains(string((*bodies)[0]), "get_weather") {
		t.Error("tools sent upstream despite tool_choice none")
	}
}

func TestChatRejectsBadTools(t *testing.T) {
	srv, bodies := toolUpstream(t)
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":""}}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"retrieval","function":{"name":"x"}}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"a"}}]}`,
		`{"messages":[{"role":"user","content":"hi"},{"role":"tool","content":"x"}]}`,
		`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"a"}}]}]}`,
	} {
		if rec := postChat(srv, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
	if len(*bodies) != 0 {
		t.Error("invalid request reached upstream")
	}
}
