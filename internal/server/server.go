package server

import (
	"mime"
	"net/http"
	"strings"
	"sync"

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

func (m *loginManager) SetPending(login *devin.LoginState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop()
	m.gen++ // bump generation so old goroutines become stale
	m.current = login
	m.statusCode = "pending"
	m.accountID = ""
	m.errorMsg = ""
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
}

// New creates a new Server from config and stores.
func New(cfg config.Config, accounts *store.AccountStore, tokens *store.TokenStore) *Server {
	return &Server{
		listen:          cfg.Listen,
		dataDir:         cfg.DataDir,
		accounts:        accounts,
		tokens:          tokens,
		loginAPIBase:    devin.APIBaseURL,
		loginServerBase: devin.ServerBaseURL,
		loginMngr:       newLoginManager(),
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
	return http.ListenAndServe(addr, s.Handler())
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
		s.v1Middleware(s.handleV1)(w, r)
		return
	}

	if strings.HasPrefix(path, "/admin/api/") {
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
