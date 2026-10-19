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
	}, func(d Delta) error {
		text.WriteString(d.Text)
		thinking.WriteString(d.Thinking)
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
		_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{Source: SourceUser, Text: "hi"}}},
			func(Delta) error { return nil })
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
	_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{Source: SourceUser, Text: "hi"}}},
		func(Delta) error { return nil })
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusUnauthorized {
		t.Fatalf("got %v", err)
	}
}

func TestStreamChatMissingTrailer(t *testing.T) {
	srv, _, _ := chatServer(t, frame(0, pwFieldString(3, "partial")))
	_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{Source: SourceUser, Text: "hi"}}},
		func(Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "end-of-stream") {
		t.Fatalf("got %v", err)
	}
}

func TestStreamChatCallbackAbort(t *testing.T) {
	srv, _, _ := chatServer(t, frame(0, pwFieldString(3, "a")), frame(flagEndStream, []byte("{}")))
	stop := errors.New("client gone")
	_, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{Prompts: []Prompt{{Source: SourceUser, Text: "hi"}}},
		func(Delta) error { return stop })
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

// toolFrame encodes a response frame holding one tool call fragment.
func toolFrame(id, name, args string) []byte {
	var b []byte
	if id != "" {
		b = append(b, pwFieldString(1, id)...)
	}
	if name != "" {
		b = append(b, pwFieldString(2, name)...)
	}
	if args != "" {
		b = append(b, pwFieldString(3, args)...)
	}
	return frame(0, PwFieldBytes(6, b))
}

func TestStreamChatToolCalls(t *testing.T) {
	srv, body, _ := chatServer(t,
		toolFrame("up_1", "get_weather", `{"city":`),
		toolFrame("", "", `"Hà Nội"}`), // id-less fragment continues the active call
		toolFrame("up_2", "get_time", `{}`),
		frame(0, pwFieldVarint(5, StopToolCalls)),
		frame(flagEndStream, []byte("{}")),
	)
	var deltas []ToolCallDelta
	res, err := StreamChat(context.Background(), srv.URL, "tok", ChatRequest{
		Prompts: []Prompt{{Source: SourceUser, Text: "weather?"}},
		Tools:   []Tool{{Name: "get_weather", Description: "Weather", Parameters: []byte(`{"type":"object"}`)}, {Name: "get_time"}},
	}, func(d Delta) error {
		if d.ToolCall != nil {
			deltas = append(deltas, *d.ToolCall)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 2 || res.StopReason != StopToolCalls {
		t.Fatalf("result = %+v", res)
	}
	if c := res.ToolCalls[0]; c.ID != "up_1" || c.Name != "get_weather" || c.Arguments != `{"city":"Hà Nội"}` {
		t.Errorf("call 0 = %+v", c)
	}
	if c := res.ToolCalls[1]; c.ID != "up_2" || c.Name != "get_time" || c.Arguments != `{}` {
		t.Errorf("call 1 = %+v", c)
	}
	// First fragment per call carries id and name; continuation only args.
	if deltas[0].ID != "up_1" || deltas[0].Name != "get_weather" || deltas[1].ID != "" || deltas[1].Index != 0 || deltas[2].Index != 1 {
		t.Errorf("deltas = %+v", deltas)
	}

	// Request carries both tool definitions (field 10) with their schema.
	var tools [][]byte
	r := &pwReader{data: *body}
	for {
		f, wire, val, _, ok := r.Next()
		if !ok {
			break
		}
		if f == 10 && wire == wireBytes {
			tools = append(tools, val)
		}
	}
	if len(tools) != 2 || !bytes.Contains(tools[0], []byte(`{"type":"object"}`)) || !bytes.Contains(tools[1], []byte("get_time")) {
		t.Errorf("tools in request = %q", tools)
	}
}

func TestToolTrackerGeneratesIDAndAdoptsLateID(t *testing.T) {
	tr := newToolTracker()
	d := tr.feed(rawToolDelta{name: "f", args: `{"a":`})
	if d == nil || d.ID == "" || d.Name != "f" {
		t.Fatalf("first delta = %+v", d)
	}
	// Upstream id arriving later is adopted by the active call, not a new one.
	tr.feed(rawToolDelta{id: "late", args: `1}`})
	if len(tr.calls) != 1 || tr.calls[0].Arguments != `{"a":1}` {
		t.Fatalf("calls = %+v", tr.calls)
	}
	// Invalid-JSON arguments (field 4) are still forwarded.
	tr.feed(rawToolDelta{id: "x", name: "g", invalidArgs: `{bad`})
	if len(tr.calls) != 2 || tr.calls[1].Arguments != `{bad` {
		t.Errorf("calls = %+v", tr.calls)
	}
}

func TestBuildChatRequestToolHistory(t *testing.T) {
	b := BuildChatRequest("tok", "fp", "sess", ChatRequest{Prompts: []Prompt{
		{Source: SourceUser, Text: "weather?"},
		{Source: SourceAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Hue"}`}}},
		{Source: SourceToolResult, Text: "sunny", ToolCallID: "call_1"},
	}})
	var prompts [][]byte
	r := &pwReader{data: b}
	for {
		f, wire, val, _, ok := r.Next()
		if !ok {
			break
		}
		if f == 3 && wire == wireBytes {
			prompts = append(prompts, val)
		}
	}
	if len(prompts) != 3 {
		t.Fatalf("got %d prompts", len(prompts))
	}
	if !bytes.Contains(prompts[1], []byte("get_weather")) || !bytes.Contains(prompts[1], []byte(`{"city":"Hue"}`)) {
		t.Error("assistant tool call missing")
	}
	// Tool result: source 4 (tag 0x10 value 0x04) and tool_call_id in field 7 (tag 0x3a).
	if !bytes.Contains(prompts[2], []byte{0x10, 0x04}) || !bytes.Contains(prompts[2], append([]byte{0x3a, 6}, "call_1"...)) {
		t.Errorf("tool result prompt = %x", prompts[2])
	}
}

// TestToolTrackerSplitsIDLessCalls: two calls that arrive without upstream
// ids must stay separate, both for different functions and for the same
// function called twice.
func TestToolTrackerSplitsIDLessCalls(t *testing.T) {
	tr := newToolTracker()
	tr.feed(rawToolDelta{name: "read", args: `{"path":"a"}`})
	tr.feed(rawToolDelta{name: "write", args: `{"path":"b"}`})
	tr.feed(rawToolDelta{name: "write", args: `{"path":`})
	tr.feed(rawToolDelta{args: `"c"}`})
	if len(tr.calls) != 3 {
		t.Fatalf("got %d calls: %+v", len(tr.calls), tr.calls)
	}
	want := []string{`{"path":"a"}`, `{"path":"b"}`, `{"path":"c"}`}
	for i, w := range want {
		if tr.calls[i].Arguments != w {
			t.Errorf("call %d args = %q, want %q", i, tr.calls[i].Arguments, w)
		}
	}
}

// TestToolTrackerNewIDStartsNewCall: a fragment with an unseen upstream id
// never extends a call that already has a different id.
func TestToolTrackerNewIDStartsNewCall(t *testing.T) {
	tr := newToolTracker()
	tr.feed(rawToolDelta{id: "a", name: "f", args: `{"x":`})
	tr.feed(rawToolDelta{id: "b", args: `{}`})
	if len(tr.calls) != 2 || tr.calls[0].Arguments != `{"x":` || tr.calls[1].Arguments != `{}` {
		t.Errorf("calls = %+v", tr.calls)
	}
}

func TestBuildChatRequestImages(t *testing.T) {
	b := BuildChatRequest("tok", "fp", "sess", ChatRequest{Prompts: []Prompt{{
		Source: SourceUser, Text: "what is this?",
		Images: []Image{{MimeType: "image/jpeg", Base64: "QUJD"}},
	}}})
	r := &pwReader{data: b}
	var prompt []byte
	for {
		f, wire, val, _, ok := r.Next()
		if !ok {
			break
		}
		if f == 3 && wire == wireBytes {
			prompt = val
		}
	}
	if !bytes.Contains(prompt, []byte("[Image 1: pasted_image_1.jpg]\n\nwhat is this?")) {
		t.Errorf("image header missing: %q", prompt)
	}
	img := PwFieldBytes(10, append(pwFieldString(1, "QUJD"), pwFieldString(2, "image/jpeg")...))
	if !bytes.Contains(prompt, img) {
		t.Error("image field 10 missing")
	}
}

func TestSanitizeSystemPrompt(t *testing.T) {
	in := "x-anthropic-billing-header: cc_version=2.1\r\n" +
		"You are Claude Code, Anthropic's official CLI for Claude.\r\n" +
		"You are an interactive CLI tool.\n" +
		"IMPORTANT: Assist with authorized security testing, defensive security.\n" +
		" - Claude Code is available as a CLI in the terminal.\n" +
		"Keep answers short.\n"
	want := "You are an interactive CLI tool.\nKeep answers short."
	if got := SanitizeSystemPrompt(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if SanitizeSystemPrompt("You are Claude Code, Anthropic's official CLI for Claude.") != "" {
		t.Error("identity-only prompt should become empty")
	}
	// The request omits field 2 when nothing is left.
	b := BuildChatRequest("tok", "fp", "s", ChatRequest{System: "You are Claude Code.", Prompts: []Prompt{{Source: SourceUser, Text: "hi"}}})
	r := &pwReader{data: b}
	for {
		f, _, _, _, ok := r.Next()
		if !ok {
			break
		}
		if f == 2 {
			t.Error("empty sanitized system prompt still sent")
		}
	}
}
