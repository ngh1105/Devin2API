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
	SourceUser      PromptSource = 1
	SourceAssistant PromptSource = 2
)

// Prompt is one conversation turn.
type Prompt struct {
	Source PromptSource
	Text   string
}

// ChatRequest is a text-only chat call.
type ChatRequest struct {
	Model       string // friendly name or upstream UID; empty = DefaultModel
	System      string
	Prompts     []Prompt
	MaxTokens   int
	Temperature float64
}

// ChatResult summarises a finished stream.
type ChatResult struct {
	StopReason       uint64
	PromptTokens     int
	CompletionTokens int
	Model            string // model name reported upstream
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
//	3  repeated Prompt {1 id, 2 source, 3 text}
//	7  5 (constant)
//	8  config {1:1, 2 max tokens, 3:400, 5 temperature (double), 7:40, 8 top_p 0.95 (double)}
//	15 session {1 session id, 3:4, 4:14 when the last prompt is from the user}
//	16 cascade id (= session id)
//	20 1 (constant)
//	21 model UID
func BuildChatRequest(sessionToken, fingerprint, sessionID string, req ChatRequest) []byte {
	out := PwFieldBytes(1, clientMetadata(sessionToken, fingerprint))
	if req.System != "" {
		out = append(out, pwFieldString(2, req.System)...)
	}
	for _, p := range req.Prompts {
		var m []byte
		m = append(m, pwFieldString(1, newUUID())...)
		m = append(m, pwFieldVarint(2, uint64(p.Source))...)
		m = append(m, pwFieldString(3, p.Text)...)
		out = append(out, PwFieldBytes(3, m)...)
	}
	out = append(out, pwFieldVarint(7, 5)...)

	var cfg []byte
	cfg = append(cfg, pwFieldVarint(1, 1)...)
	cfg = append(cfg, pwFieldVarint(2, uint64(req.MaxTokens))...)
	cfg = append(cfg, pwFieldVarint(3, 400)...)
	cfg = append(cfg, PwFieldDouble(5, req.Temperature)...)
	cfg = append(cfg, pwFieldVarint(7, 40)...)
	cfg = append(cfg, PwFieldDouble(8, float64(float32(0.95)))...)
	out = append(out, PwFieldBytes(8, cfg)...)

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

// StreamChat runs one GetChatMessage call, invoking onDelta for each piece
// of answer text or thinking text as it arrives. Deltas never split a UTF-8
// character. A non-nil error from onDelta aborts the stream.
func StreamChat(ctx context.Context, serverBase, sessionToken string, req ChatRequest, onDelta func(text, thinking string) error) (*ChatResult, error) {
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
		t, th := parseChatFrame(frame, res)
		if err := emit(onDelta, text.feed(t), thinking.feed(th)); err != nil {
			return res, err
		}
	}
	if err := emit(onDelta, text.flush(), thinking.flush()); err != nil {
		return res, err
	}
	return res, nil
}

func emit(onDelta func(text, thinking string) error, text, thinking string) error {
	if text == "" && thinking == "" {
		return nil
	}
	return onDelta(text, thinking)
}

// parseChatFrame extracts the text (field 3) and thinking (field 9) deltas of
// one response frame and records stop reason (5) and usage (7) into res.
func parseChatFrame(frame []byte, res *ChatResult) (text, thinking []byte) {
	r := &pwReader{data: frame}
	for {
		field, wire, val, v, ok := r.Next()
		if !ok {
			return text, thinking
		}
		switch {
		case field == 3 && wire == wireBytes:
			text = append(text, val...)
		case field == 9 && wire == wireBytes:
			thinking = append(thinking, val...)
		case field == 5 && wire == wireVarint && v != 0:
			res.StopReason = v
		case field == 7 && wire == wireBytes:
			parseChatUsage(val, res)
		}
	}
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