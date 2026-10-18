package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

// --- accountExpired wrapper for list -----------------------------------------

type accountExpired struct {
	*store.Account
	Expired bool `json:"expired"`
}

// ---------------------------------------------------------------------------
// Admin route handler
// ---------------------------------------------------------------------------

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
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
		switch r.Method {
		case http.MethodGet:
			s.listAccounts(w, r)
		case http.MethodPost:
			s.createAccount(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case prefix == "/stats":
		s.handleStats(w, r)
	case prefix == "/login/start":
		s.handleLoginStart(w, r)
	case prefix == "/login/status":
		s.handleLoginStatus(w, r)
	case prefix == "/login/cancel":
		s.handleLoginCancel(w, r)
	case prefix == "/login/manual":
		s.handleLoginManual(w, r)
	case strings.HasPrefix(prefix, "/accounts/") && strings.HasSuffix(prefix, "/usage/refresh"):
		parts := strings.SplitN(strings.TrimPrefix(prefix, "/accounts/"), "/", 2)
		s.usageRefresh(w, r, parts[0])
	case strings.HasPrefix(prefix, "/accounts/"):
		id := strings.TrimPrefix(prefix, "/accounts/")
		switch r.Method {
		case http.MethodDelete:
			s.deleteAccount(w, r, id)
		case http.MethodPatch:
			s.patchAccount(w, r, id)
		default:
			writeErr(w, http.StatusNotFound, "not found")
		}
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// --- login lifecycle helpers -------------------------------------------------

func (s *Server) startLogin(ctx context.Context) (string, error) {
	login, err := devin.StartLogin("")
	if err != nil {
		return "", fmt.Errorf("start login: %w", err)
	}

	mgrGen := s.loginMngr.genSnapshot() // capture gen for this goroutine's resolve calls
	s.loginMngr.SetPending(login)

	go func() {
		defer login.Stop()
		result, waitErr := login.Wait(ctx, time.Minute*5)
		if waitErr != nil {
			s.loginMngr.ResolveError(waitErr.Error(), mgrGen)
			return
		}

		rawToken, err := devin.ExchangeCode(context.Background(), s.loginAPIBase, result.Code, login.Verifier)
		if err != nil {
			s.loginMngr.ResolveError(err.Error(), mgrGen)
			return
		}

		sessToken := devin.FormatSessionToken(rawToken)

		profile, err := devin.FetchSelfProfile(context.Background(), s.loginAPIBase, sessToken)
		if err != nil {
			s.loginMngr.ResolveError(err.Error(), mgrGen)
			return
		}

		// Best-effort user status
		var usage *store.Usage
		useCtx, useDone := context.WithTimeout(context.Background(), time.Second*120)
		usage, _ = devin.FetchUserStatus(useCtx, s.loginServerBase, sessToken)
		useDone()

		acc := store.AccountFromLogin(profile.UserName, profile.UserID, profile.OrgID, rawToken, profile.Email, usage)

		if err := s.accounts.Save(acc); err != nil {
			s.loginMngr.ResolveError(err.Error(), mgrGen)
			return
		}

		s.loginMngr.ResolveComplete(acc.ID, mgrGen)
	}()

	return login.LoginURL, nil
}

// handleLoginManualDirect executes the full manual login business logic given a token.
func handleLoginManualDirect(s *Server, rawToken string) (*store.Account, error) {
	sessToken := devin.FormatSessionToken(rawToken)
	profile, err := devin.FetchSelfProfile(context.Background(), s.loginAPIBase, sessToken)
	if err != nil {
		return nil, err
	}
	var usage *store.Usage
	useCtx, useDone := context.WithTimeout(context.Background(), time.Second*120)
	usage, _ = devin.FetchUserStatus(useCtx, s.loginServerBase, sessToken)
	useDone()
	acc := store.AccountFromLogin(profile.UserName, profile.UserID, profile.OrgID, rawToken, profile.Email, usage)
	if err := s.accounts.Save(acc); err != nil {
		return nil, err
	}
	return acc, nil
}

// ---------------------------------------------------------------------------
// Existing handlers
// ---------------------------------------------------------------------------

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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unable to read body")
		return
	}
	acct, err := s.accounts.Import(body)
	if err != nil {
		var importErr *store.ImportError
		if errors.As(err, &importErr) {
			writeErr(w, http.StatusBadRequest, importErr.Reason)
			return
		}
		writeErr(w, http.StatusInternalServerError, "import failed")
		return
	}
	if err := s.accounts.Save(acct); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to save account")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(acct)
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

// ---------------------------------------------------------------------------
// Login lifecycle endpoints
// ---------------------------------------------------------------------------

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	url, err := s.startLogin(context.Background())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"login_url": url})
}

func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	code, aid, msg := s.loginMngr.Status()
	resp := map[string]string{"status": code}
	if aid != "" {
		resp["account_id"] = aid
	}
	if msg != "" {
		resp["error"] = msg
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.loginMngr.Cancel()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{})
}

func (s *Server) handleLoginManual(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Token == "" {
		writeErr(w, http.StatusBadRequest, "token is required")
		return
	}

	acc, err := handleLoginManualDirect(s, req.Token)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(acc)
}

// ---------------------------------------------------------------------------
// Usage refresh
// ---------------------------------------------------------------------------

func (s *Server) usageRefresh(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	acct, err := s.accounts.Get(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to get account")
		return
	}

	if acct.Tokens.SessionToken == "" {
		writeErr(w, http.StatusBadRequest, "no session token")
		return
	}

	ctx, done := context.WithTimeout(r.Context(), time.Second*120)
	usage, err := devin.FetchUserStatus(ctx, s.loginServerBase, acct.Tokens.SessionToken)
	done()

	if errors.Is(err, devin.ErrTokenExpired) {
		acct.UsageErr = "token expired or revoked"
		acct.Usage = nil
		_ = s.accounts.Save(acct)
		writeErr(w, http.StatusBadGateway, "token expired or revoked")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	acct.Usage = usage
	acct.UsageErr = ""
	now := time.Now().Unix()
	if acct.Usage != nil {
		acct.Usage.CheckedAt = now
	}
	if err := s.accounts.Save(acct); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to save account")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(accountExpired{Account: acct, Expired: acct.Expired()})
}

// ---------------------------------------------------------------------------
// Account patch
// ---------------------------------------------------------------------------

func (s *Server) patchAccount(w http.ResponseWriter, r *http.Request, id string) {
	rawBody := make(map[string]*json.RawMessage)
	if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	hasModifiable := false
	if v, ok := rawBody["nickname"]; ok && *v != nil && string(*v) != "null" {
		hasModifiable = true
	}
	if v, ok := rawBody["enabled"]; ok && *v != nil && string(*v) != "null" {
		hasModifiable = true
	}
	if !hasModifiable {
		writeErr(w, http.StatusBadRequest, "empty patch")
		return
	}

	acct, err := s.accounts.Get(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to get account")
		return
	}

	if v, ok := rawBody["nickname"]; ok && *v != nil && string(*v) != "null" {
		var val string
		if err := json.Unmarshal(*v, &val); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid nickname")
			return
		}
		acct.Nickname = val
	}
	if v, ok := rawBody["enabled"]; ok && *v != nil && string(*v) != "null" {
		var val bool
		if err := json.Unmarshal(*v, &val); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid enabled")
			return
		}
		acct.Enabled = val
	}

	if err := s.accounts.Save(acct); err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to save account")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(accountExpired{Account: acct, Expired: acct.Expired()})
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	accs, err := s.accounts.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to list accounts")
		return
	}
	total := len(accs)
	enabled := 0
	for _, a := range accs {
		if a.Enabled {
			enabled++
		}
	}
	resp := map[string]int{
		"accounts": total,
		"enabled":  enabled,
		"requests": 0,
		"failed":   0,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// writeErr writes a JSON {"error":"..."} response with status code and content-type.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
