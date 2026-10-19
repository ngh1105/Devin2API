package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/store"
)

// keyPost sends a /v1 POST authenticated with key.
func keyPost(srv *Server, key, path, body string) *httptest.ResponseRecorder {
	req := v1Request(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

const hiChat = `{"model":"devin","messages":[{"role":"user","content":"hi"}]}`

// TestAPIKeyLimits: a key's quota, expiry, disabled flag and model allow
// list are enforced, and its requests are counted and logged under its name.
func TestAPIKeyLimits(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	key, err := srv.tokens.Create(store.TokenOptions{Name: "ci", Quota: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if rec := keyPost(srv, key.Token, "/v1/chat/completions", hiChat); rec.Code != 200 {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := keyPost(srv, key.Token, "/v1/chat/completions", hiChat)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "quota") {
		t.Errorf("over quota: %d %s", rec.Code, rec.Body)
	}
	if rec := keyPost(srv, key.Token, "/v1/messages", `{"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
		t.Errorf("anthropic over quota: %d %s", rec.Code, rec.Body)
	}
	// Counting tokens does not use the quota.
	if rec := keyPost(srv, key.Token, "/v1/messages/count_tokens", `{"messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Errorf("count_tokens over quota: %d", rec.Code)
	}
	if logs := srv.reqLog.recent(0); len(logs) != 2 || logs[0].Key != "ci" {
		t.Errorf("logs = %+v", logs)
	}

	srv.tokens.Update(key.ID, store.TokenOptions{Models: []string{"gpt-5-4-medium"}}, true)
	rec = keyPost(srv, key.Token, "/v1/chat/completions", hiChat)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowed to use model") {
		t.Errorf("model outside allow list: %d %s", rec.Code, rec.Body)
	}
	if rec := keyPost(srv, key.Token, "/v1/chat/completions", `{"model":"gpt-5-4-medium","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Errorf("allowed model: %d %s", rec.Code, rec.Body)
	}
	req := v1Request(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key.Token)
	mrec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(mrec, req)
	var models struct{ Data []struct{ ID string } }
	json.Unmarshal(mrec.Body.Bytes(), &models)
	if len(models.Data) != 1 || models.Data[0].ID != "gpt-5-4-medium" {
		t.Errorf("models for a limited key = %+v", models.Data)
	}

	past := time.Now().Add(-time.Hour)
	srv.tokens.Update(key.ID, store.TokenOptions{ExpiresAt: &past}, false)
	if rec := keyPost(srv, key.Token, "/v1/chat/completions", hiChat); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("expired: %d %s", rec.Code, rec.Body)
	}
	srv.tokens.Update(key.ID, store.TokenOptions{Disabled: true}, false)
	if rec := keyPost(srv, key.Token, "/v1/chat/completions", hiChat); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "disabled") {
		t.Errorf("disabled: %d %s", rec.Code, rec.Body)
	}
}

// TestAdminTokenOptions: keys are created and edited with options, and
// neither the list nor an update ever returns the key or its hash.
func TestAdminTokenOptions(t *testing.T) {
	srv := New(config.Config{Listen: ":0"}, mustAccounts(t), mustTokens(t))
	rec := adminDo(srv, http.MethodPost, "/admin/api/tokens", `{"name":"laptop","quota":100,"models":["swe-2"]}`, nil)
	var created store.Token
	json.Unmarshal(rec.Body.Bytes(), &created)
	if rec.Code != http.StatusCreated || !strings.HasPrefix(created.Token, "d2sk_") || created.Name != "laptop" || created.Quota != 100 || created.Hash != "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := adminDo(srv, http.MethodPost, "/admin/api/tokens", "", nil); rec.Code != http.StatusCreated {
		t.Errorf("create without a body: %d", rec.Code)
	}
	for _, bad := range []string{`{"quota":-1}`, `{"nope":1}`, `{"name":"` + strings.Repeat("x", 65) + `"}`} {
		if rec := adminDo(srv, http.MethodPost, "/admin/api/tokens", bad, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}

	rec = adminDo(srv, http.MethodPut, "/admin/api/tokens/"+created.ID, `{"name":"desk","disabled":true,"reset_requests":true}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"desk"`) || strings.Contains(rec.Body.String(), created.Token) {
		t.Errorf("update: %d %s", rec.Code, rec.Body)
	}
	if rec := adminDo(srv, http.MethodPut, "/admin/api/tokens/t_nope", `{}`, nil); rec.Code != http.StatusNotFound {
		t.Errorf("update unknown: %d", rec.Code)
	}

	rec = adminDo(srv, http.MethodGet, "/admin/api/tokens", "", nil)
	body := rec.Body.String()
	if strings.Contains(body, created.Token) || strings.Contains(body, `"hash"`) || !strings.Contains(body, created.Hint) {
		t.Errorf("list leaks or lacks the hint: %s", body)
	}
}
