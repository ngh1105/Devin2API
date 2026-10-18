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
	"sync/atomic"
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

// loginResult holds the outcome of one /callback invocation. Owned exclusively
// by the callback-goroutine; delivered to Wait via a channel to avoid races.
type loginResult struct {
	code  string
	error string // non-empty means failure
}

// LoginState tracks an in-progress CLI login flow.
type LoginState struct {
	LoginURL string // URL to open in browser (base64url params are query-safe)
	Port     int    // bound loopback port
	State    string // OAuth state param (exported for tests)
	Verifier string // PKCE code_verifier
	resultCh chan loginResult
	done     chan struct{}
	shutOnce atomic.Bool // ensures Stop is idempotent
	srv      *http.Server
	ln       net.Listener
}

// StartLogin binds a 127.0.0.1 listener on an ephemeral port and returns a
// LoginState with the authorization URL built. The user opens the URL in their
// browser. When authentication completes, Devin redirects to the callback with
// ?code=&state=. If appBaseOverride is non-empty it replaces AppBaseURL in the
// authorization URL (useful for tests pointing at httptest.Server).
func StartLogin(appBaseOverride string) (*LoginState, error) {
	v, c := GeneratePKCE()

	base := AppBaseURL
	if appBaseOverride != "" {
		base = appBaseOverride
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	state := &LoginState{
		LoginURL: fmt.Sprintf(
			"%s/auth/cli/continue?redirect_uri=%s&prompt=select_account&code_challenge=%s&code_challenge_method=S256&state=%s",
			base,
			url.QueryEscape(redirect),
			c, v,
		),
		Port:     port,
		State:    v, // use verifier itself as state (unique enough)
		Verifier: v,
		resultCh: make(chan loginResult, 1),
		done:     make(chan struct{}),
		ln:       ln, // store for idempotent shutdown
	}

	lastVerifier = v // test-only accessor

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
			code := r.URL.Query().Get("code")
			st := r.URL.Query().Get("state")
			var res loginResult
			switch {
			case code == "" || st == "":
				res.error = "missing code or state"
			case st != state.State:
				res.error = fmt.Sprintf("state mismatch: got %q, want %q", st, state.State)
			default:
				res.code = code
			}
			if res.error != "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, res.error)
			}
			state.resultCh <- res
		})

		state.srv = &http.Server{Handler: mux}
		_ = state.srv.Serve(ln)
	}()

	return state, nil
}

// Stop shuts down the callback HTTP server and releases the listener.
// Signals Wait to finish by closing the done channel. Idempotent.
func (l *LoginState) Stop() {
	if l.shutOnce.Swap(true) {
		return
	}
	l.ln.Close() // closes listener → Serve returns immediately
	select {
	case <-l.done:
	default:
		close(l.done)
	}
}

// Wait blocks until the callback receives an authorization code (or an error)
// or ctx is cancelled / timeout elapses. Returns the result.
func (l *LoginState) Wait(ctx context.Context, timeout time.Duration) (Result, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return Result{}, fmt.Errorf("login: context cancelled: %w", ctx.Err())
	case <-timer.C:
		return Result{}, errors.New("login timed out")
	case res := <-l.resultCh:
		if res.error != "" {
			return Result{}, errors.New(res.error)
		}
		close(l.done) // close done AFTER receiving valid result
		return Result{Code: res.code}, nil
	case <-l.done:
		// Stop() closed done. Non-block drain resultCh.
		select {
		case res := <-l.resultCh:
			if res.error != "" {
				return Result{}, errors.New(res.error)
			}
			return Result{Code: res.code}, nil
		default:
			return Result{}, errors.New("login stopped")
		}
	}
}

// Result holds the outcome of a successful Wait call.
type Result struct {
	Code string
}

// lastVerifier stores the PKCE verifier from the most recent StartLogin call.
// Only intended for use in tests via LastVerifier().
var lastVerifier string

// LastVerifier returns the PKCE verifier from the most recent StartLogin call.
// Only intended for use in tests.
func LastVerifier() string {
	return lastVerifier
}
