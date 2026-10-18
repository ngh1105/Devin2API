package devin

import (
	"context"
	"encoding/json"
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

// TestStartLoginCallbackStateMismatch sends an HTTP GET to the real callback
// endpoint with a wrong state param, verifies Wait surfaces the mismatch error.
func TestStartLoginCallbackStateMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.URL.Query().Get("state")
		redirect := r.URL.Query().Get("redirect_uri")
		w.Header().Set("Location", redirect+"?code=bad&state=wrong-state")
		w.WriteHeader(http.StatusFound)
	}))
	defer ts.Close()

	s, err := StartLogin(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// Simulate browser redirect: hit the bound loopback callback with wrong state.
	callbackURL := "http://127.0.0.1:" + strconv.Itoa(s.Port) +
		"/callback?code=bad&state=wrong-state"
	client := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, callbackURL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = s.Wait(ctx, time.Second)
	if err == nil {
		t.Fatal("expected error for state mismatch")
	}
	if !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("error = %q; expected 'state mismatch'", err.Error())
	}
}

// TestStartLoginCallbackMissingParams hits /callback without code/state,
// verifying Wait returns a missing-params error.
func TestStartLoginCallbackMissingParams(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1:"+strconv.Itoa(0)+"/callback")
		w.WriteHeader(http.StatusFound)
	}))
	defer ts.Close()

	s, err := StartLogin(ts.URL)
	if err != nil {
		t.Skipf("start login failed: %v", err)
	}
	defer s.Stop()

	// Directly hit the callback with no params.
	cbURL := "http://127.0.0.1:" + strconv.Itoa(s.Port) + "/callback"
	resp, err := http.DefaultClient.Get(cbURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = s.Wait(ctx, time.Second)
	if err == nil {
		t.Fatal("expected error for missing params")
	}
	if !strings.Contains(err.Error(), "missing code or state") {
		t.Errorf("error = %q; expected 'missing code or state'", err.Error())
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
