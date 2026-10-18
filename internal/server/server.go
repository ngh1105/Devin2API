package server

import (
	"net/http"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/store"
)

// Server holds dependencies for HTTP handlers.
type Server struct {
	listen   string
	accounts *store.AccountStore
	tokens   *store.TokenStore
}

// New creates a new Server from config and stores.
func New(cfg config.Config, accounts *store.AccountStore, tokens *store.TokenStore) *Server {
	return &Server{
		listen:   cfg.Listen,
		accounts: accounts,
		tokens:   tokens,
	}
}

// Handler returns the root http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/v1/", s.v1Middleware(s.handleV1))

	return mux
}
