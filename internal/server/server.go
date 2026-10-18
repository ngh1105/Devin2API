package server

import (
	"net/http"
	"strings"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/store"
	"github.com/ngh1105/Devin2API/web"
)

// Server holds dependencies for HTTP handlers.
type Server struct {
	listen   string
	dataDir  string
	accounts *store.AccountStore
	tokens   *store.TokenStore
}

// New creates a new Server from config and stores.
func New(cfg config.Config, accounts *store.AccountStore, tokens *store.TokenStore) *Server {
	return &Server{
		listen:   cfg.Listen,
		dataDir:  cfg.DataDir,
		accounts: accounts,
		tokens:   tokens,
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
		s.adminHandler(w, r)
		return
	}

	http.NotFound(w, r)
}
