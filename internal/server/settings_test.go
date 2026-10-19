package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

func putSettings(srv *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodPut, "/admin/api/settings", strings.NewReader(body)))
	return rec
}

// TestSettingsPersist: valid settings are normalized, saved to the data dir
// and loaded by the next server; invalid ones are refused unchanged.
func TestSettingsPersist(t *testing.T) {
	dir := t.TempDir()
	srv := New(config.Config{Listen: ":0", DataDir: dir}, mustAccounts(t), mustTokens(t))
	rec := putSettings(srv, `{"upstreams":["https://a.example.com/"," ","https://a.example.com","http://127.0.0.1:9"],"system_prompt":"Be terse.","system_prompt_enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got settingsView
	json.Unmarshal(rec.Body.Bytes(), &got)
	if strings.Join(got.Upstreams, ",") != "https://a.example.com,http://127.0.0.1:9" || got.SystemPromptMode != "prepend" {
		t.Errorf("normalized = %+v", got.runtimeSettings)
	}

	for _, bad := range []string{
		`{"upstreams":["ftp://x"]}`,
		`{"upstreams":["https://x?token=1"]}`,
		`{"system_prompt_mode":"replace"}`,
		`{"unknown":1}`,
		`{bad`,
	} {
		if rec := putSettings(srv, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", bad, rec.Code)
		}
	}

	again := New(config.Config{Listen: ":0", DataDir: dir}, mustAccounts(t), mustTokens(t))
	if v := again.settings.get(); len(v.Upstreams) != 2 || v.SystemPrompt != "Be terse." || !v.SystemPromptEnabled {
		t.Errorf("reloaded = %+v", v)
	}
}

func TestSettingsNeedCSRFHeader(t *testing.T) {
	srv := New(config.Config{Listen: ":0"}, mustAccounts(t), mustTokens(t))
	req := adminRequest(http.MethodPut, "/admin/api/settings", strings.NewReader(`{}`))
	req.Header.Del("X-Devin2API")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("PUT without X-Devin2API accepted")
	}
}

// recordingUpstream answers chat with "ok" and records request bodies.
func recordingUpstream(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Write(connectFrame(0, devin.PwFieldString(3, "ok")))
		w.Write(connectFrame(2, []byte("{}")))
	}))
	t.Cleanup(up.Close)
	return up, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), bodies...) }
}

func settingsChatServer(t *testing.T, s runtimeSettings) *Server {
	t.Helper()
	accs := mustAccounts(t)
	accs.Save(store.AccountFromLogin("u", "usr_s", "org", "eyJtok", "", nil))
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	if _, err := srv.settings.set(s); err != nil {
		t.Fatal(err)
	}
	return srv
}

// TestUpstreamFailover: a dead or 503 endpoint is skipped for the next one
// with the same account.
func TestUpstreamFailover(t *testing.T) {
	good, bodies := recordingUpstream(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused from now on
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unavailable.Close()

	srv := settingsChatServer(t, runtimeSettings{Upstreams: []string{deadURL, unavailable.URL, good.URL}})
	rec := postChat(srv, `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if n := len(bodies()); n != 1 {
		t.Errorf("good endpoint called %d times", n)
	}
}

// TestUpstreamNoFailoverOnRequestError: a 400-class answer is the
// request's fault, so the next endpoint is not tried.
func TestUpstreamNoFailoverOnRequestError(t *testing.T) {
	good, bodies := recordingUpstream(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(connectFrame(2, []byte(`{"error":{"code":"invalid_argument","message":"bad"}}`)))
	}))
	defer bad.Close()
	srv := settingsChatServer(t, runtimeSettings{Upstreams: []string{bad.URL, good.URL}})
	if rec := postChat(srv, `{"messages":[{"role":"user","content":"hi"}]}`); rec.Code == http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
	if n := len(bodies()); n != 0 {
		t.Errorf("second endpoint called %d times", n)
	}
}

func TestGlobalSystemPrompt(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    runtimeSettings
		want []string // in order
		not  string
	}{
		{"prepend", runtimeSettings{SystemPrompt: "GLOBAL", SystemPromptEnabled: true}, []string{"GLOBAL", "OWN"}, ""},
		{"append", runtimeSettings{SystemPrompt: "GLOBAL", SystemPromptEnabled: true, SystemPromptMode: "append"}, []string{"OWN", "GLOBAL"}, ""},
		{"disabled", runtimeSettings{SystemPrompt: "GLOBAL"}, []string{"OWN"}, "GLOBAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, bodies := recordingUpstream(t)
			tc.s.Upstreams = []string{up.URL}
			srv := settingsChatServer(t, tc.s)
			postChat(srv, `{"messages":[{"role":"system","content":"OWN"},{"role":"user","content":"hi"}]}`)
			b := bodies()
			if len(b) != 1 {
				t.Fatalf("%d upstream calls", len(b))
			}
			last := -1
			for _, w := range tc.want {
				i := strings.Index(b[0], w)
				if i <= last {
					t.Errorf("%q missing or out of order", w)
				}
				last = i
			}
			if tc.not != "" && strings.Contains(b[0], tc.not) {
				t.Errorf("%q sent while disabled", tc.not)
			}
		})
	}
}

func TestUpstreamTest(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer up.Close()
	srv := settingsChatServer(t, runtimeSettings{})
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, adminRequest(http.MethodPost, "/admin/api/settings/upstream-test", strings.NewReader(body)))
		return rec
	}
	rec := post(`{"url":"` + up.URL + `"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), `"status":401`) {
		t.Errorf("reachable: %d %s", rec.Code, rec.Body)
	}
	if gotAuth != "" {
		t.Error("endpoint test sent an Authorization header")
	}
	if rec := post(`{"url":"not a url"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad url: %d", rec.Code)
	}
}
