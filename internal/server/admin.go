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
		s.handleSettings(w, r)
	case prefix == "/settings/upstream-test":
		s.handleUpstreamTest(w, r)
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
	case prefix == "/cache":
		s.handleCache(w, r)
	case prefix == "/session":
		s.handleSession(w, r)
	case prefix == "/login":
		s.handleLogin(w, r)
	case prefix == "/logout":
		s.handleLogout(w, r)
	case prefix == "/models":
		s.handleAdminModels(w, r)
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
		switch r.Method {
		case http.MethodDelete:
			s.deleteToken(w, r, id)
		case http.MethodPut:
			s.updateToken(w, r, id)
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

	mgrGen := s.loginMngr.SetPending(login)

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
		usage, _ = s.fetchUserStatus(useCtx, sessToken)
		useDone()

		// A cancel (or a newer login) after the code arrived must not still
		// add the account: check and upsert under the manager's lock, so a
		// cancel cannot slip in between.
		var acc *store.Account
		s.loginMngr.mu.Lock()
		if s.loginMngr.gen == mgrGen {
			// Upsert: logging in again with the same Devin user updates it in place.
			acc, err = s.accounts.Upsert(store.AccountFromLogin(profile.UserName, profile.UserID, profile.OrgID, rawToken, profile.Email, usage))
		}
		s.loginMngr.mu.Unlock()
		if acc == nil && err == nil {
			return // cancelled
		}
		if err != nil {
			s.loginMngr.ResolveError(err.Error(), mgrGen)
			return
		}

		s.loginMngr.ResolveComplete(acc.ID, mgrGen)
	}()

	return login.LoginURL, nil
}

// handleLoginManualDirect executes the full manual login business logic given a token.
func handleLoginManualDirect(ctx context.Context, s *Server, rawToken string) (*store.Account, error) {
	sessToken := devin.FormatSessionToken(rawToken)
	profile, err := devin.FetchSelfProfile(ctx, s.loginAPIBase, sessToken)
	if err != nil {
		return nil, err
	}
	var usage *store.Usage
	useCtx, useDone := context.WithTimeout(ctx, time.Second*120)
	usage, _ = s.fetchUserStatus(useCtx, sessToken)
	useDone()
	return s.accounts.Upsert(store.AccountFromLogin(profile.UserName, profile.UserID, profile.OrgID, rawToken, profile.Email, usage))
}

// ---------------------------------------------------------------------------
// Existing handlers
// ---------------------------------------------------------------------------

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

	acc, err := handleLoginManualDirect(r.Context(), s, req.Token)
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
	// A JSON null decodes into a nil pointer, so a field is only present
	// when its pointer is set and does not spell "null".
	rawBody := make(map[string]*json.RawMessage)
	if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	field := func(name string) (json.RawMessage, bool) {
		v, ok := rawBody[name]
		if !ok || v == nil || string(*v) == "null" {
			return nil, false
		}
		return *v, true
	}

	var nickname *string
	var enabled *bool
	if raw, ok := field("nickname"); ok {
		var val string
		if err := json.Unmarshal(raw, &val); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid nickname")
			return
		}
		nickname = &val
	}
	if raw, ok := field("enabled"); ok {
		var val bool
		if err := json.Unmarshal(raw, &val); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid enabled")
			return
		}
		enabled = &val
	}
	if nickname == nil && enabled == nil {
		writeErr(w, http.StatusBadRequest, "empty patch")
		return
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
		"hourly":   s.stats.hourly(24, time.Now(), false),
		"cache":    s.cacheView(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleCache serves GET /admin/api/cache (state and totals) and DELETE,
// which drops every cached answer.
func (s *Server) handleCache(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeAdminJSON(w, http.StatusOK, s.cacheView())
	case http.MethodDelete:
		if err := s.cache.clear(); err != nil {
			writeErr(w, http.StatusInternalServerError, "unable to clear the response cache")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// writeErr writes a JSON {"error":"..."} response with status code and content-type.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
