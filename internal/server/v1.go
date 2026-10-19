package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

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
				openai.WriteError(w, http.StatusUnauthorized, "create an API key in the panel to use /v1 from this origin")
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
			openai.WriteError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		s.tokens.Touch(id)
		next.ServeHTTP(w, r)
	}
}

// knownModels is the advertised model list. "devin" aliases the default
// upstream model; any other upstream model UID is also accepted as-is.
var knownModels = []string{
	"devin", "swe-2-high", "swe-1-7", "swe-1-6",
	"claude-sonnet-4-6", "claude-sonnet-4-6-thinking", "claude-opus-4-6", "claude-opus-4-6-thinking",
	"gpt-5-4", "gpt-5-5", "gemini-3-1-pro-high", "kimi-k2-6", "glm-5-2",
}

// getModels returns the model list in the OpenAI API shape.
func (s *Server) getModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, len(knownModels))
	for i, id := range knownModels {
		data[i] = model{ID: id, Object: "model", OwnedBy: "devin"}
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
