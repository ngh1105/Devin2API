package devin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SelfProfile holds the response from GET /v3/self.
type SelfProfile struct {
	UserID        string `json:"user_id"`
	UserName      string `json:"user_name"`
	OrgID         string `json:"org_id"`
	PrincipalType string `json:"principal_type"`
	Email         string `json:"email"`
}

// FetchSelfProfile calls GET <apiBase>/v3/self with Bearer session-token and
// returns the decoded SelfProfile. apiBase defaults to APIBaseURL.
func FetchSelfProfile(ctx context.Context, apiBase, sessionToken string) (SelfProfile, error) {
	if apiBase == "" {
		apiBase = APIBaseURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/v3/self", nil)
	if err != nil {
		return SelfProfile{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return SelfProfile{}, fmt.Errorf("self profile request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return SelfProfile{}, fmt.Errorf("self profile: status %d", resp.StatusCode)
	}

	var p SelfProfile
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return SelfProfile{}, fmt.Errorf("decode self profile: %w", err)
	}
	return p, nil
}
