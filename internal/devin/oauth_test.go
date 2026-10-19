package devin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
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

// ---------------------------------------------------------------------------
// Callback integration tests (hit the real LoginState callback handler)
// ---------------------------------------------------------------------------

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

// TestStartLoginCallbackStateMismatch: a callback with the wrong state is
// refused with 400 and does not end the login; the real redirect still works.
func TestStartLoginCallbackStateMismatch(t *testing.T) {
	s, err := StartLogin("http://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if s.State == s.Verifier {
		t.Fatal("state must not reveal the PKCE verifier")
	}
	if strings.Contains(s.LoginURL, s.Verifier) {
		t.Fatal("login URL contains the PKCE verifier")
	}

	base := "http://127.0.0.1:" + strconv.Itoa(s.Port) + "/callback"
	for _, q := range []string{"?code=bad&state=wrong-state", "", "?code=bad"} {
		resp, err := http.Get(base + q)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || strings.Contains(string(body), s.State) {
			t.Errorf("%q: status %d body %q", q, resp.StatusCode, body)
		}
	}
	if _, err := s.Wait(context.Background(), 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("stray callbacks ended the login: %v", err)
	}

	resp, err := http.Get(base + "?code=good&state=" + url.QueryEscape(s.State))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	res, err := s.Wait(context.Background(), time.Second)
	if err != nil || res.Code != "good" {
		t.Fatalf("real callback: %v %+v", err, res)
	}
}

// TestStartLoginCallbackError: an error redirect with the right state
// fails the login.
func TestStartLoginCallbackError(t *testing.T) {
	s, err := StartLogin("http://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(s.Port) + "/callback?error=access_denied&state=" + url.QueryEscape(s.State))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := s.Wait(context.Background(), time.Second); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("err = %v", err)
	}
}

// TestWaitAfterStopReturnsImmediately verifies that Stop() delivers a terminal
// result so Wait returns immediately (via error) instead of blocking for the full
// timeout duration.
func TestWaitAfterStopReturnsImmediately(t *testing.T) {
	s, err := StartLogin("")
	if err != nil {
		t.Fatal(err)
	}

	// Call Stop synchronously BEFORE Wait — no concurrent goroutines.
	// Stop sends a terminal result on resultCh which Wait will receive.
	s.Stop()

	result, waitErr := s.Wait(context.Background(), 5*time.Minute)
	if result.Code != "" {
		t.Errorf("expected empty code after stop, got %q", result.Code)
	}
	if waitErr == nil {
		t.Fatal("expected error from Wait after Stop")
	}
	if !strings.Contains(waitErr.Error(), "stopped") {
		t.Errorf("error should mention 'stopped', got: %v", waitErr)
	}
}
