package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/ngh1105/Devin2API/internal/store"
)

// accountExpired wraps an Account with an expired bool for the admin API response.
type accountExpired struct {
	*store.Account
	Expired bool `json:"expired"`
}

func (s *Server) adminHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	prefix := strings.TrimPrefix(path, "/admin/api")

	switch {
	case prefix == "/settings":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSettings(w, r)
	case prefix == "/accounts":
		if r.Method == http.MethodGet {
			s.listAccounts(w, r)
			return
		}
		if r.Method == http.MethodPost {
			s.createAccount(w, r)
			return
		}
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	case strings.HasPrefix(prefix, "/accounts/"):
		id := strings.TrimPrefix(prefix, "/accounts/")
		if len(strings.Split(id, "/")) > 1 {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method == http.MethodDelete {
			s.deleteAccount(w, r, id)
			return
		}
		writeErr(w, http.StatusNotFound, "not found")
	case prefix == "/tokens":
		if r.Method == http.MethodGet {
			s.listTokens(w, r)
			return
		}
		if r.Method == http.MethodPost {
			s.createToken(w, r)
			return
		}
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	case strings.HasPrefix(prefix, "/tokens/"):
		id := strings.TrimPrefix(prefix, "/tokens/")
		if r.Method == http.MethodDelete {
			s.deleteToken(w, r, id)
			return
		}
		writeErr(w, http.StatusNotFound, "not found")
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{
		"listen":   s.listen,
		"data_dir": s.dataDir,
		"version":  "dev",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	accs, err := s.accounts.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to list accounts")
		return
	}
	out := make([]accountExpired, len(accs))
	for i, a := range accs {
		out[i] = accountExpired{Account: a, Expired: a.Expired()}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unable to read body")
		return
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON")
		return
	}

	acc, err := s.accounts.Import(json.RawMessage(rawBody))
	if err != nil {
		var importErr *store.ImportError
		if errors.As(err, &importErr) {
			writeErr(w, http.StatusBadRequest, importErr.Reason)
			return
		}
		writeErr(w, http.StatusInternalServerError, "import failed")
		return
	}

	if err := s.accounts.Save(acc); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to save account")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(acc)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := s.accounts.Get(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to get account")
		return
	}
	if err := s.accounts.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete account")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.tokens.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to list tokens")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toks)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	tok, err := s.tokens.Create()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create token")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(tok)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, id string) {
	toks, _ := s.tokens.List()
	found := false
	for _, t := range toks {
		if t.ID == id {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	if err := s.tokens.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to delete token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeErr writes a JSON {"error":"..."} response with status code and content-type.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
