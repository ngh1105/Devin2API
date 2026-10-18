package devin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---------- base URLs (vars for test overrides) ------------------------------

var (
	AppBaseURL    = "https://app.devin.ai"
	APIBaseURL    = "https://api.devin.ai"
	ServerBaseURL = "https://server.codeium.com"
)

// ---------- token exchange ----------------------------------------------------

// ExchangeCode POSTs <apiBase>/auth/cli/token with {code, code_verifier} and
// returns the raw token string from {"token": "<raw>"}.
func ExchangeCode(ctx context.Context, apiBase, code, verifier string) (string, error) {
	if apiBase == "" {
		apiBase = APIBaseURL
	}
	type req struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
	}
	body, err := json.Marshal(req{code, verifier})
	if err != nil {
		return "", fmt.Errorf("marshal exchange request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/auth/cli/token", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("exchange request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("exchange failed: status %d", resp.StatusCode)
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	return out.Token, nil
}

// FormatSessionToken formats a raw token value for persistence. Values starting
// with the prefix are kept as-is; all others get the prefix.
func FormatSessionToken(raw string) string {
	if raw == "" {
		return raw
	}
	const prefix = "devin-session-token$"
	if strings.HasPrefix(raw, prefix) {
		return raw
	}
	return prefix + raw
}

// ---------- PKCE ------------------------------------------------------------

// GeneratePKCE produces a 43-char base64url verifier and its SHA-256 S256
// challenge (also base64url no-padding).
func GeneratePKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	io.ReadFull(rand.Reader, b)
	verifier = base64.RawURLEncoding.EncodeToString(b)

	hash := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(hash[:])
	return
}

// ---------- login session -----------------------------------------------------

// LoginState tracks an in-progress CLI login flow.
type LoginState struct {
	LoginURL string // URL to open in browser
	Port     int    // bound loopback port
	State    string // OAuth state param (exported for tests)
	Verifier string // PKCE code_verifier
	code     chan string
	errCh    chan error
	done     chan struct{}
	result   Result
}

// Result holds the outcome of Wait after a successful /callback.
type Result struct {
	Code  string
	Error string
}

// StartLogin binds a 127.0.0.1 listener on an ephemeral port and returns a
// LoginState with the authorization URL built. The user opens the URL in their
// browser. When authentication completes, Devin redirects to the callback with
// ?code=&state=. If appBaseOverride is non-empty it replaces appBaseURL in the
// authorization URL (useful for tests pointing at httptest.Server).
func StartLogin(appBaseOverride string) (*LoginState, error) {
	v, c := GeneratePKCE()

	base := APIBaseURL
	if appBaseOverride != "" {
		base = appBaseOverride
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	s := &LoginState{
		LoginURL: fmt.Sprintf(
			"%s/auth/cli/continue?redirect_uri=%s&prompt=select_account&code_challenge=%s&code_challenge_method=S256&state=%s",
			base,
			url.QueryEscape(redirect),
			c, v,
		),
		Port:     port,
		State:    v, // use verifier itself as state (unique enough)
		Verifier: v,
		code:     make(chan string, 1),
		errCh:    make(chan error, 1),
		done:     make(chan struct{}),
		result:   Result{},
	}

	lastVerifier = v // test-only accessor

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
			code := r.URL.Query().Get("code")
			st := r.URL.Query().Get("state")
			if code == "" || st == "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, "missing code or state")
				s.errCh <- errors.New("missing code or state")
				close(s.done)
				return
			}
			if st != s.State {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, "state mismatch")
				s.errCh <- fmt.Errorf("state mismatch: got %q, want %q", st, s.State)
				close(s.done)
				return
			}
			s.result.Code = code
			s.code <- code
			close(s.done)
		})

		srv := &http.Server{Handler: mux}
		_ = srv.Serve(ln)
	}()

	return s, nil
}

// Stop shuts down the callback server without waiting for authentication.
func (l *LoginState) Stop() {
	select {
	case <-l.done:
	default:
	}
}

// Wait blocks until the callback receives an authorization code (or an error)
// or ctx is cancelled / timeout elapses. Returns the result.
func (l *LoginState) Wait(ctx context.Context, timeout time.Duration) (Result, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		l.result.Error = "context cancelled"
		return l.result, fmt.Errorf("login: context cancelled: %w", ctx.Err())
	case <-timer.C:
		l.result.Error = "login timed out"
		return l.result, errors.New("login timed out")
	case <-l.done:
		if l.result.Error != "" {
			return l.result, errors.New(l.result.Error)
		}
		return l.result, nil
	}
}

// lastVerifier stores the PKCE verifier from the most recent StartLogin call.
// Only intended for use in tests via LastVerifier().
var lastVerifier string

// LastVerifier returns the PKCE verifier from the most recent StartLogin call.
// Only intended for use in tests.
func LastVerifier() string {
	return lastVerifier
}
