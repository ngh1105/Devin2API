package devin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngh1105/Devin2API/internal/store"
)

// TestStreamChatStallTimeout: an upstream that stops sending frames fails
// the call with a 504 UpstreamError instead of hanging.
func TestStreamChatStallTimeout(t *testing.T) {
	old := streamIdleTimeout
	streamIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { streamIdleTimeout = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		w.Write(frame(0, pwFieldString(3, "po")))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never send another frame
	}))
	defer srv.Close()

	start := time.Now()
	var got string
	_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{Source: SourceUser, Text: "hi"}}}, func(d Delta) error {
		got += d.Text
		return nil
	})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusGatewayTimeout {
		t.Fatalf("err = %v, want a 504 UpstreamError", err)
	}
	if got != "po" {
		t.Errorf("text before the stall = %q", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v, the stall timeout did not apply", time.Since(start))
	}
}

func TestResolveModelCaseInsensitive(t *testing.T) {
	for _, in := range []string{"Devin", "DEVIN", "devin/", "Devin/devin", ""} {
		if got := ResolveModel(in); got != DefaultModel {
			t.Errorf("ResolveModel(%q) = %q", in, got)
		}
	}
	if got := ResolveModel("DEVIN/claude-sonnet-4.6"); got != "claude-sonnet-4-6" {
		t.Errorf("prefix: %q", got)
	}
}

// TestFingerprintPerToken: accounts get distinct, stable fingerprints.
func TestFingerprintPerToken(t *testing.T) {
	a, b := Fingerprint("eyJa"), Fingerprint("eyJb")
	if a == b || len(a) != 732 || a != Fingerprint("eyJa") {
		t.Errorf("fingerprints a=%d bytes equal=%v stable=%v", len(a), a == b, a == Fingerprint("eyJa"))
	}
}

// TestReadVarintOverflow: a tenth byte above 1 does not fit in 64 bits.
func TestReadVarintOverflow(t *testing.T) {
	bad := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}
	if _, n := readVarint(bad); n != 0 {
		t.Errorf("overflowing varint accepted, n=%d", n)
	}
	ok := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	if v, n := readVarint(ok); n != 10 || v != ^uint64(0) {
		t.Errorf("max varint = %d, n=%d", v, n)
	}
}

// TestParseUserStatusWireGuard: a field of the wrong wire type is skipped
// instead of read as a zero value, and a truncated message is an error.
func TestParseUserStatusWireGuard(t *testing.T) {
	// plan_start (field 2) sent as bytes instead of a varint.
	plan := pwFieldString(2, "not-a-time")
	plan = append(plan, pwFieldVarint(14, 40)...)
	status := PwFieldBytes(13, plan)
	u, err := ParseGetUserStatusResponse(PwFieldBytes(1, status))
	if err != nil {
		t.Fatal(err)
	}
	if !u.PlanStart.IsZero() || u.DailyRemainingPercent != 40 {
		t.Errorf("usage = %+v", u)
	}
	var zero store.Usage
	_ = zero
	// Truncated top-level field: a length prefix past the end.
	if _, err := ParseGetUserStatusResponse([]byte{0x0a, 0x10, 0x01}); err == nil {
		t.Error("truncated message accepted")
	}
}

func TestBodyExcerpt(t *testing.T) {
	long := strings.Repeat("<html>", 200) + "\x00\x01"
	got := bodyExcerpt([]byte(long))
	if len(got) > maxErrExcerpt+4 || strings.ContainsAny(got, "\x00\x01") {
		t.Errorf("excerpt len %d: %q", len(got), got[:40])
	}
}

// TestLoginWaitNoTimeout: a non-positive timeout leaves ctx to bound the wait.
func TestLoginWaitNoTimeout(t *testing.T) {
	l := &LoginState{resultCh: make(chan loginResult, 1), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := l.Wait(ctx, 0)
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Errorf("err = %v, want a context error", err)
	}
}
