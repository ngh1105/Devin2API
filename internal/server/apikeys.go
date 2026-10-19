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

// modelNotAllowedError rejects a model outside the API key's allow list.
type modelNotAllowedError struct{ model string }

func (e *modelNotAllowedError) Error() string {
	return fmt.Sprintf("this API key is not allowed to use model %q", e.model)
}

// checkModel returns a modelNotAllowedError when the key behind ctx may
// not use the requested model.
func checkModel(ctx context.Context, requested string) error {
	resolved := devin.ResolveModel(requested)
	if modelAllowed(ctx, requested, resolved) {
		return nil
	}
	if requested == "" {
		requested = resolved
	}
	return &modelNotAllowedError{model: requested}
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
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var opts store.TokenOptions
	if err := decodeTokenBody(r, &opts); err != nil {
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
