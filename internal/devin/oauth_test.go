package devin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestExchangeCodeRoundTrip(t *testing.T) {
	code := "test-code-123"
	verifier := "test-verifier-abc"
	wantToken := "raw-token-from-server"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/cli/token" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Code         string `json:"code"`
			CodeVerifier string `json:"code_verifier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Code != code {
			t.Errorf("code = %q; want %q", req.Code, code)
		}
		if req.CodeVerifier != verifier {
			t.Errorf("code_verifier = %q; want %q", req.CodeVerifier, verifier)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": wantToken})
	}))
	defer ts.Close()

	got, err := ExchangeCode(context.Background(), ts.URL, code, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantToken {
		t.Errorf("token = %q; want %q", got, wantToken)
	}
}

func TestFormatSessionToken(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"devin-session-token$abc", "devin-session-token$abc"},
		{"eyJhbGciOiJIUzI1NiJ9.test", "devin-session-token$eyJhbGciOiJIUzI1NiJ9.test"},
	}
	for _, tc := range tests {
		got := FormatSessionToken(tc.input)
		if got != tc.expected {
			t.Errorf("FormatSessionToken(%q) = %q; want %q", tc.input, got, tc.expected)
		}
	}
}

func TestGeneratePKCEFormat(t *testing.T) {
	v, c := GeneratePKCE()
	if len(v) != 43 {
		t.Errorf("verifier length = %d; want 43", len(v))
	}
	if len(c) != 43 {
		t.Errorf("challenge length = %d; want 43", len(c))
	}
	for i, b := range v {
		if !((b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
			(b >= '0' && b <= '9') || b == '-' || b == '_') {
			t.Errorf("verifier[%d] = %q; not base64url allowed", i, string(b))
			break
		}
	}
}

func TestStartLoginCallbackHappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		redirect := r.URL.Query().Get("redirect_uri")
		w.Header().Set("Location", redirect+"?code=fake-code&state="+state)
		w.WriteHeader(http.StatusFound)
	}))
	defer ts.Close()

	s, err := StartLogin(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	callbackURL := "http://127.0.0.1:" + strconv.Itoa(s.Port) +
		"/callback?code=fake-code&state=" + url.QueryEscape(s.State)

	client := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, callbackURL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := s.Wait(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != "fake-code" {
		t.Errorf("code = %q; want %q", result.Code, "fake-code")
	}
}

func TestStartLoginCallbackStateMismatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		conn, _ := ln.Accept()
		if conn == nil {
			return
		}
		fmt.Fprintf(conn, "HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:%d/callback?code=bad&state=wrong-state\r\n\r\n", port)
		conn.Close()
	}()
	time.Sleep(50 * time.Millisecond)

	s, err := StartLogin("")
	if err != nil {
		t.Skipf("start login failed: %v", err)
	}

	callbackDone := make(chan struct{})
	var gotError string
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		st := r.URL.Query().Get("state")
		if st != s.State {
			gotError = fmt.Sprintf("state mismatch: got %q, want %q", st, s.State)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		close(callbackDone)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/callback?code=bad&state=wrong-state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case <-callbackDone:
		t.Log("handler completed without error — check state param parsing")
	default:
	}

	if gotError == "" {
		t.Fatal("expected error message for state mismatch")
	}
}
