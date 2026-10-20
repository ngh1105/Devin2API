package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

// apiKeyKey carries the API key that authenticated a /v1 request.
type apiKeyKey struct{}

func withAPIKey(ctx context.Context, k store.Token) context.Context {
	return context.WithValue(ctx, apiKeyKey{}, k)
}

// apiKeyFrom returns the request's API key; false in keyless mode.
func apiKeyFrom(ctx context.Context) (store.Token, bool) {
	k, ok := ctx.Value(apiKeyKey{}).(store.Token)
	return k, ok
}

// keyLabel names an API key for logs: its name, else its masked value.
func keyLabel(k store.Token) string {
	if k.Name != "" {
		return k.Name
	}
	return k.Hint
}

// modelAllowed reports whether the request's API key may use a model,
// given the names it goes by (as requested and as resolved upstream).
func modelAllowed(ctx context.Context, names ...string) bool {
	k, ok := apiKeyFrom(ctx)
	if !ok || len(k.Models) == 0 {
		return true
	}
	for _, n := range names {
		if n != "" && slices.Contains(k.Models, n) {
			return true
		}
	}
	return false
}

// keyCharge records whether the request's counted use of its API key is to
// be kept. v1Middleware counts the request up front and refunds it unless
// startTrace marked it kept, i.e. it went upstream.
type keyCharge struct{ keep bool }

type keyChargeKey struct{}

func withKeyCharge(ctx context.Context, c *keyCharge) context.Context {
	return context.WithValue(ctx, keyChargeKey{}, c)
}

// keepCharge marks the request as having reached upstream (keep=true), or
// as refused before that (keep=false).
func keepCharge(ctx context.Context, keep bool) {
	if c, ok := ctx.Value(keyChargeKey{}).(*keyCharge); ok {
		c.keep = keep
	}
}

// keyModelCheck refuses, with 403, a model outside the allow list of the
// request's API key; names are the ones the model goes by (as requested,
// matched to the catalog, resolved upstream). An empty requested model
// means the "devin" alias. A zero status means the request may proceed.
func (s *Server) keyModelCheck(ctx context.Context, requested string, names ...string) (int, string) {
	if requested == "" {
		requested = "devin"
	}
	if modelAllowed(ctx, append(names, requested)...) {
		return 0, ""
	}
	return http.StatusForbidden, fmt.Sprintf("this API key is not allowed to use model %q", requested)
}

// counted reports whether a /v1 request uses the API key's request quota:
// generation requests do, listing models and counting tokens do not.
func counted(r *http.Request) bool {
	return r.Method == http.MethodPost && !strings.HasSuffix(r.URL.Path, "/count_tokens")
}

// --- admin endpoints ---------------------------------------------------------

// tokenView strips the stored hash; the plaintext value is only ever set
// on the record returned by createToken.
func tokenView(t store.Token) store.Token {
	t.Hash = ""
	return t
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.tokens.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "unable to list tokens")
		return
	}
	sort.Slice(toks, func(i, j int) bool { return toks[i].CreatedAt.Before(toks[j].CreatedAt) })
	for i := range toks {
		toks[i] = tokenView(toks[i])
		toks[i].Token = ""
	}
	writeAdminJSON(w, http.StatusOK, toks)
}

// decodeTokenBody reads an optional JSON body into v; an empty body leaves
// v unchanged.
func decodeTokenBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return errEmptyBody
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// errEmptyBody is returned by decodeTokenBody for a blank body: fine for
// create (all defaults), refused for update, which replaces every option.
var errEmptyBody = errors.New("empty body")

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var opts store.TokenOptions
	if err := decodeTokenBody(r, &opts); err != nil && !errors.Is(err, errEmptyBody) {
		writeErr(w, http.StatusBadRequest, "invalid API key JSON: "+err.Error())
		return
	}
	tok, err := s.tokens.Create(opts)
	if err != nil {
		writeTokenErr(w, err)
		return
	}
	writeAdminJSON(w, http.StatusCreated, tokenView(tok))
}

func (s *Server) updateToken(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		store.TokenOptions
		ResetRequests bool `json:"reset_requests"`
	}
	if err := decodeTokenBody(r, &body); err != nil {
		if errors.Is(err, errEmptyBody) {
			// PUT replaces every option, so a blank body would clear them.
			writeErr(w, http.StatusBadRequest, "PUT needs the key's full options (name, disabled, expires_at, models, quota)")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid API key JSON: "+err.Error())
		return
	}
	tok, err := s.tokens.Update(id, body.TokenOptions, body.ResetRequests)
	if err != nil {
		writeTokenErr(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, tokenView(tok))
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.tokens.Delete(id); err != nil {
		writeTokenErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeTokenErr maps token store errors: unknown id, failed save, or else
// invalid options.
func writeTokenErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeErr(w, http.StatusNotFound, "token not found")
	case strings.HasPrefix(err.Error(), "save "):
		writeErr(w, http.StatusInternalServerError, "unable to save API keys")
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

// handleAdminModels serves GET /admin/api/models: the model catalog, for
// picking the models an API key may use.
func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	type model struct {
		ID    string `json:"id"`
		Label string `json:"label,omitempty"`
	}
	out := []model{}
	for _, m := range s.fetchModels(r.Context()) {
		out = append(out, model{ID: m.UID, Label: m.Label})
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"default": devin.DefaultModel, "models": out})
}
