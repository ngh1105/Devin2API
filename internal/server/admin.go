package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/ngh1105/Devin2API/internal/auth"
	"github.com/ngh1105/Devin2API/internal/store"
)

// accountView is the admin API representation of an Account. Token values
// are masked: the panel never needs them, and returning them would let any
// page that can read the API exfiltrate live Devin credentials.
type accountView struct {
	*store.Account
	Expired bool `json:"expired"`
}

func viewAccount(a *store.Account) accountView {
	cp := *a
	cp.Tokens.AccessToken = store.MaskSecret(a.Tokens.AccessToken)
	cp.Tokens.RefreshToken = store.MaskSecret(a.Tokens.RefreshToken)
	cp.Tokens.IDToken = store.MaskSecret(a.Tokens.IDToken)
	return accountView{Account: &cp, Expired: a.Expired()}
}

func (s *Server) adminHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/api")

	switch {
	case path == "/settings":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSettings(w, r)
	case path == "/accounts":
		switch r.Method {
		case http.MethodGet:
			s.listAccounts(w, r)
		case http.MethodPost:
			s.createAccount(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case strings.HasPrefix(path, "/accounts/"):
		rest := strings.TrimPrefix(path, "/accounts/")
		id, action, _ := strings.Cut(rest, "/")
		if !store.ValidID(id) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		switch {
		case action == "" && r.Method == http.MethodDelete:
			s.deleteAccount(w, r, id)
		case action == "refresh" && r.Method == http.MethodPost:
			s.refreshAccount(w, r, id)
		case action == "" || action == "refresh":
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		default:
			writeErr(w, http.StatusNotFound, "not found")
		}
	case path == "/tokens":
		switch r.Method {
		case http.MethodGet:
			s.listTokens(w, r)
		case http.MethodPost:
			s.createToken(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case strings.HasPrefix(path, "/tokens/"):
		id := strings.TrimPrefix(path, "/tokens/")
		if r.Method != http.MethodDelete {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.deleteToken(w, r, id)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"listen":   s.listen,
		"data_dir": s.dataDir,
		"version":  "dev",
	})
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	accs, err := s.accounts.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to list accounts")
		return
	}
	out := make([]accountView, len(accs))
	for i, a := range accs {
		out[i] = viewAccount(a)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unable to read body")
		return
	}

	var creds struct {
		Email    *string `json:"email"`
		Password *string `json:"password"`
	}
	if err := json.Unmarshal(rawBody, &creds); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON")
		return
	}
	if creds.Email != nil && creds.Password != nil {
		s.handleLoginFlow(w, r, *creds.Email, *creds.Password)
		return
	}

	acc, err := s.accounts.Import(json.RawMessage(rawBody))
	if err != nil {
		var importErr *store.ImportError
		if errors.As(err, &importErr) {
			writeErr(w, http.StatusBadRequest, importErr.Reason)
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to save account")
		return
	}
	writeJSON(w, http.StatusCreated, viewAccount(acc))
}

func (s *Server) handleLoginFlow(w http.ResponseWriter, r *http.Request, email, password string) {
	if s.auth == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth client not configured")
		return
	}
	if email == "" || password == "" {
		writeErr(w, http.StatusBadRequest, "email and password are required")
		return
	}

	tokens, err := s.auth.Login(r.Context(), email, password)
	if err != nil {
		writeAuthErr(w, err, "login failed")
		return
	}
	profile, err := s.auth.UserInfo(r.Context(), tokens.AccessToken)
	if err != nil {
		writeAuthErr(w, err, "userinfo failed")
		return
	}

	acct, err := s.accounts.Upsert(store.AccountFromLogin(email, tokens, profile))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to save account")
		return
	}
	writeJSON(w, http.StatusCreated, viewAccount(acct))
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

func (s *Server) refreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	if s.auth == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth client not configured")
		return
	}
	acc, err := s.accounts.Get(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to get account")
		return
	}
	if acc.Tokens.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "no refresh token")
		return
	}

	newTokens, err := s.auth.Refresh(r.Context(), acc.Tokens.RefreshToken)
	if err != nil {
		writeAuthErr(w, err, "refresh failed")
		return
	}
	updated, err := s.accounts.UpdateTokens(id, newTokens)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, viewAccount(updated))
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.tokens.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to list tokens")
		return
	}
	// The plaintext value is only returned once, by createToken.
	for i := range toks {
		toks[i].Token = store.MaskSecret(toks[i].Token)
	}
	writeJSON(w, http.StatusOK, toks)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	tok, err := s.tokens.Create()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to create token")
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.tokens.Delete(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "token not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to delete token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeAuthErr maps an auth failure to 502, exposing AuthError details only.
func writeAuthErr(w http.ResponseWriter, err error, fallback string) {
	var authErr *auth.AuthError
	if errors.As(err, &authErr) {
		writeErr(w, http.StatusBadGateway, authErr.Error())
		return
	}
	writeErr(w, http.StatusBadGateway, fallback)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeErr writes a JSON {"error":"..."} response with status code and content-type.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
