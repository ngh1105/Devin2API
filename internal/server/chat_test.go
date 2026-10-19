package server

import (
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
	calls    []string // Authorization headers seen, in order
	trailers map[string]string
}

func (f *fakeDevin) handler(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
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
