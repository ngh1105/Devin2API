package server

import (
	"context"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
	"github.com/ngh1105/Devin2API/web"
)

// ---------- login manager ---------------------------------------------------

type loginManager struct {
	mu         sync.Mutex
	current    *devin.LoginState
	gen        uint64 // monotonically incremented on each SetPending
	statusCode string // idle | pending | complete | error
	accountID  string
	errorMsg   string
}

func newLoginManager() *loginManager {
	return &loginManager{statusCode: "idle"}
}

func (m *loginManager) stop() {
	if m.current != nil {
		m.current.Stop()
		m.current = nil
	}
}

// genSnapshot returns the current generation for use by async goroutines.
func (m *loginManager) genSnapshot() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

// ResolveComplete only writes if the provided gen matches m.gen.
func (m *loginManager) ResolveComplete(accountID string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen {
		return // stale resolve — ignore
	}
	m.statusCode = "complete"
	m.accountID = accountID
	m.errorMsg = ""
}

// ResolveError only writes if the provided gen matches m.gen.
func (m *loginManager) ResolveError(msg string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen {
		return // stale resolve — ignore
	}
	m.statusCode = "error"
	m.accountID = ""
	m.errorMsg = msg
}

func (m *loginManager) Status() (string, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusCode, m.accountID, m.errorMsg
}

// SetPending makes login the current one and returns its generation, taken
// under the same lock so concurrent starts cannot read each other's.
func (m *loginManager) SetPending(login *devin.LoginState) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop()
	m.gen++ // bump generation so old goroutines become stale
	m.current = login
	m.statusCode = "pending"
	m.accountID = ""
	m.errorMsg = ""
	return m.gen
}

// isCurrent reports whether gen is still the active login (not cancelled
// or replaced).
func (m *loginManager) isCurrent(gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen == gen
}

func (m *loginManager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop()
	m.statusCode = "idle"
	m.accountID = ""
	m.errorMsg = ""
}

// --- Server ------------------------------------------------------------

// Server holds dependencies for HTTP handlers.
type Server struct {
	listen          string
	dataDir         string
	accounts        *store.AccountStore
	tokens          *store.TokenStore
	loginAPIBase    string
	loginServerBase string
	loginMngr       *loginManager

	chatRR          atomic.Uint64 // round-robin cursor over chat accounts
	models          modelCache    // upstream model catalog
	reqLog          requestLog    // recent chat requests for the panel
	storedResponses responseStore // /v1/responses history for previous_response_id
	auth            adminAuth     // optional admin password and sessions
	cooldown        cooldowns     // accounts parked per model after a 429
	stats           chatStats     // chat request totals, persisted in the data dir
	settings        settingsStore // panel-editable settings, persisted in the data dir
}

// New creates a new Server from config and stores.
func New(cfg config.Config, accounts *store.AccountStore, tokens *store.TokenStore) *Server {
	s := &Server{
		listen:          cfg.Listen,
		dataDir:         cfg.DataDir,
		accounts:        accounts,
		tokens:          tokens,
		loginAPIBase:    devin.APIBaseURL,
		loginServerBase: devin.ServerBaseURL,
		loginMngr:       newLoginManager(),
	}
	s.stats.load(cfg.DataDir)
	s.reqLog.load(cfg.DataDir)
	s.settings.load(cfg.DataDir)
	s.auth.password = cfg.AdminPassword
	s.storedResponses.configure(cfg.ResponsesKeep, cfg.ResponsesTTL, cfg.ResponsesMB)
	return s
}

// Handler returns the root http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.rootHandler)
	return mux
}

// maxBodyBytes caps admin API request bodies; maxV1BodyBytes caps /v1,
// which must fit base64 images and long agent histories.
const (
	maxBodyBytes   = 4 << 20
	maxV1BodyBytes = 32 << 20
)

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
	// Warm the model catalog so the first chat can map family names.
	go s.fetchModels(context.Background())
	// Keep quota data fresh for routing.
	go s.usageLoop(context.Background())

	// On Ctrl+C or SIGTERM, give open requests a moment to finish, then
	// write the counters that are saved with a delay.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	srv.Shutdown(sctx)
	s.flush()
	return nil
}

// shutdownGrace bounds how long shutdown waits for open requests.
const shutdownGrace = 5 * time.Second

// flush writes state that is otherwise saved after a delay.
func (s *Server) flush() {
	if err := s.tokens.Flush(); err != nil {
		slog.Warn("save api keys", "error", err)
	}
	if s.stats.path != "" {
		s.stats.save()
	}
}

// adminRequestError guards the admin API against browser attacks from
// other sites. It returns a non-empty reason to reject r.
//
//   - Without an admin password, Host must be a loopback name/IP: defeats
//     DNS rebinding, where an attacker domain resolves to 127.0.0.1 and
//     reads responses. With a password, any Host is served: a rebinding
//     page never holds the SameSite session cookie, so it gets 401.
//   - Origin, when sent, must match Host: defeats cross-site requests.
//   - State-changing requests must carry the X-Devin2API header, which a
//     cross-site form or no-cors fetch cannot set without a preflight.
func adminRequestError(r *http.Request, passwordSet bool) string {
	if !passwordSet && !isLocalRequest(r) {
		return "admin API is only served to local clients (set DEVIN2API_ADMIN_PASSWORD to allow other hosts)"
	}
	if foreignOrigin(r) {
		return "cross-origin request rejected"
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Devin2API") == "" {
		return "missing X-Devin2API header"
	}
	return ""
}

// foreignOrigin reports whether r carries an Origin header that does not
// match its Host. Non-browser clients send no Origin and pass.
func foreignOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err != nil || !strings.EqualFold(u.Host, r.Host)
}

// isLocalRequest reports whether r comes from this machine: the TCP peer is
// a loopback address AND the Host header names this machine. The peer check
// stops direct requests from the network that forge "Host: localhost"; the
// Host check stops DNS rebinding from a local browser.
func isLocalRequest(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && isLoopbackHost(r.Host)
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

// rootHandler dispatches web files, admin API, and healthz.
func (s *Server) rootHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// SPA entry — redirect bare paths to /web/ so fileserver can find index.html.
	if path == "/" || path == "/v1" || path == "/admin" {
		http.Redirect(w, r, "/web/", http.StatusFound)
		return
	}

	if strings.HasPrefix(path, "/web/") {
		if path == "/web/" {
			data, err := web.FS.ReadFile("index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		// Serve embedded static assets (app.js, styles.css).
		webFileServer(w, r)
		return
	}

	if path == "/healthz" && r.Method == http.MethodGet {
		s.healthz(w, r)
		return
	}

	if strings.HasPrefix(path, "/v1/") {
		r.Body = http.MaxBytesReader(w, r.Body, maxV1BodyBytes)
		s.v1Middleware(s.handleV1)(w, r)
		return
	}

	if strings.HasPrefix(path, "/admin/api/") {
		if msg := adminRequestError(r, s.auth.enabled()); msg != "" {
			writeErr(w, http.StatusForbidden, msg)
			return
		}
		if !adminPublicPaths[path] && !s.auth.authenticated(r) {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		s.handleAdmin(w, r)
		return
	}

	http.NotFound(w, r)
}

// webFileServer serves embedded web assets from index.html through styles.css.
func webFileServer(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/web/")
	// Guard against path traversal before reading from the embedded FS.
	if strings.Contains(path, "..") || path != strings.TrimPrefix(path, "/") {
		http.NotFound(w, r)
		return
	}
	data, err := web.FS.ReadFile(path)
	if err != nil || len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	var ct string
	switch {
	case strings.HasSuffix(path, ".html"):
		ct = "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		ct = "text/css; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		ct = "application/javascript; charset=utf-8"
	default:
		ct = mime.TypeByExtension("." + pathExt(path))
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func pathExt(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '.' {
			return p[i+1:]
		}
	}
	return ""
}
