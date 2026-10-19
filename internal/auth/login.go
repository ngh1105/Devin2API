package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
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

const (
	// maxHops bounds the redirect/page chain of the hosted login flow.
	maxHops = 12
	// maxPageBytes caps how much of an HTML page or error body is read.
	maxPageBytes = 1 << 20
	userAgent    = "Devin2API"
)

var (
	// stateInputRe matches the hidden state <input>, whatever the attribute order.
	stateInputRe = regexp.MustCompile(`(?is)<input\b[^>]*\bname=["']state["'][^>]*>`)
	valueAttrRe  = regexp.MustCompile(`(?is)\bvalue=["']([^"']*)["']`)
	// errorElemRe matches Universal Login's inline error, e.g.
	// <span id="error-element-password" ...>Wrong email or password</span>.
	errorElemRe = regexp.MustCompile(`(?is)id=["']error-element-[^"']*["'][^>]*>\s*([^<]+?)\s*<`)
)

// flowClient returns a client sharing c's transport that never follows
// redirects (the flow inspects each Location itself) and keeps session
// cookies across the hosted-login pages.
func (c *Client) flowClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Transport: c.httpClt.Transport,
		Timeout:   c.httpClt.Timeout,
		Jar:       jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// send performs a GET (form == nil) or form POST to u.
func send(ctx context.Context, cl *http.Client, u *url.URL, form url.Values) (*http.Response, error) {
	method, body := http.MethodGet, io.Reader(nil)
	if form != nil {
		method, body = http.MethodPost, strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("User-Agent", userAgent)
	return cl.Do(req)
}

// readBody reads up to maxPageBytes of resp.Body and closes it.
func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
}

// formState returns the HTML-unescaped value of the hidden state input.
func formState(body []byte) (string, bool) {
	tag := stateInputRe.Find(body)
	if tag == nil {
		return "", false
	}
	m := valueAttrRe.FindSubmatch(tag)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(string(m[1])), true
}

// pageError extracts the inline error message of a re-rendered login page.
func pageError(body []byte) string {
	if m := errorElemRe.FindSubmatch(body); m != nil {
		return html.UnescapeString(string(m[1]))
	}
	return ""
}

// oauthError formats an error/error_description pair from a query or JSON body.
func oauthError(code, desc string) string {
	if desc != "" {
		return code + ": " + desc
	}
	return code
}

// sameEndpoint reports whether a and b share scheme, host and path.
func sameEndpoint(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host) && a.Path == b.Path
}

func isRedirect(status int) bool {
	return status == http.StatusFound || status == http.StatusSeeOther ||
		status == http.StatusMovedPermanently || status == http.StatusTemporaryRedirect
}

func (c *Client) pkcePair() pkcePair { return newPKCEPair(rand.Reader) }

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Login performs the Auth0 Universal Login flow headlessly. It starts at
// /authorize and follows every Location the server returns, filling the
// identifier and password forms it lands on, until Auth0 redirects to
// RedirectURI with an authorization code — possibly via /authorize/resume.
// The code is then exchanged for tokens with the PKCE verifier.
func (c *Client) Login(ctx context.Context, email, password string) (Tokens, error) {
	var tokens Tokens
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return tokens, &AuthError{"authorize", "bad base URL: " + err.Error()}
	}
	callback, _ := url.Parse(RedirectURI)
	cl := c.flowClient()
	pkce := c.pkcePair()
	state := randomState()

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", ClientID)
	params.Set("redirect_uri", RedirectURI)
	params.Set("scope", Scope)
	params.Set("state", state)
	params.Set("audience", Audience)
	params.Set("code_challenge", pkce.Challenge)
	params.Set("code_challenge_method", "S256")

	cur := base.ResolveReference(&url.URL{Path: "/authorize", RawQuery: params.Encode()})
	stage := "authorize"
	var form url.Values // nil = GET cur, non-nil = POST form to cur
	var code string

	for hop := 0; code == ""; hop++ {
		if hop >= maxHops {
			return tokens, &AuthError{stage, "too many redirects"}
		}
		resp, err := send(ctx, cl, cur, form)
		if err != nil {
			return tokens, &AuthError{stage, err.Error()}
		}
		body, err := readBody(resp)
		if err != nil {
			return tokens, &AuthError{stage, err.Error()}
		}

		if isRedirect(resp.StatusCode) {
			loc := resp.Header.Get("Location")
			if loc == "" {
				return tokens, &AuthError{stage, "redirect without Location"}
			}
			next, err := cur.Parse(loc)
			if err != nil {
				return tokens, &AuthError{stage, "bad redirect: " + err.Error()}
			}
			q := next.Query()
			if e := q.Get("error"); e != "" {
				return tokens, &AuthError{stage, oauthError(e, q.Get("error_description"))}
			}
			if sameEndpoint(next, callback) {
				if q.Get("state") != state {
					return tokens, &AuthError{stage, "state mismatch in callback"}
				}
				if code = q.Get("code"); code == "" {
					return tokens, &AuthError{stage, "no code in redirect location"}
				}
				break
			}
			if !strings.EqualFold(next.Host, base.Host) {
				return tokens, &AuthError{stage, "unexpected redirect to " + next.Host}
			}
			cur, form = next, nil
			continue
		}

		if form != nil {
			// Auth0 re-renders the page (usually 400) when a form is rejected.
			msg := pageError(body)
			if msg == "" {
				msg = "rejected with status " + strconv.Itoa(resp.StatusCode)
			}
			return tokens, &AuthError{stage, msg}
		}
		if resp.StatusCode != http.StatusOK {
			return tokens, &AuthError{stage, "unexpected status " + strconv.Itoa(resp.StatusCode)}
		}

		pageState, ok := formState(body)
		if !ok {
			pageState = cur.Query().Get("state")
		}
		if pageState == "" {
			return tokens, &AuthError{stage, "state not found on " + cur.Path}
		}
		switch cur.Path {
		case "/u/login/identifier":
			stage = "identifier"
			form = url.Values{
				"state":                       {pageState},
				"username":                    {email},
				"js-available":                {"true"},
				"webauthn-available":          {"false"},
				"is-brave":                    {"false"},
				"webauthn-platform-available": {"false"},
				"action":                      {"default"},
			}
		case "/u/login/password", "/u/login":
			stage = "password"
			form = url.Values{
				"state":    {pageState},
				"username": {email},
				"password": {password},
				"action":   {"default"},
			}
		default:
			return tokens, &AuthError{stage, "unsupported login page " + cur.Path + " (MFA, captcha and social logins are not supported)"}
		}
	}

	tokenURL := base.ResolveReference(&url.URL{Path: "/oauth/token"})
	return c.tokenRequest(ctx, "exchange", tokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code_verifier": {pkce.Verifier},
		"code":          {code},
		"redirect_uri":  {RedirectURI},
	})
}

// tokenRequest POSTs form to the token endpoint and decodes the response.
// Non-200 responses surface Auth0's error/error_description.
func (c *Client) tokenRequest(ctx context.Context, stage string, u *url.URL, form url.Values) (Tokens, error) {
	var tokens Tokens
	resp, err := send(ctx, c.httpClt, u, form)
	if err != nil {
		return tokens, &AuthError{stage, err.Error()}
	}
	body, err := readBody(resp)
	if err != nil {
		return tokens, &AuthError{stage, err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		msg := "unexpected status " + strconv.Itoa(resp.StatusCode)
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg += " (" + oauthError(e.Error, e.Description) + ")"
		}
		return tokens, &AuthError{stage, msg}
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return tokens, &AuthError{stage, err.Error()}
	}
	return tokens, nil
}

// Refresh requests a new access token using the provided refresh_token.
// If the server's response omits refresh_token, the old one is kept.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	u, err := url.Parse(c.baseURL + "/oauth/token")
	if err != nil {
		return Tokens{}, &AuthError{"refresh", err.Error()}
	}
	tokens, err := c.tokenRequest(ctx, "refresh", u, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {ClientID},
	})
	if err != nil {
		return tokens, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

// UserInfo calls the Auth0 userinfo endpoint with the given access token.
func (c *Client) UserInfo(ctx context.Context, accessToken string) (Profile, error) {
	var p Profile
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/userinfo", nil)
	if err != nil {
		return p, &AuthError{"userinfo", err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.httpClt.Do(req)
	if err != nil {
		return p, &AuthError{"userinfo", err.Error()}
	}
	body, err := readBody(resp)
	if err != nil {
		return p, &AuthError{"userinfo", err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return p, &AuthError{"userinfo", "status " + resp.Status}
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, &AuthError{"userinfo", err.Error()}
	}
	return p, nil
}
