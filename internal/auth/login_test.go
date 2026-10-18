package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// makeTestClient builds an http.Client for fake-Auth0 flows.
// Returns every redirect as-is (ErrUseLastResponse) so the caller can
// inspect Location headers and extract intermediate state manually.
func makeTestClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func check(tb testing.TB, field, got, want string) {
	tb.Helper()
	if got != want {
		tb.Errorf("%s: got %q, want %q", field, got, want)
	}
}

func checkInt(tb testing.TB, field string, got int64, want int64) {
	tb.Helper()
	if got != want {
		tb.Errorf("%s: got %d, want %d", field, got, want)
	}
}

// ---------------------------------------------------------------------------
// TestLoginSuccess
// ---------------------------------------------------------------------------

func TestLoginSuccess(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/authorize" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/identifier?state="+url.PathEscape(state))
			w.Header().Add("Set-Cookie", "did/auth0=sess; Path=/")
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/u/login/identifier" && r.Method == "GET" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><form id="f"><input name="state" value="` + state + `"/></form></html>`))
			return
		}
		if path == "/u/login/identifier" && r.Method == "POST" {
			r.ParseForm()
			state := r.FormValue("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/password?state="+url.PathEscape(state))
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/u/login/password" && r.Method == "GET" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><form><input name="state" value="` + state + `"/></form></html>`))
			return
		}
		if path == "/u/login/password" && r.Method == "POST" {
			r.ParseForm()
			code := "c-ok"
			loc := "https://app.devin.ai/post-auth?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(r.FormValue("state"))
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/oauth/token" && r.Method == "POST" {
			r.ParseForm()
			if r.FormValue("grant_type") == "authorization_code" {
				if r.FormValue("code_verifier") == "" {
					t.Error("expected non-empty code_verifier")
				}
				wantRedir := "https://app.devin.ai/post-auth"
				if r.FormValue("redirect_uri") != wantRedir {
					t.Errorf("redirect_uri=%q want %q", r.FormValue("redirect_uri"), wantRedir)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok-ok","refresh_token":"rt-ok","id_token":"idt-ok","token_type":"Bearer","expires_in":3600,"scope":"openid profile email"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(WithBaseURL(ts.URL), WithHTTPClient(makeTestClient()))

	ctx := context.Background()
	tokens, err := c.Login(ctx, "user@test.io", "pass1")
	if err != nil {
		t.Fatal(err)
	}
	check(t, "AccessToken", tokens.AccessToken, "tok-ok")
	check(t, "RefreshToken", tokens.RefreshToken, "rt-ok")
	check(t, "IDToken", tokens.IDToken, "idt-ok")
	check(t, "TokenType", tokens.TokenType, "Bearer")
	checkInt(t, "ExpiresIn", tokens.ExpiresIn, 3600)
	check(t, "Scope", tokens.Scope, "openid profile email")
}

// ---------------------------------------------------------------------------
// TestLoginWrongPassword
// ---------------------------------------------------------------------------

func TestLoginWrongPassword(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/authorize" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/identifier?state="+url.PathEscape(state))
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/u/login/identifier" && r.Method == "GET" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><form><input name="state" value="` + state + `"/></form></html>`))
			return
		}
		if path == "/u/login/identifier" && r.Method == "POST" {
			r.ParseForm()
			state := r.FormValue("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/password?state="+url.PathEscape(state))
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/u/login/password" && r.Method == "GET" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><form><input name="state" value="` + state + `"/></form></html>`))
			return
		}
		if path == "/u/login/password" && r.Method == "POST" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body><h1>Login Error</h1><p>Invalid credentials.</p></body></html>`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(WithBaseURL(ts.URL), WithHTTPClient(makeTestClient()))

	ctx := context.Background()
	_, err := c.Login(ctx, "user@test.io", "wrongpass")
	if err == nil {
		t.Fatal("expected error for wrong password")
	}
	aerr, ok := err.(*AuthError)
	if !ok {
		t.Fatalf("expected *AuthError, got %T", err)
	}
	if aerr.Stage != "password" {
		t.Errorf("stage=%q, want password", aerr.Stage)
	}
}

// ---------------------------------------------------------------------------
// TestLoginFormEncodingSpecialChars
// ---------------------------------------------------------------------------

func TestLoginFormEncodingSpecialChars(t *testing.T) {
	var capturedPwd string
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/authorize" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/identifier?state="+url.PathEscape(state))
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/u/login/identifier" && r.Method == "GET" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><form><input name="state" value="` + state + `"/></form></html>`))
			return
		}
		if path == "/u/login/identifier" && r.Method == "POST" {
			r.ParseForm()
			state := r.FormValue("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/password?state="+url.PathEscape(state))
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/u/login/password" && r.Method == "GET" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><form><input name="state" value="` + state + `"/></form></html>`))
			return
		}
		if path == "/u/login/password" && r.Method == "POST" {
			r.ParseForm()
			capturedPwd = r.FormValue("password")
			code := "c-spc"
			w.Header().Set("Location", "https://app.devin.ai/post-auth?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(r.FormValue("state")))
			w.WriteHeader(http.StatusFound)
			return
		}
		if path == "/oauth/token" && r.Method == "POST" {
			r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"at","refresh_token":"rt","id_token":"idt","token_type":"Bearer","expires_in":3600,"scope":"openid profile email"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(WithBaseURL(ts.URL), WithHTTPClient(makeTestClient()))

	ctx := context.Background()
	specPwd := "p&ss=wo%rd+d"
	_, err := c.Login(ctx, "user@test.io", specPwd)
	if err != nil {
		t.Fatal(err)
	}
	if capturedPwd != specPwd {
		t.Errorf("password round-trip: got %q, want %q", capturedPwd, specPwd)
	}
}

// ---------------------------------------------------------------------------
// TestLoginAuthorizeError
// ---------------------------------------------------------------------------

func TestLoginAuthorizeError(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authorize" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Location",
				ts.URL+"/u/login/identifier?state="+url.PathEscape(state)+"&error=access_denied")
			w.WriteHeader(http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(WithBaseURL(ts.URL), WithHTTPClient(makeTestClient()))

	ctx := context.Background()
	_, err := c.Login(ctx, "user@test.io", "pass")
	if err == nil {
		t.Fatal("expected error")
	}
	aerr, ok := err.(*AuthError)
	if !ok {
		t.Fatalf("expected *AuthError, got %T", err)
	}
	if aerr.Stage != "password" {
		t.Errorf("stage=%q, want password", aerr.Stage)
	}
}

// ---------------------------------------------------------------------------
// TestRefreshSuccess
// ---------------------------------------------------------------------------

func TestRefreshSuccess(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" && r.Method == "POST" {
			r.ParseForm()
			gt := r.FormValue("grant_type")
			if gt != "refresh_token" {
				t.Errorf("grant_type=%q, want refresh_token", gt)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			rt := r.FormValue("refresh_token")
			if rt == "" {
				t.Error("missing refresh_token in request")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","id_token":"new-idt","token_type":"Bearer","expires_in":3600,"scope":"openid profile email"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(WithBaseURL(ts.URL), WithHTTPClient(makeTestClient()))

	ctx := context.Background()
	tokens, err := c.Refresh(ctx, "old-rt")
	if err != nil {
		t.Fatal(err)
	}
	check(t, "RefreshToken", tokens.RefreshToken, "new-rt")
	check(t, "AccessToken", tokens.AccessToken, "new-at")
}

// ---------------------------------------------------------------------------
// TestRefreshKeepsOldTokenWhenOmitted
// ---------------------------------------------------------------------------

func TestRefreshKeepsOldTokenWhenOmitted(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" && r.Method == "POST" {
			r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"kept-at","id_token":"idt","token_type":"Bearer","expires_in":3600,"scope":"openid profile email"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	oldRT := "keep-this"
	c := NewClient(WithBaseURL(ts.URL), WithHTTPClient(makeTestClient()))

	ctx := context.Background()
	tokens, err := c.Refresh(ctx, oldRT)
	if err != nil {
		t.Fatal(err)
	}
	check(t, "RefreshToken", tokens.RefreshToken, oldRT)
	check(t, "AccessToken", tokens.AccessToken, "kept-at")
}

// ---------------------------------------------------------------------------
// TestUserInfoSuccess
// ---------------------------------------------------------------------------

func TestUserInfoSuccess(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo" && r.Method == "GET" {
			auth := r.Header.Get("Authorization")
			expected := "Bearer tok-userinfo"
			if auth != expected {
				t.Errorf("Authorization=%q, want %q", auth, expected)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sub":"uid-1","email":"u@v.com","name":"U V","picture":"https://example.com/pic.jpg"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(WithBaseURL(ts.URL), WithHTTPClient(makeTestClient()))

	ctx := context.Background()
	p, err := c.UserInfo(ctx, "tok-userinfo")
	if err != nil {
		t.Fatal(err)
	}
	check(t, "Sub", p.Sub, "uid-1")
	check(t, "Email", p.Email, "u@v.com")
	check(t, "Name", p.Name, "U V")
	check(t, "Picture", p.Picture, "https://example.com/pic.jpg")
}
