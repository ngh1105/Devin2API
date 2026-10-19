package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie = "d2_admin"
	sessionTTL    = 7 * 24 * time.Hour
	// Failed logins are slowed down and capped per minute per client IP,
	// so a password exposed on the LAN cannot be brute-forced quickly and
	// one noisy client cannot lock everyone else out.
	loginFailsPerMinute = 10
)

// loginFailDelay is a var so tests can skip the wait.
var loginFailDelay = 500 * time.Millisecond

// adminAuth guards the panel's admin API with a password when one is
// configured. Sessions are random cookie tokens kept in memory, so a
// restart signs everyone out.
type adminAuth struct {
	password string // "" = no login required

	mu       sync.Mutex
	sessions map[string]time.Time  // token -> expiry
	fails    map[string]failWindow // client IP -> failures this minute
}

type failWindow struct {
	start time.Time
	count int
}

func (a *adminAuth) enabled() bool { return a.password != "" }

func (a *adminAuth) checkPassword(p string) bool {
	// Hash both sides so the comparison is constant-time regardless of length.
	want, got := sha256.Sum256([]byte(a.password)), sha256.Sum256([]byte(p))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// clientIP is the request's remote address without the port. Behind a
// reverse proxy every client shares the proxy's IP, which only makes the
// limit stricter.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// allowAttempt reports whether ip may try another login now.
func (a *adminAuth) allowAttempt(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, w := range a.fails {
		if now.Sub(w.start) > time.Minute {
			delete(a.fails, k)
		}
	}
	return a.fails[ip].count < loginFailsPerMinute
}

func (a *adminAuth) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fails == nil {
		a.fails = map[string]failWindow{}
	}
	w := a.fails[ip]
	if w.count == 0 {
		w.start = time.Now()
	}
	w.count++
	a.fails[ip] = w
}

func (a *adminAuth) newSession() string {
	tok := randID() + randID()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions == nil {
		a.sessions = map[string]time.Time{}
	}
	now := time.Now()
	for t, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, t)
		}
	}
	a.sessions[tok] = now.Add(sessionTTL)
	return tok
}

// authenticated reports whether r carries a live session cookie (always
// true when no password is configured).
func (a *adminAuth) authenticated(r *http.Request) bool {
	if !a.enabled() {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

func (a *adminAuth) endSession(r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
}

func setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/",
		MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// adminPublicPaths are reachable without a session: the login flow itself.
var adminPublicPaths = map[string]bool{
	"/admin/api/session": true,
	"/admin/api/login":   true,
	"/admin/api/logout":  true,
}

// handleSession serves GET /admin/api/session for the panel's boot.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"auth_required": s.auth.enabled(),
		"authenticated": s.auth.authenticated(r),
	})
}

// handleLogin serves POST /admin/api/login {"password": "..."}.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.auth.enabled() {
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
		return
	}
	ip := clientIP(r)
	if !s.auth.allowAttempt(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many failed logins; try again in a minute")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !s.auth.checkPassword(body.Password) {
		s.auth.recordFailure(ip)
		time.Sleep(loginFailDelay)
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	setSessionCookie(w, s.auth.newSession(), int(sessionTTL/time.Second))
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

// handleLogout serves POST /admin/api/logout.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.auth.endSession(r)
	setSessionCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}
