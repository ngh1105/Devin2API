package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ngh1105/Devin2API/internal/auth"
	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/store"
	"github.com/ngh1105/Devin2API/web"
)

// maxBodyBytes caps request bodies on /v1 and the admin API.
const maxBodyBytes = 4 << 20

// Server holds dependencies for HTTP handlers.
type Server struct {
	listen   string
	dataDir  string
	accounts *store.AccountStore
	tokens   *store.TokenStore
	auth     *auth.Client
}

// New creates a new Server from config, stores, and auth client.
func New(cfg config.Config, accounts *store.AccountStore, tokens *store.TokenStore, authClient *auth.Client) *Server {
	return &Server{
		listen:   cfg.Listen,
		dataDir:  cfg.DataDir,
		accounts: accounts,
		tokens:   tokens,
		auth:     authClient,
	}
}

// Handler returns the root http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.rootHandler)
	return mux
}

// Start runs the HTTP server on the configured address.
func (s *Server) Start(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: streamed chat completions can run for minutes.
	}
	return srv.ListenAndServe()
}

// rootHandler dispatches web files, admin API, and healthz.
func (s *Server) rootHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/" || path == "/v1" || path == "/admin" {
		http.Redirect(w, r, "/web/accounts.html", http.StatusFound)
		return
	}

	if strings.HasPrefix(path, "/web/") {
		http.StripPrefix("/web/", http.FileServer(http.FS(web.FS))).ServeHTTP(w, r)
		return
	}

	if path == "/healthz" && r.Method == http.MethodGet {
		s.healthz(w, r)
		return
	}

	if strings.HasPrefix(path, "/v1/") {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		s.v1Middleware(s.handleV1)(w, r)
		return
	}

	if strings.HasPrefix(path, "/admin/api/") {
		if msg := adminRequestError(r); msg != "" {
			writeErr(w, http.StatusForbidden, msg)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		s.adminHandler(w, r)
		return
	}

	http.NotFound(w, r)
}

// adminRequestError guards the unauthenticated admin API against browser
// attacks from other sites. It returns a non-empty reason to reject r.
//
//   - Host must be a loopback name/IP: defeats DNS rebinding, where an
//     attacker domain resolves to 127.0.0.1 and reads responses.
//   - Origin, when sent, must match Host: defeats cross-site requests.
//   - State-changing requests must carry the X-Devin2API header, which a
//     cross-site form or no-cors fetch cannot set without a preflight.
func adminRequestError(r *http.Request) string {
	if !isLoopbackHost(r.Host) {
		return "admin API is only served on loopback hosts"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return "cross-origin request rejected"
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Devin2API") == "" {
		return "missing X-Devin2API header"
	}
	return ""
}

// isLoopbackHost reports whether a Host header names this machine.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
