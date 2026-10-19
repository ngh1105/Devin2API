package devin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// modelsPath is a unary Connect RPC returning every model the account can use.
const modelsPath = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"

// ModelInfo describes one upstream chat model.
type ModelInfo struct {
	UID           string // upstream model UID, sent as GetChatMessage field 21
	Label         string // display name, e.g. "Claude Sonnet 4.6"
	ContextLength int
	Multimodal    bool
}

// FetchModels calls GetCliModelConfigs and returns the model catalog.
// The request is ClientMetadata as field 1; the response holds one
// ModelConfig per repeated field 1 {1 label, 5 multimodal, 18 context, 22 uid}.
func FetchModels(ctx context.Context, serverBase, sessionToken string) ([]ModelInfo, error) {
	if serverBase == "" {
		serverBase = ServerBaseURL
	}
	body := PwFieldBytes(1, clientMetadata(sessionToken, GenerateFingerprint("devin2api-cli")))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverBase+modelsPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+sessionToken+"-"+sessionToken)
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := chatHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpStatusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return nil, fmt.Errorf("read models: %w", err)
	}
	return ParseModels(data)
}

// ParseModels decodes a GetCliModelConfigs response body.
func ParseModels(data []byte) ([]ModelInfo, error) {
	var out []ModelInfo
	r := &pwReader{data: data}
	for {
		field, wire, val, _, ok := r.Next()
		if !ok {
			break
		}
		if field == 1 && wire == wireBytes {
			if m := parseModelConfig(val); m.UID != "" {
				out = append(out, m)
			}
		}
	}
	if err := r.Done(); err != nil {
		return out, fmt.Errorf("parse models: %w", err)
	}
	return out, nil
}

func parseModelConfig(data []byte) ModelInfo {
	var m ModelInfo
	r := &pwReader{data: data}
	for {
		field, wire, val, v, ok := r.Next()
		if !ok {
			return m
		}
		switch {
		case field == 1 && wire == wireBytes:
			m.Label = string(val)
		case field == 5 && wire == wireVarint:
			m.Multimodal = v == 1
		case field == 18 && wire == wireVarint:
			m.ContextLength = int(v)
		case field == 22 && wire == wireBytes:
			m.UID = string(val)
		}
	}
}

// HasModel reports whether uid is in the catalog (case-insensitive).
func HasModel(uid string, catalog []ModelInfo) bool {
	for _, m := range catalog {
		if strings.EqualFold(m.UID, uid) {
			return true
		}
	}
	return false
}

// effortSuffixes is the order tried when a client names a model family
// without an effort level ("gpt-5-4" -> "gpt-5-4-medium").
var effortSuffixes = []string{"", "-medium", "-high", "-low"}

// MatchModel resolves a client model name against the catalog: the name is
// normalised with ResolveModel, then matched exactly or with an effort
// suffix. It returns the normalised name unchanged when nothing matches,
// leaving upstream to reject unknown models.
func MatchModel(name string, catalog []ModelInfo) string {
	uid := ResolveModel(name)
	if len(catalog) == 0 {
		return uid
	}
	known := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		known[strings.ToLower(m.UID)] = true
	}
	for _, suf := range effortSuffixes {
		if cand := uid + suf; known[strings.ToLower(cand)] {
			return cand
		}
	}
	return uid
}
