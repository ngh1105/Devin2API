package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/openai"
	"github.com/ngh1105/Devin2API/internal/store"
)

func makeTestServer(t *testing.T, tmpDir string) *Server {
	t.Helper()
	acc, err := store.NewAccountStore(tmpDir)
	if err != nil {
		t.Fatalf("new account store: %v", err)
	}
	tok, err := store.NewTokenStore(tmpDir)
	if err != nil {
		t.Fatalf("new token store: %v", err)
	}
	return New(config.Config{Listen: ":0"}, acc, tok, nil)
}

// TestHealthz ensures GET /healthz returns {"status":"ok"}.
func TestHealthz(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ok" {
		t.Errorf("body: got %v, want {\"status\":\"ok\"}", body)
	}
}

// TestModels ensures GET /v1/models returns the static model list.
func TestModels(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Object != "list" || len(resp.Data) != 1 {
		t.Errorf("got %+v", resp)
	}
	if resp.Data[0].ID != "devin" || resp.Data[0].OwnedBy != "devin" {
		t.Errorf("model data wrong: %+v", resp.Data[0])
	}
}

// TestV1OpenWhenNoTokens ensures that with an empty token store,
// /v1/chat/completions still reaches the handler and returns 501.
func TestV1OpenWhenNoTokens(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)
	payload := `{"model":"devin","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusNotImplemented)
	}
	var errResp openai.Error
	json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp.Error.Message != "upstream not implemented yet" {
		t.Errorf("error msg: got %q", errResp.Error.Message)
	}
	// Make sure no 401 was returned.
	if strings.Contains(rec.Body.String(), "invalid or missing token") {
		t.Error("expected no auth error with empty token store")
	}
}

// TestV1RejectsBadToken ensures a non-empty store with a bad token → 401.
func TestV1RejectsBadToken(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)
	_, err := srv.tokens.Create()
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	payload := `{"model":"devin","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bad-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var errResp openai.Error
	json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp.Error.Type != "devin2api_error" {
		t.Errorf("error type: got %q", errResp.Error.Type)
	}
}

// TestV1AcceptsCreatedToken verifies the valid token value from Create() is accepted.
func TestV1AcceptsCreatedToken(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)
	tok, err := srv.tokens.Create()
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	payload := `{"model":"devin","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status: got %d, want %d (not 401)", rec.Code, http.StatusNotImplemented)
	}
	// With X-Api-Key header should also work.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req2.Header.Set("X-Api-Key", tok.Token)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotImplemented {
		t.Errorf("x-api-key status: got %d, want %d", rec2.Code, http.StatusNotImplemented)
	}
}

// TestV1RejectsWrongScheme ensures only "Bearer" is accepted, and an empty
// bearer value never authenticates.
func TestV1RejectsWrongScheme(t *testing.T) {
	srv := makeTestServer(t, t.TempDir())
	tok, _ := srv.tokens.Create()
	for _, h := range []string{"Basic " + tok.Token, "Bearer ", "Bearer", "XXXXXXX" + tok.Token} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", h)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: got %d, want 401", h, rec.Code)
		}
	}
}

// TestV1TouchesToken ensures a successful request records last_used.
func TestV1TouchesToken(t *testing.T) {
	srv := makeTestServer(t, t.TempDir())
	tok, _ := srv.tokens.Create()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "bearer "+tok.Token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	list, _ := srv.tokens.List()
	if list[0].LastUsed == nil {
		t.Error("last_used not set")
	}
}

// TestChatCompletionsBadBody verifies 400 for invalid JSON.
func TestChatCompletionsBadBody(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var errResp openai.Error
	json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp.Error.Message != "invalid JSON" {
		t.Errorf("error msg: got %q", errResp.Error.Message)
	}
}

// TestChatCompletionsStreamStub verifies SSE stream mode for chat completions stub.
func TestChatCompletionsStreamStub(t *testing.T) {
	tmp := t.TempDir()
	srv := makeTestServer(t, tmp)
	payload := `{"model":"devin","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("content-type: got %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream not implemented") {
		t.Errorf("body missing upstream msg: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("body missing [DONE]: %q", body)
	}
	if !strings.Contains(body, "data: ") {
		t.Errorf("body missing SSE data: prefix: %q", body)
	}
}

func TestTokenListEmpty(t *testing.T) {
	tmp := t.TempDir()
	ts, err := store.NewTokenStore(tmp)
	if err != nil {
		t.Fatalf("new token store: %v", err)
	}
	list, err := ts.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d tokens", len(list))
	}
}
