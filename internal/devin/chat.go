package devin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// GetChatMessage is a Connect server-streaming RPC: the request is one
// enveloped protobuf message, the response a sequence of enveloped frames
// ending with a JSON end-of-stream trailer.
const chatPath = "/exa.api_server_pb.ApiServerService/GetChatMessage"

// DefaultModel is used when a request names no model.
const DefaultModel = "swe-2-high"

const (
	flagCompressed = 0x01
	flagEndStream  = 0x02

	maxFrameBytes    = 16 << 20
	maxInflatedBytes = 64 << 20
)

// PromptSource is the speaker of a prompt in GetChatMessageRequest.
type PromptSource uint64

const (
	SourceUser       PromptSource = 1
	SourceAssistant  PromptSource = 2
	SourceToolResult PromptSource = 4
)

// StopToolCalls is the stop reason upstream sends when the model ends its
// turn to call tools.
const StopToolCalls = 10

// maxToolCalls caps tool calls accepted from one response.
const maxToolCalls = 128

// ToolCall is a function call made by the assistant.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON text
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	Parameters  []byte // JSON schema
}

// Prompt is one conversation turn. Assistant turns may carry ToolCalls;
// tool results use SourceToolResult with the ToolCallID they answer.
// User turns may carry inline Images.
type Prompt struct {
	Source     PromptSource
	Text       string
	ToolCalls  []ToolCall
	ToolCallID string
	Images     []Image
}

// Image is an inline image sent with a prompt.
type Image struct {
	MimeType string // e.g. "image/png"
	Base64   string // standard base64
}

// ChatRequest is a chat call.
type ChatRequest struct {
	Model       string // friendly name or upstream UID; empty = DefaultModel
	System      string
	Prompts     []Prompt
	Tools       []Tool
	MaxTokens   int
	Temperature float64
	TopP        float64 // 0 = upstream default (0.95)
	// Stop sequences are not sent upstream; the proxy cuts the output at
	// the first match (see server.stopWatcher).
	Stop []string
}

// Delta is one streamed piece of output: answer text, thinking text, or a
// fragment of a tool call.
type Delta struct {
	Text     string
	Thinking string
	ToolCall *ToolCallDelta
}

// ToolCallDelta is a fragment of one tool call. ID and Name are set on the
// first fragment of a call; Arguments carries the next piece of its JSON.
type ToolCallDelta struct {
	Index     int // position of the call within this response
	ID        string
	Name      string
	Arguments string
}

// ChatResult summarises a finished stream.
type ChatResult struct {
	StopReason       uint64
	PromptTokens     int
	CompletionTokens int
	Model            string     // model name reported upstream
	ToolCalls        []ToolCall // complete tool calls, in order
	StopSequence     string     // client stop sequence that ended the output, if any
}

// UpstreamError is a failure reported by Devin, mapped to an HTTP status.
type UpstreamError struct {
	Status  int
	Code    string
	Message string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("devin: %s (%d): %s", e.Code, e.Status, e.Message)
}

// chatHTTPClient has no overall timeout: streams can run for minutes and are
// bounded by the request context instead.
var chatHTTPClient = &http.Client{}

// ResolveModel maps a client-facing model name to the upstream model UID.
// "devin" and "" select DefaultModel; a "devin/" prefix is dropped and dots
// become dashes ("claude-sonnet-4.6" -> "claude-sonnet-4-6").
func ResolveModel(name string) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "devin/")
	if name == "" || name == "devin" {
		return DefaultModel
	}
	return strings.ReplaceAll(name, ".", "-")
}

// BuildChatRequest encodes a GetChatMessageRequest. Field layout:
//
//	1  ClientMetadata
//	2  system prompt (omitted when empty)
//	3  repeated Prompt {1 id, 2 source, 3 text, 6 repeated ToolCall {1 id, 2 name, 3 args}, 7 tool call id,
//	   10 repeated Image {1 base64, 2 mime type}}
//	7  5 (constant)
//	8  config {1:1, 2 max tokens, 3:400, 5 temperature (double), 7:40, 8 top_p 0.95 (double)}
//	10 repeated Tool {1 name, 2 description, 3 JSON schema}
//	15 session {1 session id, 3:4, 4:14 when the last prompt is from the user}
//	16 cascade id (= session id)
//	20 1 (constant)
//	21 model UID
func BuildChatRequest(sessionToken, fingerprint, sessionID string, req ChatRequest) []byte {
	out := PwFieldBytes(1, clientMetadata(sessionToken, fingerprint))
	if system := SanitizeSystemPrompt(req.System); system != "" {
		out = append(out, pwFieldString(2, system)...)
	}
	for _, p := range req.Prompts {
		var m []byte
		m = append(m, pwFieldString(1, newUUID())...)
		m = append(m, pwFieldVarint(2, uint64(p.Source))...)
		m = append(m, pwFieldString(3, imageHeader(p.Images)+p.Text)...)
		for _, tc := range p.ToolCalls {
			var c []byte
			if tc.ID != "" {
				c = append(c, pwFieldString(1, tc.ID)...)
			}
			if tc.Name != "" {
				c = append(c, pwFieldString(2, tc.Name)...)
			}
			if tc.Arguments != "" {
				c = append(c, pwFieldString(3, tc.Arguments)...)
			}
			m = append(m, PwFieldBytes(6, c)...)
		}
		if p.ToolCallID != "" {
			m = append(m, pwFieldString(7, p.ToolCallID)...)
		}
		for _, img := range p.Images {
			ib := pwFieldString(1, img.Base64)
			ib = append(ib, pwFieldString(2, img.MimeType)...)
			m = append(m, PwFieldBytes(10, ib)...)
		}
		out = append(out, PwFieldBytes(3, m)...)
	}
	out = append(out, pwFieldVarint(7, 5)...)

	var cfg []byte
	cfg = append(cfg, pwFieldVarint(1, 1)...)
	cfg = append(cfg, pwFieldVarint(2, uint64(req.MaxTokens))...)
	cfg = append(cfg, pwFieldVarint(3, 400)...)
	cfg = append(cfg, PwFieldDouble(5, req.Temperature)...)
	topP := req.TopP
	if topP <= 0 || topP > 1 {
		topP = float64(float32(0.95))
	}
	cfg = append(cfg, pwFieldVarint(7, 40)...)
	cfg = append(cfg, PwFieldDouble(8, topP)...)
	out = append(out, PwFieldBytes(8, cfg)...)

	for _, t := range req.Tools {
		if t.Name == "" {
			continue
		}
		tb := pwFieldString(1, t.Name)
		if t.Description != "" {
			tb = append(tb, pwFieldString(2, t.Description)...)
		}
		params := t.Parameters
		if len(params) == 0 {
			params = []byte(`{"type":"object","properties":{}}`)
		}
		tb = append(tb, PwFieldBytes(3, params)...)
		out = append(out, PwFieldBytes(10, tb)...)
	}

	sess := pwFieldString(1, sessionID)
	sess = append(sess, pwFieldVarint(3, 4)...)
	if n := len(req.Prompts); n > 0 && req.Prompts[n-1].Source == SourceUser {
		sess = append(sess, pwFieldVarint(4, 14)...)
	}
	out = append(out, PwFieldBytes(15, sess)...)
	out = append(out, pwFieldString(16, sessionID)...)
	out = append(out, pwFieldVarint(20, 1)...)
	out = append(out, pwFieldString(21, ResolveModel(req.Model))...)
	return out
}

// imageHeader returns the "[Image N: pasted_image_N.ext]" lines the Devin CLI
// puts before a prompt's text so the model can refer to attached images.
func imageHeader(images []Image) string {
	if len(images) == 0 {
		return ""
	}
	var b strings.Builder
	for i, img := range images {
		ext := strings.TrimPrefix(img.MimeType, "image/")
		if ext == "jpeg" {
			ext = "jpg"
		}
		fmt.Fprintf(&b, "[Image %d: pasted_image_%d.%s]\n", i+1, i+1, ext)
	}
	b.WriteString("\n")
	return b.String()
}

// StreamChat runs one GetChatMessage call, invoking onDelta for each piece
// of answer text, thinking text or tool call as it arrives. Deltas never
// split a UTF-8 character. A non-nil error from onDelta aborts the stream.
func StreamChat(ctx context.Context, serverBase, sessionToken string, req ChatRequest, onDelta func(Delta) error) (*ChatResult, error) {
	if serverBase == "" {
		serverBase = ServerBaseURL
	}
	sessionID := newUUID()
	payload := BuildChatRequest(sessionToken, GenerateFingerprint("devin2api-cli"), sessionID, req)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, serverBase+chatPath, bytes.NewReader(envelope(payload)))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	// The chat API expects the token twice, dash-joined, as a raw Basic value.
	httpReq.Header.Set("Authorization", "Basic "+sessionToken+"-"+sessionToken)
	httpReq.Header.Set("Content-Type", "application/connect+proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Sentry-Trace", randHex(16)+"-"+randHex(8)+"-1")
	httpReq.Header["User-Agent"] = []string{""} // suppress Go's default

	resp, err := chatHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpStatusError(resp)
	}

	res := &ChatResult{}
	var text, thinking utf8Buffer
	tools := newToolTracker()
	defer func() { res.ToolCalls = tools.calls }()
	for {
		flag, frame, err := readFrame(resp.Body)
		if err == io.EOF {
			return res, errors.New("devin: stream ended without end-of-stream frame")
		}
		if err != nil {
			return res, fmt.Errorf("devin: read frame: %w", err)
		}
		if flag&flagEndStream != 0 {
			if err := trailerError(frame); err != nil {
				return res, err
			}
			break
		}
		t, th, calls := parseChatFrame(frame, res)
		if err := emit(onDelta, Delta{Text: text.feed(t), Thinking: thinking.feed(th)}); err != nil {
			return res, err
		}
		for _, raw := range calls {
			if d := tools.feed(raw); d != nil {
				if err := onDelta(Delta{ToolCall: d}); err != nil {
					return res, err
				}
			}
		}
	}
	if err := emit(onDelta, Delta{Text: text.flush(), Thinking: thinking.flush()}); err != nil {
		return res, err
	}
	for _, d := range tools.flush() {
		if err := onDelta(Delta{ToolCall: d}); err != nil {
			return res, err
		}
	}
	return res, nil
}

func emit(onDelta func(Delta) error, d Delta) error {
	if d.Text == "" && d.Thinking == "" {
		return nil
	}
	return onDelta(d)
}

// rawToolDelta is response field 6 as sent upstream:
// {1 id, 2 name, 3 arguments, 4 arguments when they are not valid JSON}.
type rawToolDelta struct {
	id, name, args, invalidArgs string
}

// parseChatFrame extracts the text (field 3), thinking (field 9) and tool
// call (field 6) deltas of one response frame and records stop reason (5)
// and usage (7) into res.
func parseChatFrame(frame []byte, res *ChatResult) (text, thinking []byte, calls []rawToolDelta) {
	r := &pwReader{data: frame}
	for {
		field, wire, val, v, ok := r.Next()
		if !ok {
			return text, thinking, calls
		}
		switch {
		case field == 3 && wire == wireBytes:
			text = append(text, val...)
		case field == 9 && wire == wireBytes:
			thinking = append(thinking, val...)
		case field == 5 && wire == wireVarint && v != 0:
			res.StopReason = v
		case field == 6 && wire == wireBytes:
			calls = append(calls, parseToolDelta(val))
		case field == 7 && wire == wireBytes:
			parseChatUsage(val, res)
		}
	}
}

func parseToolDelta(data []byte) rawToolDelta {
	var d rawToolDelta
	r := &pwReader{data: data}
	for {
		field, wire, val, _, ok := r.Next()
		if !ok {
			return d
		}
		if wire != wireBytes {
			continue
		}
		switch field {
		case 1:
			d.id = string(val)
		case 2:
			d.name = string(val)
		case 3:
			d.args = string(val)
		case 4:
			d.invalidArgs = string(val)
		}
	}
}

// toolTracker assembles streamed tool call fragments into calls. A fragment
// names its call by upstream id; fragments without an id continue the most
// recent call. Calls without an upstream id get a generated one, because
// OpenAI clients need an id to answer the call.
type toolTracker struct {
	calls  []ToolCall
	upIDs  []string       // upstream id per call ("" until seen)
	byUpID map[string]int // upstream id -> call index
	bufs   []utf8Buffer   // per-call argument buffer
	active int            // index of the call that id-less fragments extend
}

func newToolTracker() *toolTracker {
	return &toolTracker{byUpID: map[string]int{}, active: -1}
}

// feed records one fragment and returns what to forward downstream (nil
// when the fragment adds nothing or the call limit is reached).
func (t *toolTracker) feed(raw rawToolDelta) *ToolCallDelta {
	idx := -1
	if raw.id != "" {
		if i, ok := t.byUpID[raw.id]; ok {
			idx = i
		}
	}
	if idx < 0 && t.active >= 0 && t.continues(raw) {
		idx = t.active
		if raw.id != "" {
			// The active call started without an id; this is its id.
			t.upIDs[idx] = raw.id
			t.byUpID[raw.id] = idx
		}
	}

	out := &ToolCallDelta{}
	if idx < 0 {
		if len(t.calls) >= maxToolCalls {
			return nil
		}
		idx = len(t.calls)
		id := raw.id
		if id == "" {
			id = "call_" + randHex(12)
		}
		t.calls = append(t.calls, ToolCall{ID: id, Name: raw.name})
		t.upIDs = append(t.upIDs, raw.id)
		t.bufs = append(t.bufs, utf8Buffer{})
		if raw.id != "" {
			t.byUpID[raw.id] = idx
		}
		out.ID, out.Name = id, raw.name
	} else if t.calls[idx].Name == "" && raw.name != "" {
		t.calls[idx].Name = raw.name
		out.Name = raw.name
	}
	t.active = idx
	out.Index = idx

	args := raw.args
	if args == "" {
		args = raw.invalidArgs
	}
	out.Arguments = t.bufs[idx].feed([]byte(args))
	t.calls[idx].Arguments += out.Arguments
	if out.ID == "" && out.Name == "" && out.Arguments == "" {
		return nil
	}
	return out
}

// continues reports whether a fragment that matches no known upstream id
// extends the active call. It starts a new call instead when it carries a
// different upstream id, names a different function, or repeats the name
// after the active call's arguments already form complete JSON (the same
// tool called twice).
func (t *toolTracker) continues(raw rawToolDelta) bool {
	if raw.id != "" && t.upIDs[t.active] != "" {
		return false
	}
	cur := t.calls[t.active]
	if raw.name != "" && cur.Name != "" && (raw.name != cur.Name || json.Valid([]byte(cur.Arguments))) {
		return false
	}
	return true
}

// flush emits argument bytes still held back by the UTF-8 buffers.
func (t *toolTracker) flush() []*ToolCallDelta {
	var out []*ToolCallDelta
	for i := range t.bufs {
		if rest := t.bufs[i].flush(); rest != "" {
			t.calls[i].Arguments += rest
			out = append(out, &ToolCallDelta{Index: i, Arguments: rest})
		}
	}
	return out
}

// parseChatUsage reads usage {2 prompt tokens, 3 completion tokens, 9 model}.
// Upstream repeats the usage block on many frames; the last non-zero wins.
func parseChatUsage(data []byte, res *ChatResult) {
	r := &pwReader{data: data}
	for {
		field, wire, val, v, ok := r.Next()
		if !ok {
			return
		}
		switch {
		case field == 2 && wire == wireVarint && v != 0:
			res.PromptTokens = int(v)
		case field == 3 && wire == wireVarint && v != 0:
			res.CompletionTokens = int(v)
		case field == 9 && wire == wireBytes:
			res.Model = string(val)
		}
	}
}

// envelope wraps a protobuf message in a Connect frame: flag byte 0
// (uncompressed data), 4-byte big-endian length, payload.
func envelope(payload []byte) []byte {
	out := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}

// readFrame reads one Connect frame, inflating it when the compressed flag
// is set. Returns io.EOF only at a clean frame boundary.
func readFrame(r io.Reader) (flag byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return 0, nil, errors.New("truncated frame header")
		}
		return 0, nil, err
	}
	flag = hdr[0]
	if flag&^(flagCompressed|flagEndStream) != 0 {
		return 0, nil, fmt.Errorf("invalid frame flag 0x%02x", flag)
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrameBytes {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds limit", n)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("truncated frame payload: %w", err)
	}
	if flag&flagCompressed != 0 {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return 0, nil, fmt.Errorf("gzip frame: %w", err)
		}
		payload, err = io.ReadAll(io.LimitReader(zr, maxInflatedBytes+1))
		if err != nil {
			return 0, nil, fmt.Errorf("gzip frame: %w", err)
		}
		if len(payload) > maxInflatedBytes {
			return 0, nil, errors.New("inflated frame exceeds limit")
		}
	}
	return flag, payload, nil
}

// trailerError decodes the end-of-stream JSON. "{}" (or empty) is success;
// {"error":{"code","message"}} becomes an *UpstreamError.
func trailerError(payload []byte) error {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	var t struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &t); err != nil {
		return fmt.Errorf("devin: bad end-of-stream trailer: %w", err)
	}
	if t.Error == nil {
		return nil
	}
	return &UpstreamError{Status: connectStatus(t.Error.Code, t.Error.Message), Code: t.Error.Code, Message: t.Error.Message}
}

// connectStatus maps a Connect error code to the HTTP status a client of
// this proxy should see. Quota and capacity errors surface as 429.
func connectStatus(code, msg string) int {
	lower := strings.ToLower(msg)
	switch code {
	case "unauthenticated":
		return http.StatusUnauthorized
	case "resource_exhausted":
		return http.StatusTooManyRequests
	case "permission_denied":
		if strings.Contains(lower, "high demand") {
			return http.StatusTooManyRequests
		}
		return http.StatusForbidden
	case "failed_precondition":
		for _, kw := range []string{"quota", "credit", "acu", "exhausted", "limit"} {
			if strings.Contains(lower, kw) {
				return http.StatusTooManyRequests
			}
		}
		return http.StatusBadRequest
	case "invalid_argument":
		if strings.Contains(lower, "internal error") {
			return http.StatusBadGateway
		}
		return http.StatusBadRequest
	case "unavailable":
		return http.StatusServiceUnavailable
	case "deadline_exceeded":
		return http.StatusGatewayTimeout
	case "canceled":
		return 499
	default:
		return http.StatusBadGateway
	}
}

// httpStatusError turns a non-200 response (sent before any frame) into an
// *UpstreamError, using a Connect JSON error body when present.
func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Code != "" {
		return &UpstreamError{Status: connectStatus(e.Code, e.Message), Code: e.Code, Message: e.Message}
	}
	status := resp.StatusCode
	if status == http.StatusUnauthorized {
		return &UpstreamError{Status: status, Code: "unauthenticated", Message: "session token expired or revoked"}
	}
	if status != http.StatusTooManyRequests && status < 500 {
		status = http.StatusBadGateway
	}
	return &UpstreamError{Status: status, Code: "http_" + fmt.Sprint(resp.StatusCode), Message: strings.TrimSpace(string(body))}
}

// utf8Buffer holds back a trailing incomplete UTF-8 sequence so a delta
// never ends in the middle of a multi-byte character.
type utf8Buffer struct{ pending []byte }

func (b *utf8Buffer) feed(chunk []byte) string {
	if len(chunk) == 0 {
		return ""
	}
	data := append(b.pending, chunk...)
	cut := len(data)
	// A UTF-8 sequence is at most 4 bytes: look back up to 3 for its start.
	for i := len(data) - 1; i >= 0 && i >= len(data)-3; i-- {
		c := data[i]
		if c < 0x80 {
			break // ASCII: everything is complete
		}
		if utf8.RuneStart(c) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	b.pending = append([]byte(nil), data[cut:]...)
	return string(data[:cut])
}

// flush returns whatever is left, even if it is an incomplete sequence.
func (b *utf8Buffer) flush() string {
	s := string(b.pending)
	b.pending = nil
	return s
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("devin: crypto/rand failed: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("devin: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
