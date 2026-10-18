package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
)

// TestRunLoginSavesAccount runs runLogin against a fake auth server.
// Stdin is non-TTY (pipe), so term.ReadPassword falls back to line read.
func TestRunLoginSavesAccount(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/authorize" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/identifier?state="+url.PathEscape(state))
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/u/login/identifier" && r.Method == "GET" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><form id="f"><input name="state" value="` + state + `"/></form></html>`))
			return
		}
		if path == "/u/login/identifier" && r.Method == "POST" {
			r.ParseForm()
			state := r.FormValue("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/password?state="+url.PathEscape(state))
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/u/login/password" && r.Method == "GET" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><form><input name="state" value="` + state + `"/></form></html>`))
			return
		}
		if path == "/u/login/password" && r.Method == "POST" {
			r.ParseForm()
			code := "c-test"
			loc := "https://app.devin.ai/post-auth?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(r.FormValue("state"))
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/oauth/token" && r.Method == "POST" {
			r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok-ok","refresh_token":"rt-ok","id_token":"idt-ok","token_type":"Bearer","expires_in":3600,"scope":"openid profile email offline_access"}`))
			return
		}
		if path == "/userinfo" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sub":"uid-abc","email":"test@example.com","name":"Test User","picture":"https://example.com/pic.jpg"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	tmp := t.TempDir()

	cfg := config.Config{
		Listen:  "127.0.0.1:0",
		DataDir: tmp,
		BaseURL: ts.URL,
	}

	stdin := strings.NewReader("test@example.com\nsupersecret\n")
	var buf strings.Builder

	err := runLogin(context.Background(), &buf, stdin, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// --- assertions -----------------------------------------------------------

	out := buf.String()
	if !strings.Contains(out, "saved ") {
		t.Fatalf("expected 'saved' in output, got %q", out)
	}

	entries, err := os.ReadDir(tmp + "/accounts/")
	if err != nil {
		t.Fatal(err)
	}
	var accFile string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			accFile = e.Name()
			break
		}
	}
	if accFile == "" {
		t.Fatal("no account files written")
	}

	data, err := os.ReadFile(tmp + "/accounts/" + accFile)
	if err != nil {
		t.Fatal(err)
	}

	var acct map[string]any
	if err := json.Unmarshal(data, &acct); err != nil {
		t.Fatal(err)
	}

	tokens, ok := acct["tokens"].(map[string]any)
	if !ok || tokens == nil {
		t.Fatal("no 'tokens' object in saved account")
	}
	if accessToken, ok := tokens["access_token"].(string); !ok || accessToken == "" {
		t.Error("missing access_token in saved account tokens")
	}

	if sub, ok := acct["sub"].(string); !ok || sub != "uid-abc" {
		t.Errorf("sub: got %v, want %q", acct["sub"], "uid-abc")
	}

	// Password must NOT appear in the saved file bytes.
	if strings.Contains(string(data), "supersecret") {
		t.Error("password found in saved account file — security violation")
	}
}

var _ = time.NewTimer(0) // ensure time import is used
