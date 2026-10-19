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

	"github.com/ngh1105/Devin2API/internal/buildinfo"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

// --- accountExpired wrapper for list -----------------------------------------

// accountExpired is the admin API view of an Account. The session token is
// masked: the panel only needs to know one exists, and returning it would let
// any page that can read the API exfiltrate a live Devin credential.
type accountExpired struct {
	*store.Account
	Expired bool `json:"expired"`
}

func viewAccount(a *store.Account) accountExpired {
	cp := *a
	cp.Tokens.SessionToken = store.MaskSecret(a.Tokens.SessionToken)
	return accountExpired{Account: &cp, Expired: a.Expired()}
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
	case prefix == "/session":
		s.handleSession(w, r)
	case prefix == "/login":
		s.handleLogin(w, r)
	case prefix == "/logout":
		s.handleLogout(w, r)
	case prefix == "/logs":
		s.handleLogs(w, r)
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
		if !store.ValidID(parts[0]) || parts[1] != "usage/refresh" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		s.usageRefresh(w, r, parts[0])
	case strings.HasPrefix(prefix, "/accounts/"):
		id := strings.TrimPrefix(prefix, "/accounts/")
		if !store.ValidID(id) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		switch r.Method {
		case http.MethodDelete:
			s.deleteAccount(w, r, id)
		case http.MethodPatch:
			s.patchAccount(w, r, id)
		default:
			writeErr(w, http.StatusNotFound, "not found")
		}
	case prefix == "/tokens":
		switch r.Method {
		case http.MethodGet:
			s.listTokens(w, r)
		case http.MethodPost:
			s.createToken(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case strings.HasPrefix(prefix, "/tokens/"):
		id := strings.TrimPrefix(prefix, "/tokens/")
		if r.Method != http.MethodDelete {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		s.deleteToken(w, r, id)
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
	mgrGen = s.loginMngr.genSnapshot() // snap AFTER gen bump so Resolve* calls match

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

		// Upsert: logging in again with the same Devin user updates it in place.
		acc, err := s.accounts.Upsert(store.AccountFromLogin(profile.UserName, profile.UserID, profile.OrgID, rawToken, profile.Email, usage))
		if err != nil {
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
	return s.accounts.Upsert(store.AccountFromLogin(profile.UserName, profile.UserID, profile.OrgID, rawToken, profile.Email, usage))
}

// ---------------------------------------------------------------------------
// Existing handlers
// ---------------------------------------------------------------------------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{
		"listen":   s.listen,
		"data_dir": s.dataDir,
		"version":  buildinfo.String(),
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
		out[i] = viewAccount(a)
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
		writeErr(w, http.StatusInternalServerError, "unable to save account")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(viewAccount(acct))
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
	// The plaintext value is only returned once, by createToken.
	for i := range toks {
		toks[i].Token = store.MaskSecret(toks[i].Token)
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
	// Post-cancel gen bump: ensures any concurrent Resolve* from async goroutines
	// sees a mismatched mgrGen and is treated as stale → ignored. This guarantees
	// that cancel always wins the race against background resolves.
	s.loginMngr.mu.Lock()
	s.loginMngr.gen++
	s.loginMngr.statusCode = "idle"
	s.loginMngr.current = nil
	s.loginMngr.accountID = ""
	s.loginMngr.errorMsg = ""
	s.loginMngr.mu.Unlock()
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
	json.NewEncoder(w).Encode(viewAccount(acc))
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
	updated, err := s.refreshUsage(ctx, id)
	done()
	switch {
	case errors.Is(err, devin.ErrTokenExpired):
		writeErr(w, http.StatusBadGateway, "token expired or revoked")
		return
	case errors.Is(err, os.ErrNotExist):
		writeErr(w, http.StatusNotFound, "account not found")
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(viewAccount(updated))
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

	var nickname *string
	var enabled *bool
	if v, ok := rawBody["nickname"]; ok && *v != nil && string(*v) != "null" {
		var val string
		if err := json.Unmarshal(*v, &val); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid nickname")
			return
		}
		nickname = &val
	}
	if v, ok := rawBody["enabled"]; ok && *v != nil && string(*v) != "null" {
		var val bool
		if err := json.Unmarshal(*v, &val); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid enabled")
			return
		}
		enabled = &val
	}

	// Patch applies the change under the store lock, so a concurrent
	// background usage refresh cannot overwrite it.
	if err := s.accounts.Patch(id, nickname, enabled); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "unable to save account")
		return
	}
	acct, err := s.accounts.Get(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to get account")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(viewAccount(acct))
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
	resp := map[string]any{
		"accounts": total,
		"enabled":  enabled,
		"models":   len(s.cachedModels()),
		"requests": s.stats.requests.Load(),
		"failed":   s.stats.failed.Load(),
		"since":    s.stats.since,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// writeErr writes a JSON {"error":"..."} response with status code and content-type.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
