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
	"sync"
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

	resp, err := apiHTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("exchange request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONBytes))
		return "", fmt.Errorf("exchange failed: status %d", resp.StatusCode)
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("exchange returned no token")
	}
	return out.Token, nil
}

// apiHTTPClient is used for the short request/response calls (token
// exchange, profile, user status, model list). A timeout keeps a stalled
// upstream from hanging a login or the background usage refresh forever.
var apiHTTPClient = &http.Client{Timeout: 30 * time.Second}

// maxJSONBytes caps the small JSON/proto bodies read from those calls.
const maxJSONBytes = 1 << 20

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
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("devin: crypto/rand failed: " + err.Error())
	}
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
	doneOnce sync.Once   // ensures done is closed at most once (prevents close-panic race)
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

	// state is independent of the PKCE verifier: the verifier must stay
	// secret until the token exchange, while state travels in the URL.
	oauthState := randHex(16)
	state := &LoginState{
		LoginURL: fmt.Sprintf(
			"%s/auth/cli/continue?redirect_uri=%s&prompt=select_account&code_challenge=%s&code_challenge_method=S256&state=%s",
			base,
			url.QueryEscape(redirect),
			c, oauthState,
		),
		Port:     port,
		State:    oauthState,
		Verifier: v,
		resultCh: make(chan loginResult, 1),
		done:     make(chan struct{}),
		ln:       ln, // store for idempotent shutdown
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Only a request carrying this login's state may complete or fail
		// it; anything else (a stray or hostile local request) is refused
		// without affecting the pending login.
		if q.Get("state") != state.State {
			http.Error(w, "invalid or missing state", http.StatusBadRequest)
			return
		}
		var res loginResult
		switch {
		case q.Get("error") != "":
			res.error = "login failed: " + q.Get("error")
		case q.Get("code") == "":
			res.error = "missing code"
		default:
			res.code = q.Get("code")
		}
		select {
		case state.resultCh <- res:
		default: // a result is already pending; ignore repeats
		}
		if res.error != "" {
			http.Error(w, res.error, http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "Login complete. You can close this tab.")
	})
	state.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = state.srv.Serve(ln) }()

	return state, nil
}

// Stop shuts down the callback HTTP server and releases the listener.
// Signals Wait to finish by closing the done channel. Idempotent.
func (l *LoginState) Stop() {
	if l.shutOnce.Swap(true) {
		return
	}
	l.srv.Close() // closes the listener and any open callback connections
	select {
	case <-l.done:
	default:
		l.doneOnce.Do(func() { close(l.done) })
	}
}

// Wait blocks until the callback receives an authorization code (or an error)
// or ctx is cancelled / timeout elapses. A timeout <= 0 means no timeout
// (ctx alone bounds the wait). Returns the result.
func (l *LoginState) Wait(ctx context.Context, timeout time.Duration) (Result, error) {
	var timeoutC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	select {
	case <-ctx.Done():
		return Result{}, fmt.Errorf("login: context cancelled: %w", ctx.Err())
	case <-timeoutC:
		return Result{}, errors.New("login timed out")
	case res := <-l.resultCh:
		if res.error != "" {
			return Result{}, errors.New(res.error)
		}
		l.doneOnce.Do(func() { close(l.done) }) // close done AFTER receiving valid result (race-guarded by sync.Once)
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

