package server

import (
	"encoding/json"
	"io"
	"net/http"

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
			next.ServeHTTP(w, r)
			return
		}

		authed := false
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			const prefix = "Bearer "
			if len(authHeader) >= len(prefix) {
				token := authHeader[len(prefix):]
				if ok, _ := s.tokens.Verify(token); ok {
					authed = true
				}
			}
		}
		if !authed {
			if apiKey := r.Header.Get("X-Api-Key"); apiKey != "" {
				if ok, _ := s.tokens.Verify(apiKey); ok {
					authed = true
				}
			}
		}
		if !authed {
			openai.WriteError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// getModels returns a static model list matching the OpenAI API shape.
func (s *Server) getModels(w http.ResponseWriter, r *http.Request) {
	body := `{"object":"list","data":[{"id":"devin","object":"model","owned_by":"devin"}]}`
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(body))
}

// chatCompletions handles /v1/chat/completions — stub returning 501.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req openai.ChatRequest
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		openai.WriteError(w, http.StatusBadRequest, "unable to read body")
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		openai.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(req.Messages) == 0 {
		openai.WriteError(w, http.StatusBadRequest, "messages must not be empty")
		return
	}

	msg := "upstream not implemented yet"

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		errResp := openai.Error{Error: openai.ErrorBody{Message: msg, Type: "devin2api_error"}}
		payload, _ := json.Marshal(errResp)
		io.WriteString(w, "data: ")
		w.Write(payload)
		io.WriteString(w, "\n\ndata: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	openai.WriteError(w, http.StatusNotImplemented, msg)
}
