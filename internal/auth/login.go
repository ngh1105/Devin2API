package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Tokens holds an Auth0 token response.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// Profile is the userinfo endpoint response.
type Profile struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// AuthError represents a failure at a specific stage of the auth flow.
type AuthError struct {
	Stage   string // authorize, identifier, password, exchange, refresh, userinfo
	Message string
}

func (e *AuthError) Error() string { return "auth: " + e.Stage + ": " + e.Message }

// noRedirectClient returns an http.Client that never follows redirects —
// callers inspect Location headers themselves.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// do performs an HTTP request against c's baseURL using cl.
func (c *Client) do(cl *http.Client, ctx context.Context, method, uri string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+uri, nil)
	if err != nil {
		return nil, err
	}
	return cl.Do(req)
}

// doFormPost performs a POST with application/x-www-form-urlencoded body.
func (c *Client) doFormPost(cl *http.Client, ctx context.Context, uri string, data url.Values) (*http.Response, error) {
	bodyStr := data.Encode()
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+uri, strings.NewReader(bodyStr))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Content-Length", strconv.Itoa(len(bodyStr)))
	req.Header.Set("User-Agent", "Devin2API")
	return cl.Do(req)
}

// extractHidden returns the value of the hidden <input name="n"> from body bytes.
func extractHidden(body []byte, n string) (string, error) {
	s := string(body)
	idx := strings.Index(s, `<input name="`+n+`"`)
	if idx < 0 {
		return "", &AuthError{"extract", "hidden field " + n + " not found"}
	}
	vStart := strings.Index(s[idx:], `value="`)
	if vStart < 0 {
		return "", &AuthError{"extract", "value attribute not found"}
	}
	vStart += idx + len(`value="`)
	vEnd := strings.Index(s[vStart:], `"`)
	if vEnd < 0 {
		return "", &AuthError{"extract", "value unclosed"}
	}
	return s[vStart : vStart+vEnd], nil
}

// queryVal extracts a named value from a URL string.
func queryVal(loc, key string) (string, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	v := u.Query().Get(key)
	if v == "" {
		return "", &AuthError{"extract", "field " + key + " not in query"}
	}
	return v, nil
}

func (c *Client) pkcePair() pkcePair { return newPKCEPair(rand.Reader) }

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Login performs the full Auth0 hosted-login flow:
// authorize -> identifier POST -> password POST -> code capture -> token exchange.
func (c *Client) Login(ctx context.Context, email, password string) (Tokens, error) {
	var tokens Tokens
	cl := noRedirectClient()
	pkce := c.pkcePair()

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", ClientID)
	params.Set("redirect_uri", RedirectURI)
	params.Set("scope", Scope)
	params.Set("state", randomState())
	params.Set("audience", Audience)
	params.Set("code_challenge", pkce.Challenge)
	params.Set("code_challenge_method", "S256")

	// GET /authorize — gets cookies, redirects to /u/login/identifier.
	resp, err := c.do(cl, ctx, "GET", "/authorize?"+params.Encode())
	if err != nil {
		return tokens, &AuthError{"authorize", err.Error()}
	}
	expectOK(resp, http.StatusFound)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Check for error in redirect Location (e.g., access_denied).
	redirLoc := resp.Header.Get("Location")
	if strings.Contains(redirLoc, "error=") {
		eqIdx := strings.Index(redirLoc, "error=")
		msg := redirLoc[eqIdx+len("error="):]
		if amp := strings.Index(msg, "&"); amp > 0 {
			msg = msg[:amp]
		}
		return tokens, &AuthError{"password", msg}
	}

	stateVal := params.Get("state")

	// GET /u/login/identifier — grab hidden state from HTML.
	resp, err = c.do(cl, ctx, "GET", "/u/login/identifier?state="+url.PathEscape(stateVal))
	if err != nil {
		return tokens, &AuthError{"identifier", err.Error()}
	}
	expectOK(resp, http.StatusOK)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return tokens, &AuthError{"identifier", err.Error()}
	}
	idState, err := extractHidden(body, "state")
	if err != nil {
		return tokens, &AuthError{"identifier", err.Error()}
	}

	// POST /u/login/identifier — submit email, redirect to /u/login/password.
	data := url.Values{
		"state":                       {idState},
		"username":                    {email},
		"js-available":                {"true"},
		"webauthn-available":          {"false"},
		"is-brave":                    {"false"},
		"webauthn-platform-available": {"false"},
		"action":                      {"default"},
	}
	resp, err = c.doFormPost(cl, ctx, "/u/login/identifier", data)
	if err != nil {
		return tokens, &AuthError{"identifier", err.Error()}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !expectAny(resp, http.StatusFound) {
		return tokens, &AuthError{"identifier", "unexpected status " + strconv.Itoa(resp.StatusCode)}
	}

	pwLoc := resp.Header.Get("Location")
	pwState, err := queryVal(pwLoc, "state")
	if err != nil {
		return tokens, &AuthError{"password", "missing state"}
	}

	// GET /u/login/password — grab hidden state.
	resp, err = c.do(cl, ctx, "GET", "/u/login/password?state="+url.PathEscape(pwState))
	if err != nil {
		return tokens, &AuthError{"password", err.Error()}
	}
	expectOK(resp, http.StatusOK)
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return tokens, &AuthError{"password", err.Error()}
	}
	pwStateHTML, err := extractHidden(body, "state")
	if err != nil {
		return tokens, &AuthError{"password", err.Error()}
	}
	if pwStateHTML != pwState {
		return tokens, &AuthError{"password", "state mismatch between query and form"}
	}

	// POST /u/login/password — submit credentials. Expect 302 to redirect URI with code.
	pwData := url.Values{
		"state":    {pwStateHTML},
		"username": {email},
		"password": {password},
		"action":   {"default"},
	}
	resp, err = c.doFormPost(cl, ctx, "/u/login/password", pwData)
	if err != nil {
		return tokens, &AuthError{"password", err.Error()}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !expectAny(resp, http.StatusFound) {
		return tokens, &AuthError{"password", "login failed — status " + strconv.Itoa(resp.StatusCode)}
	}

	rloc := resp.Header.Get("Location")
	if strings.Contains(rloc, "error=") {
		eqIdx := strings.Index(rloc, "error=")
		msg := rloc[eqIdx+len("error="):]
		if amp := strings.Index(msg, "&"); amp > 0 {
			msg = msg[:amp]
		}
		return tokens, &AuthError{"password", msg}
	}

	// Extract authorization code from redirect Location.
	codeQ := strings.TrimPrefix(rloc, RedirectURI)
	codeQ = strings.TrimPrefix(codeQ, "?")
	parsed, err := url.ParseQuery(codeQ)
	if err != nil || len(parsed["code"]) == 0 {
		return tokens, &AuthError{"password", "no code in redirect location"}
	}
	authCode := parsed["code"][0]

	// POST /oauth/token — exchange code for tokens.
	tokenData := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code_verifier": {pkce.Verifier},
		"code":          {authCode},
		"redirect_uri":  {RedirectURI},
	}
	resp, err = c.doFormPost(cl, ctx, "/oauth/token", tokenData)
	if err != nil {
		return tokens, &AuthError{"exchange", err.Error()}
	}
	expectOK(resp, http.StatusOK)
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return tokens, &AuthError{"exchange", err.Error()}
	}
	return tokens, nil
}

// Refresh requests a new access token using the provided refresh_token.
// If the server's response omits refresh_token, the old one is kept.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	var tokens Tokens
	cl := noRedirectClient()
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {ClientID},
	}
	resp, err := c.doFormPost(cl, ctx, "/oauth/token", data)
	if err != nil {
		return tokens, &AuthError{"refresh", err.Error()}
	}
	defer resp.Body.Close()
	expectOK(resp, http.StatusOK)
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return tokens, &AuthError{"refresh", err.Error()}
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

// UserInfo calls the Auth0 userinfo endpoint with the given access token.
func (c *Client) UserInfo(ctx context.Context, accessToken string) (Profile, error) {
	var p Profile
	cl := noRedirectClient()
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/userinfo", nil)
	if err != nil {
		return p, &AuthError{"userinfo", err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := cl.Do(req)
	if err != nil {
		return p, &AuthError{"userinfo", err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p, &AuthError{"userinfo", "status " + resp.Status}
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return p, &AuthError{"userinfo", err.Error()}
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// expectOK checks resp.StatusCode == want. Caller must close resp.Body.
func expectOK(resp *http.Response, want int) {
	if resp.StatusCode != want {
		panic(&AuthError{"exchange", "unexpected status " + strconv.Itoa(resp.StatusCode)})
	}
}

// expectAny checks resp.StatusCode matches any of the given codes.
// Returns false if none match (caller must handle close).
func expectAny(resp *http.Response, codes ...int) bool {
	for _, c := range codes {
		if resp.StatusCode == c {
			return true
		}
	}
	return false
}
