package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ngh1105/Devin2API/internal/anthropic"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/openai"
)

// healthz returns a simple readiness probe.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleV1 dispatches /v1/* sub-paths.
func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/v1/models" && r.Method == http.MethodGet:
		s.getModels(w, r)
	case path == "/v1/chat/completions" && r.Method == http.MethodPost:
		s.chatCompletions(w, r)
	case path == "/v1/messages" && r.Method == http.MethodPost:
		s.messages(w, r)
	case path == "/v1/messages/count_tokens" && r.Method == http.MethodPost:
		s.countTokens(w, r)
	case path == "/v1/responses" && r.Method == http.MethodPost:
		s.responses(w, r)
	default:
		http.NotFound(w, r)
	}
}

// v1Middleware enforces token verification for every /v1/* request.
func (s *Server) v1Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokens, err := s.tokens.List()
		if err != nil || len(tokens) == 0 {
			// Keyless mode spends real Devin quota, so only local non-browser
			// style callers are let in: loopback Host (blocks DNS rebinding)
			// and no foreign Origin (blocks cross-site requests).
			if !isLoopbackHost(r.Host) || foreignOrigin(r) {
				writeV1AuthError(w, r, "create an API key in the panel to use /v1 from this origin")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		var id string
		authed := false
		if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			id, authed = s.tokens.Match(strings.TrimSpace(token))
		}
		if !authed {
			id, authed = s.tokens.Match(r.Header.Get("X-Api-Key"))
		}
		if !authed {
			writeV1AuthError(w, r, "invalid or missing token")
			return
		}
		s.tokens.Touch(id)
		next.ServeHTTP(w, r)
	}
}

// getModels returns the upstream model catalog in the OpenAI API shape,
// led by the "devin" alias for the default model. Without a reachable
// account it lists only the alias.
func (s *Server) getModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		OwnedBy       string `json:"owned_by"`
		DisplayName   string `json:"display_name,omitempty"`
		ContextLength int    `json:"context_length,omitempty"`
		Multimodal    bool   `json:"multimodal,omitempty"`
	}
	data := []model{{ID: "devin", Object: "model", OwnedBy: "devin", DisplayName: "Default (" + devin.DefaultModel + ")"}}
	for _, m := range s.fetchModels(r.Context()) {
		data = append(data, model{
			ID: m.UID, Object: "model", OwnedBy: "devin",
			DisplayName: m.Label, ContextLength: m.ContextLength, Multimodal: m.Multimodal,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// writeJSON writes v as a JSON response with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// randID returns 24 random hex characters for response ids.
func randID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// writeV1AuthError answers a rejected /v1 request with a 401 in the
// caller's API format: Anthropic clients (Claude Code) expect
// {"type":"error","error":{"type":"authentication_error",...}}.
func writeV1AuthError(w http.ResponseWriter, r *http.Request, msg string) {
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		anthropic.WriteError(w, http.StatusUnauthorized, msg)
		return
	}
	openai.WriteError(w, http.StatusUnauthorized, msg)
}
