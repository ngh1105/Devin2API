package devin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchSelfProfile(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/self" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer eyfaketoken" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"user_id":        "user-123",
			"user_name":      "testuser",
			"org_id":         "org-456",
			"principal_type": "cli",
		})
	}))
	defer ts.Close()

	p, err := FetchSelfProfile(context.Background(), ts.URL, "eyfaketoken")
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != "user-123" {
		t.Errorf("UserID = %q; want %q", p.UserID, "user-123")
	}
	if p.UserName != "testuser" {
		t.Errorf("UserName = %q; want %q", p.UserName, "testuser")
	}
	if p.OrgID != "org-456" {
		t.Errorf("OrgID = %q; want %q", p.OrgID, "org-456")
	}
	if p.PrincipalType != "cli" {
		t.Errorf("PrincipalType = %q; want %q", p.PrincipalType, "cli")
	}
}

func TestFetchSelfProfile401(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	_, err := FetchSelfProfile(context.Background(), ts.URL, "badtoken")
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q; expected to contain 401", err.Error())
	}
}
