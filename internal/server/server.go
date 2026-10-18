package server

import (
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
		s.v1Middleware(s.handleV1)(w, r)
		return
	}

	if strings.HasPrefix(path, "/admin/api/") {
		s.handleAdmin(w, r)
		return
	}

	http.NotFound(w, r)
}
