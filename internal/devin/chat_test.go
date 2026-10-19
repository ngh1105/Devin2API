package devin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// frame builds one Connect response frame.
func frame(flag byte, payload []byte) []byte {
	out := make([]byte, 5, 5+len(payload))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// chatServer replies with the given frames and records the decoded request.
func chatServer(t *testing.T, frames ...[]byte) (*httptest.Server, *[]byte, *http.Header) {
	t.Helper()
	var gotBody []byte
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotHdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		if len(raw) >= 5 {
			gotBody = raw[5:]
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		for _, f := range frames {
			w.Write(f)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody, &gotHdr
}

func TestStreamChatHappyPath(t *testing.T) {
	usage := PwFieldBytes(7, append(append(pwFieldVarint(2, 472), pwFieldVarint(3, 40)...), pwFieldString(9, "swe-2-high")...))
	srv, body, hdr := chatServer(t,
		frame(0, pwFieldString(9, "thinking...")),
		frame(0, pwFieldString(3, "po")),
		frame(flagCompressed, gz(append(pwFieldString(3, "ng"), usage...))),
		frame(0, pwFieldVarint(5, 2)),
		frame(flagEndStream, []byte("{}")),
	)

	var text, thinking strings.Builder
	res, err := StreamChat(context.Background(), srv.URL, "devin-session-token$tok", ChatRequest{
		System:  "be brief",
		Prompts: []Prompt{{Source: SourceUser, Text: "ping"}},
	}, func(t, th string) error {
		text.WriteString(t)
		thinking.WriteString(th)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "pong" || thinking.String() != "thinking..." {
		t.Errorf("text=%q thinking=%q", text.String(), thinking.String())
	}
	if res.StopReason != 2 || res.PromptTokens != 472 || res.CompletionTokens != 40 || res.Model != "swe-2-high" {
		t.Errorf("result = %+v", res)
	}

	if got := hdr.Get("Authorization"); got != "Basic devin-session-token$tok-devin-session-token$tok" {
		t.Errorf("Authorization = %q", got)
	}
	if hdr.Get("Content-Type") != "application/connect+proto" || hdr.Get("Connect-Protocol-Version") != "1" {
		t.Errorf("headers = %v", *hdr)
	}

	// Decode the request we sent and check the fields that matter.
	fields := map[int][][]byte{}
	r := &pwReader{data: *body}
	for {
		f, wire, val, v, ok := r.Next()
		if !ok {
			break
		}
		if wire == wireVarint {
			val = PwVarint(v)
		}
		fields[f] = append(fields[f], val)
	}
	if string(fields[2][0]) != "be brief" {
		t.Errorf("system = %q", fields[2][0])
	}
	if string(fields[21][0]) != DefaultModel {
		t.Errorf("model = %q", fields[21][0])
	}
	if !bytes.Contains(fields[3][0], []byte("ping")) {
		t.Error("prompt text missing")
	}
	if !bytes.Contains(fields[1][0], []byte("devin-session-token$tok")) {
		t.Error("client metadata missing session token")
	}
}

func TestStreamChatTrailerErrors(t *testing.T) {
	cases := []struct {
		trailer string
		status  int
	}{
		{`{"error":{"code":"unauthenticated","message":"bad token"}}`, 401},
		{`{"error":{"code":"resource_exhausted","message":"slow down"}}`, 429},
		{`{"error":{"code":"failed_precondition","message":"daily quota exhausted"}}`, 429},
		{`{"error":{"code":"permission_denied","message":"nope"}}`, 403},
		{`{"error":{"code":"unavailable","message":"down"}}`, 503},
	}
	for _, tc := range cases {
		srv, _, _ := chatServer(t, frame(flagEndStream, []byte(tc.trailer)))
		_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{SourceUser, "hi"}}},
			func(string, string) error { return nil })
		var ue *UpstreamError
		if !errors.As(err, &ue) || ue.Status != tc.status {
			t.Errorf("%s: got %v, want status %d", tc.trailer, err, tc.status)
		}
	}
}

func TestStreamChatHTTP401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{SourceUser, "hi"}}},
		func(string, string) error { return nil })
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusUnauthorized {
		t.Fatalf("got %v", err)
	}
}

func TestStreamChatMissingTrailer(t *testing.T) {
	srv, _, _ := chatServer(t, frame(0, pwFieldString(3, "partial")))
	_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{SourceUser, "hi"}}},
		func(string, string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "end-of-stream") {
		t.Fatalf("got %v", err)
	}
}

func TestStreamChatCallbackAbort(t *testing.T) {
	srv, _, _ := chatServer(t, frame(0, pwFieldString(3, "a")), frame(flagEndStream, []byte("{}")))
	stop := errors.New("client gone")
	_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{SourceUser, "hi"}}},
		func(string, string) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("got %v", err)
	}
}

func TestUTF8BufferSplitsCleanly(t *testing.T) {
	s := "xin chào 👋"
	raw := []byte(s)
	var b utf8Buffer
	var out strings.Builder
	// Feed one byte at a time: every emitted chunk must be valid UTF-8.
	for i := range raw {
		chunk := b.feed(raw[i : i+1])
		if !utf8.ValidString(chunk) {
			t.Fatalf("invalid chunk %q", chunk)
		}
		out.WriteString(chunk)
	}
	out.WriteString(b.flush())
	if out.String() != s {
		t.Errorf("got %q", out.String())
	}
}

func TestResolveModel(t *testing.T) {
	for in, want := range map[string]string{
		"":                  DefaultModel,
		"devin":             DefaultModel,
		"devin/swe-2-high":  "swe-2-high",
		"claude-sonnet-4.6": "claude-sonnet-4-6",
		"gpt-5-4":           "gpt-5-4",
	} {
		if got := ResolveModel(in); got != want {
			t.Errorf("ResolveModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadFrameRejectsBadFlag(t *testing.T) {
	if _, _, err := readFrame(bytes.NewReader(frame(0x08, nil))); err == nil {
		t.Error("expected error for unknown flag")
	}
}
