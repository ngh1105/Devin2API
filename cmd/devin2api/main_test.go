package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
)

// TestRunLoginSavesAccount simulates the CLI OAuth login flow against fake
// Devin endpoints.
func TestRunLoginSavesAccount(t *testing.T) {
	tmp := t.TempDir()
	portCh := make(chan int, 1)

	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/cli/continue" {
			http.NotFound(w, r)
			return
		}
		state := r.URL.Query().Get("state")
		redirect := r.URL.Query().Get("redirect_uri")
		w.Header().Set("Location", redirect+"?code=fake-code&state="+state)
		w.WriteHeader(http.StatusFound)
	}))
	defer appServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/cli/token":
			var req struct {
				Code         string `json:"code"`
				CodeVerifier string `json:"code_verifier"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"token": "eyJhbGciOiJIUzI1NiJ9.test"})
		case "/v3/self":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"user_id":        "uid-test-123",
				"user_name":      "testuser",
				"org_id":         "org-abc",
				"principal_type": "cli",
			})
		case "/exa.seat_management_pb.SeatManagementService/GetUserStatus":
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiServer.Close()

	devin.APIBaseURL = apiServer.URL
	devin.AppBaseURL = appServer.URL
	devin.ServerBaseURL = apiServer.URL

	cfg := config.Config{
		Listen:  "127.0.0.1:0",
		DataDir: tmp,
		BaseURL: "", // leave empty — ExchangeCode uses devin.APIBaseURL, FetchSelfProfile uses same
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runLogin(context.Background(), os.Stdout, cfg, func(p int) { portCh <- p })
	}()

	port := <-portCh
	time.Sleep(100 * time.Millisecond)

	cbReq, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/callback?code=fake-code&state=%s",
			port, devin.LastVerifier()), nil)
	if _, err := http.DefaultClient.Do(cbReq); err != nil {
		t.Fatal(err)
	}

	err := <-errCh
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(tmp + "/accounts/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no account files written")
	}

	data, err := os.ReadFile(tmp + "/accounts/" + entries[0].Name())
	if err != nil {
		t.Fatal(err)
	}

	var acct map[string]any
	if err := json.Unmarshal(data, &acct); err != nil {
		t.Fatal(err)
	}

	tokens, ok := acct["tokens"].(map[string]any)
	if !ok || tokens == nil {
		t.Fatal("no 'tokens' object in saved account")
	}

	st, ok := tokens["session_token"].(string)
	if !ok || st == "" {
		t.Error("missing session_token")
	}
	if !strings.HasPrefix(st, "devin-session-token$") {
		t.Errorf("session_token missing prefix: %q", st)
	}
}
