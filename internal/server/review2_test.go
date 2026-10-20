package server

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
	"github.com/ngh1105/Devin2API/internal/store"
)

// TestQuotaOnlyForUpstreamRequests: requests refused before they reach
// upstream (unknown route, bad JSON, unsupported endpoint, unknown model)
// do not use the key's quota; a served chat does.
func TestQuotaOnlyForUpstreamRequests(t *testing.T) {
	fake := &fakeDevin{}
	srv := chatTestServer(t, fake, "eyJtok1")
	srv.fetchModels(t.Context())
	key, _ := srv.tokens.Create(store.TokenOptions{Quota: 10})
	cases := []struct{ path, body string }{
		{"/v1/nope", hiChat},
		{"/v1/chat/completions", `{not json`},
		{"/v1/embeddings", `{"input":"x"}`},
		{"/v1/chat/completions", `{"model":"no-such","messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, c := range cases {
		if rec := keyPost(srv, key.Token, c.path, c.body); rec.Code < 400 {
			t.Errorf("%s: status %d", c.path, rec.Code)
		}
	}
	if m, _ := srv.tokens.Match(key.Token); m.Requests != 0 {
		t.Errorf("refused requests used quota: %d", m.Requests)
	}
	gptChat := `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}` // in the fake catalog
	if rec := keyPost(srv, key.Token, "/v1/chat/completions", gptChat); rec.Code != 200 {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body)
	}
	if m, _ := srv.tokens.Match(key.Token); m.Requests != 1 {
		t.Errorf("served request counted %d times", m.Requests)
	}
	// No usable account: 503 before upstream, not counted either.
	accs, _ := srv.accounts.List()
	for _, a := range accs {
		srv.accounts.Delete(a.ID)
	}
	if rec := keyPost(srv, key.Token, "/v1/chat/completions", gptChat); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no account: %d", rec.Code)
	}
	if m, _ := srv.tokens.Match(key.Token); m.Requests != 1 {
		t.Errorf("no-account request used quota: %d", m.Requests)
	}
}

// TestNoFramingHeaders: the panel and admin API refuse to be framed.
func TestNoFramingHeaders(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{})
	for _, p := range []string{"/web/", "/web/app.js", "/admin/api/stats"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, adminRequest(http.MethodGet, p, nil))
		if rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: headers %v", p, rec.Header())
		}
	}
}

// TestStopSequenceLengthCap: an over-long stop sequence is refused.
func TestStopSequenceLengthCap(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	body := `{"model":"devin","stop":"` + strings.Repeat("a", maxStopLen+1) + `","messages":[{"role":"user","content":"hi"}]}`
	if rec := postChat(srv, body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at most") {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
}

// TestResponseStoreCopiesSlices: two follow-ups to one stored response get
// independent tool call slices.
func TestResponseStoreCopiesSlices(t *testing.T) {
	var s responseStore
	calls := make([]devin.ToolCall, 1, 4) // spare capacity, as append leaves
	calls[0] = devin.ToolCall{ID: "c1", Name: "f"}
	s.put("r", "", conversation{prompts: []devin.Prompt{{Source: devin.SourceAssistant, ToolCalls: calls}}})
	a, _ := s.get("r", "")
	b, _ := s.get("r", "")
	a.prompts = appendAssistantCall(a.prompts, devin.ToolCall{ID: "from-a"})
	b.prompts = appendAssistantCall(b.prompts, devin.ToolCall{ID: "from-b"})
	if a.prompts[0].ToolCalls[1].ID != "from-a" || b.prompts[0].ToolCalls[1].ID != "from-b" {
		t.Errorf("follow-ups share a backing array: %+v / %+v", a.prompts[0].ToolCalls, b.prompts[0].ToolCalls)
	}
	if len(calls) != 1 || cap(calls) != 4 {
		t.Error("stored slice changed")
	}
}

// recWriter records segment events.
type recWriter struct{ events []string }

func (r *recWriter) open(i int, s *segment) error { r.events = append(r.events, "open"); return nil }
func (r *recWriter) delta(i int, s *segment, c string) error {
	r.events = append(r.events, "delta"+c)
	return nil
}
func (r *recWriter) close(i int, s *segment) error { r.events = append(r.events, "close"); return nil }

// TestSegmenterLateToolFragment: a fragment for a tool call whose block is
// already closed goes into the final arguments but emits no stream event.
func TestSegmenterLateToolFragment(t *testing.T) {
	w := &recWriter{}
	seg := newSegmenter(w, false)
	seg.feed(devin.Delta{ToolCall: &devin.ToolCallDelta{Index: 0, ID: "c", Name: "f", Arguments: `{"a":`}})
	seg.feed(devin.Delta{Text: "note"})
	seg.feed(devin.Delta{ToolCall: &devin.ToolCallDelta{Index: 0, Arguments: `1}`}})
	seg.finish()
	if seg.segs[0].text.String() != `{"a":1}` {
		t.Errorf("tool args = %q", seg.segs[0].text.String())
	}
	want := []string{"open", `delta{"a":`, "close", "open", "deltanote", "close"}
	if strings.Join(w.events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v", w.events)
	}
}

// TestUpdateTokenNeedsBody: a blank PUT would wipe every option.
func TestUpdateTokenNeedsBody(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{})
	key, _ := srv.tokens.Create(store.TokenOptions{Name: "n", Quota: 5})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodPut, "/admin/api/tokens/"+key.ID, strings.NewReader("")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty PUT: %d %s", rec.Code, rec.Body)
	}
	if m, _ := srv.tokens.Match(key.Token); m.Name != "n" || m.Quota != 5 {
		t.Errorf("options changed: %+v", m)
	}
}

// TestResponsesFollowUpNeedsInput: continuing a stored response needs new
// input, not just reasoning items.
func TestResponsesFollowUpNeedsInput(t *testing.T) {
	var req openai.ResponsesRequest
	json.Unmarshal([]byte(`{"input":[{"type":"reasoning"}]}`), &req)
	prior := conversation{prompts: []devin.Prompt{{Source: devin.SourceUser, Text: "q"}, {Source: devin.SourceAssistant, Text: "a"}}}
	if _, _, err := toDevinFromResponses(req, prior); err == nil || !strings.Contains(err.Error(), "must add") {
		t.Errorf("err = %v", err)
	}
}

// TestStreamWithoutOutputHasRole: a stream that ends with no deltas still
// names the assistant role on its finish chunk.
func TestStreamWithoutOutputHasRole(t *testing.T) {
	fake := &fakeDevin{trailers: map[string]string{"eyJempty": "{}"}}
	srv := chatTestServer(t, fake, "eyJempty")
	rec := postChat(srv, `{"model":"devin","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role":"assistant"`) {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
}

// TestToolParametersNull: "parameters": null means no schema.
func TestToolParametersNull(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	body := `{"model":"devin","tools":[{"type":"function","function":{"name":"f","parameters":null}}],"messages":[{"role":"user","content":"hi"}]}`
	if rec := postChat(srv, body); rec.Code != 200 {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
}

// TestUpstreamMustBeHTTPS: plain http is only accepted for loopback hosts.
func TestUpstreamMustBeHTTPS(t *testing.T) {
	if _, err := normalizeUpstream("http://devin.example.com"); err == nil {
		t.Error("remote http upstream accepted")
	}
	for _, ok := range []string{"http://127.0.0.1:9000", "http://localhost:9000", "https://devin.example.com"} {
		if _, err := normalizeUpstream(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

// TestClientIPv6Block: addresses in one /64 share a login-limit key.
func TestClientIPv6Block(t *testing.T) {
	a := httptest.NewRequest(http.MethodGet, "/", nil)
	a.RemoteAddr = "[2001:db8:1:2:aaaa::1]:5000"
	b := httptest.NewRequest(http.MethodGet, "/", nil)
	b.RemoteAddr = "[2001:db8:1:2:bbbb::9]:5001"
	if clientIP(a) != clientIP(b) {
		t.Errorf("%s vs %s", clientIP(a), clientIP(b))
	}
	c := httptest.NewRequest(http.MethodGet, "/", nil)
	c.RemoteAddr = "10.0.0.1:5"
	if clientIP(c) != "10.0.0.1" {
		t.Errorf("ipv4 = %s", clientIP(c))
	}
}

// TestLoginAttemptsGlobalCap: many addresses together are capped too.
func TestLoginAttemptsGlobalCap(t *testing.T) {
	var a adminAuth
	allowed := 0
	for i := 0; i < loginFailsPerMinuteAll+20; i++ {
		if a.takeAttempt("10.0." + string(rune('a'+i%26)) + ".1") {
			allowed++
		}
	}
	if allowed != loginFailsPerMinuteAll {
		t.Errorf("%d attempts allowed, cap %d", allowed, loginFailsPerMinuteAll)
	}
}

// TestSessionCookieSecureOverTLS: the session cookie is Secure when the
// login came over TLS.
func TestSessionCookieSecureOverTLS(t *testing.T) {
	srv := passwordServer(t)
	req := adminRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"s3cret"}`))
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var secure bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			secure = c.Secure
		}
	}
	if !secure {
		t.Error("session cookie not Secure over TLS")
	}
}

// TestLoginCancelBumpsGen: after Cancel the pending login's generation is
// stale, so its late result is ignored.
func TestLoginCancelBumpsGen(t *testing.T) {
	m := newLoginManager()
	m.mu.Lock()
	gen := m.gen
	m.mu.Unlock()
	m.Cancel()
	if m.isCurrent(gen) {
		t.Error("cancelled login still current")
	}
}

// TestSettingsLoadDropsBadUpstream: an upstream the current rules refuse
// is dropped on load; the other settings are kept.
func TestSettingsLoadDropsBadUpstream(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"upstreams":["http://192.168.1.5:9000","https://ok.example"],"system_prompt":"keep me","system_prompt_enabled":true}`), 0o600)
	var st settingsStore
	st.load(dir)
	v := st.get()
	if v.SystemPrompt != "keep me" || len(v.Upstreams) != 1 || v.Upstreams[0] != "https://ok.example" {
		t.Errorf("settings = %+v", v)
	}
}

// TestStopWatcherResetBetweenEndpoints: text held back from an endpoint
// that failed is not replayed into the next endpoint's output.
func TestStopWatcherResetBetweenEndpoints(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		w.Write(connectFrame(0, devin.PwFieldString(3, "There"))) // a stop prefix, all held
		w.Write(connectFrame(2, []byte(`{"error":{"code":"unavailable","message":"down"}}`)))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		w.Write(connectFrame(0, devin.PwFieldString(3, "The answer is 4.")))
		w.Write(connectFrame(0, devin.PwFieldVarint(5, 2)))
		w.Write(connectFrame(2, []byte("{}")))
	}))
	defer good.Close()
	// Distinct URLs: the settings de-duplicate upstreams.
	srv := settingsChatServer(t, runtimeSettings{Upstreams: []string{bad.URL, good.URL}})
	rec := postChat(srv, `{"model":"devin","stop":["There is"],"messages":[{"role":"user","content":"2+2?"}]}`)
	var out struct {
		Choices []struct{ Message struct{ Content string } }
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || len(out.Choices) != 1 || out.Choices[0].Message.Content != "The answer is 4." {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
}
