package server

import (
	"net/http"

	"github.com/ngh1105/Devin2API/internal/auth"
	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/store"
)

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

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/v1/", s.v1Middleware(s.handleV1))
	mux.HandleFunc("/admin/api/", s.adminHandler)

	return mux
}
