// Package auth provides OAuth 2.0 / Auth0 helpers for the Devin2API proxy.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"
)

const (
	// AuthDomain is the Auth0 tenant domain.
	AuthDomain = "auth.devin.ai"

	// Audience is the Auth0 API identifier.
	Audience = "https://backend.webapp.devin.ai"

	// RedirectURI is the callback URI after the Auth0 post-auth page.
	RedirectURI = "https://app.devin.ai/post-auth"

	// Scope is requested at the authorization request.
	Scope = "openid profile email offline_access"

	// ClientID is the OAuth 2.0 client identifier registered with Auth0.
	ClientID = "DjKr4B4qtVDBVMBzE4FvveclTSkJYfhk"
)

// BaseURL for the Auth0 tenant — built from AuthDomain when not overridden.
const defaultBaseURL = "https://" + AuthDomain

// Client is an Auth0 client ready for the login flow.
type Client struct {
	baseURL string
	httpClt *http.Client
}

// Option sets a field on c during NewClient construction.
type Option func(*Client)

// WithHTTPClient replaces the default http.Client used by c.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClt = h }
}

// WithBaseURL overrides the base URL; used by tests only.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// NewClient returns an authenticated Client configured with opts.
func NewClient(opts ...Option) *Client {
	c := &Client{baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	if c.httpClt == nil {
		c.httpClt = &http.Client{Timeout: defaultTimeout}
	}
	return c
}

// defaultTimeout bounds each Auth0 request when no client is supplied.
const defaultTimeout = 30 * time.Second

// randomState generates a 16-byte url-safe base64 state string.
func randomState() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
