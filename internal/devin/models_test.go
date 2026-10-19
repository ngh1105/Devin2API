package devin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// modelConfig encodes one ModelConfig {1 label, 5 multimodal, 18 context, 22 uid}.
func modelConfig(uid, label string, ctx int, multimodal bool) []byte {
	var b []byte
	b = append(b, pwFieldString(1, label)...)
	if multimodal {
		b = append(b, pwFieldVarint(5, 1)...)
	}
	b = append(b, pwFieldVarint(18, uint64(ctx))...)
	b = append(b, pwFieldString(22, uid)...)
	return PwFieldBytes(1, b)
}

func TestFetchModels(t *testing.T) {
	var body []byte
	body = append(body, modelConfig("swe-2-high", "SWE-2 High", 262000, true)...)
	body = append(body, modelConfig("gpt-5-4-medium", "GPT-5.4 Medium", 400000, false)...)
	body = append(body, PwFieldBytes(1, pwFieldString(1, "no uid"))...) // skipped
	body = append(body, pwFieldVarint(3, 7)...)                         // unrelated top-level field

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != modelsPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Basic tok-tok" || r.Header.Get("Content-Type") != "application/proto" {
			t.Errorf("headers = %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/proto")
		w.Write(body)
	}))
	defer srv.Close()

	models, err := FetchModels(context.Background(), srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models: %+v", len(models), models)
	}
	want := ModelInfo{UID: "swe-2-high", Label: "SWE-2 High", ContextLength: 262000, Multimodal: true}
	if models[0] != want {
		t.Errorf("models[0] = %+v, want %+v", models[0], want)
	}
	if models[1].Multimodal {
		t.Error("models[1] should not be multimodal")
	}
}

func TestFetchModels401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := FetchModels(context.Background(), srv.URL, "tok"); err == nil {
		t.Fatal("expected error")
	}
}

func TestMatchModel(t *testing.T) {
	catalog := []ModelInfo{{UID: "swe-2-high"}, {UID: "gpt-5-4-medium"}, {UID: "gpt-5-4-high"}, {UID: "kimi-k2-6"}, {UID: "glm-5-3-high"}}
	for in, want := range map[string]string{
		"":             "swe-2-high",
		"devin":        "swe-2-high",
		"gpt-5-4":      "gpt-5-4-medium", // family name -> medium first
		"gpt-5.4-high": "gpt-5-4-high",
		"kimi-k2-6":    "kimi-k2-6",
		"glm-5-3":      "glm-5-3-high", // no medium -> high
		"unknown-x":    "unknown-x",    // passed through for upstream to reject
	} {
		if got := MatchModel(in, catalog); got != want {
			t.Errorf("MatchModel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := MatchModel("gpt-5-4", nil); got != "gpt-5-4" {
		t.Errorf("empty catalog: got %q", got)
	}
}

// TestMatchModelKeepsCatalogCase: upstream UIDs are case-sensitive, so a
// lowercase request must resolve to the catalog's spelling.
func TestMatchModelKeepsCatalogCase(t *testing.T) {
	catalog := []ModelInfo{{UID: "MODEL_PRIVATE_11"}, {UID: "swe-2-medium"}}
	if got := MatchModel("model_private_11", catalog); got != "MODEL_PRIVATE_11" {
		t.Errorf("got %q, want MODEL_PRIVATE_11", got)
	}
	if got := MatchModel("SWE-2", catalog); got != "swe-2-medium" {
		t.Errorf("got %q, want swe-2-medium", got)
	}
}
