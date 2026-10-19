package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
	"github.com/ngh1105/Devin2API/internal/store"
)

func postV1(srv *Server, path, body string) *httptest.ResponseRecorder {
	req := v1Request(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// sseEvents splits an SSE body into (event name, data) pairs.
func sseEvents(t *testing.T, body string) (names []string, data []map[string]any) {
	t.Helper()
	for _, block := range strings.Split(body, "\n\n") {
		var name, payload string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				payload = v
			}
		}
		if name == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("event %s: bad data %q", name, payload)
		}
		names = append(names, name)
		data = append(data, m)
	}
	return names, data
}

// thinkingUpstream streams thinking, then text, then a tool call.
func thinkingUpstream(t *testing.T) (*Server, *[][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/GetCliModelConfigs") {
			return
		}
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Write(connectFrame(0, devin.PwFieldString(9, "let me think")))
		w.Write(connectFrame(0, devin.PwFieldString(3, "Checking.")))
		c := append(devin.PwFieldString(1, "get_weather:0#ab"), devin.PwFieldString(2, "get_weather")...)
		c = append(c, devin.PwFieldString(3, `{"city":"Hue"}`)...)
		w.Write(connectFrame(0, devin.PwFieldBytes(6, c)))
		usage := devin.PwFieldBytes(7, append(devin.PwFieldVarint(2, 30), devin.PwFieldVarint(3, 5)...))
		w.Write(connectFrame(0, append(devin.PwFieldVarint(5, devin.StopToolCalls), usage...)))
		w.Write(connectFrame(2, []byte("{}")))
	}))
	t.Cleanup(up.Close)
	accs := mustAccounts(t)
	accs.Save(store.AccountFromLogin("u", "usr_th", "org", "eyJtok", "", nil))
	srv := New(config.Config{Listen: ":0"}, accs, mustTokens(t))
	srv.loginServerBase = up.URL
	return srv, &bodies
}

const anthropicTools = `"tools":[{"name":"get_weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}},{"type":"web_search_20250305","name":"web_search"}]`

func TestMessagesNonStream(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postV1(srv, "/v1/messages", `{"model":"devin","max_tokens":100,"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Type != "message" || resp.Role != "assistant" || resp.StopReason != "end_turn" ||
		len(resp.Content) != 1 || resp.Content[0].Text != "pong" || resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 2 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestMessagesStreamThinkingTextTool(t *testing.T) {
	srv, bodies := thinkingUpstream(t)
	rec := postV1(srv, "/v1/messages", `{"model":"devin","max_tokens":100,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"weather?"}],`+anthropicTools+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	names, data := sseEvents(t, rec.Body.String())
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // thinking
		"content_block_start", "content_block_delta", "content_block_stop", // text
		"content_block_start", "content_block_delta", "content_block_stop", // tool_use
		"message_delta", "message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", names)
	}
	blockTypes := []string{}
	for i, n := range names {
		if n == "content_block_start" {
			blockTypes = append(blockTypes, data[i]["content_block"].(map[string]any)["type"].(string))
		}
	}
	if strings.Join(blockTypes, ",") != "thinking,text,tool_use" {
		t.Errorf("blocks = %v", blockTypes)
	}
	tool := data[7]["content_block"].(map[string]any)
	if id := tool["id"].(string); strings.ContainsAny(id, "#:") || tool["name"] != "get_weather" {
		t.Errorf("tool block = %v", tool)
	}
	if d := data[8]["delta"].(map[string]any); d["type"] != "input_json_delta" || d["partial_json"] != `{"city":"Hue"}` {
		t.Errorf("tool delta = %v", d)
	}
	md := data[10]
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" || md["usage"].(map[string]any)["output_tokens"].(float64) != 5 {
		t.Errorf("message_delta = %v", md)
	}
	// Only the client tool goes upstream; the server tool is skipped.
	b := string((*bodies)[0])
	if !strings.Contains(b, "get_weather") || strings.Contains(b, "web_search") || !strings.Contains(b, `"properties"`) {
		t.Error("tools not converted as expected")
	}
}

func TestMessagesThinkingHiddenByDefault(t *testing.T) {
	srv, _ := thinkingUpstream(t)
	rec := postV1(srv, "/v1/messages", `{"model":"devin","max_tokens":100,"messages":[{"role":"user","content":"weather?"}],`+anthropicTools+`}`)
	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Content) != 2 || resp.Content[0].Type != "text" || resp.Content[1].Type != "tool_use" || resp.StopReason != "tool_use" {
		t.Fatalf("resp = %s", rec.Body)
	}
	if string(resp.Content[1].Input) != `{"city":"Hue"}` {
		t.Errorf("tool input = %s", resp.Content[1].Input)
	}
}

// TestMessagesToolHistory: tool_use / tool_result blocks reach upstream with
// the original call id, and errors are marked in the result text.
func TestMessagesToolHistory(t *testing.T) {
	srv, bodies := toolUpstream(t)
	id := encodeToolCallID("get_weather:0#ab")
	rec := postV1(srv, "/v1/messages", `{"model":"devin","max_tokens":100,"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":""},{"type":"tool_use","id":"`+id+`","name":"get_weather","input":{"city":"Hue"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+id+`","content":[{"type":"text","text":"timeout"}],"is_error":true}]}],`+anthropicTools+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	b := string((*bodies)[0])
	if strings.Count(b, "get_weather:0#ab") != 2 || !strings.Contains(b, "Error: timeout") || !strings.Contains(b, `{"city":"Hue"}`) {
		t.Error("tool history not forwarded")
	}
}

func TestMessagesErrorsAreAnthropicShaped(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	for _, body := range []string{
		`{bad`,
		`{"messages":[]}`,
		`{"messages":[{"role":"system","content":"x"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"document"}]}]}`,
	} {
		rec := postV1(srv, "/v1/messages", body)
		var e struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != http.StatusBadRequest || e.Type != "error" || e.Error.Type != "invalid_request_error" {
			t.Errorf("%s: status %d body %s", body, rec.Code, rec.Body)
		}
	}
}

func TestMessagesRateLimit(t *testing.T) {
	limit := `{"error":{"code":"resource_exhausted","message":"Your limit will reset in 2 minutes."}}`
	srv := chatTestServer(t, &fakeDevin{trailers: map[string]string{"eyJrl": limit}}, "eyJrl")
	rec := postV1(srv, "/v1/messages", `{"stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "120" || !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Errorf("status %d Retry-After %q body %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
}

func TestMessagesImage(t *testing.T) {
	srv, bodies := imageServer(t)
	rec := postV1(srv, "/v1/messages", `{"model":"vision-model","max_tokens":50,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="}},
		{"type":"text","text":"what is this?"}]}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(string((*bodies)[0]), "iVBORw0KGgo") {
		t.Errorf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestCountTokens(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postV1(srv, "/v1/messages/count_tokens", `{"model":"devin","messages":[{"role":"user","content":"`+strings.Repeat("a", 400)+`"}]}`)
	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.InputTokens < 90 || resp.InputTokens > 120 {
		t.Errorf("status %d tokens %d", rec.Code, resp.InputTokens)
	}
}

// --- /v1/responses ---

func TestResponsesNonStream(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	rec := postV1(srv, "/v1/responses", `{"model":"devin","instructions":"be brief","input":"ping"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Object     string `json:"object"`
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Object != "response" || resp.Status != "completed" || resp.OutputText != "pong" ||
		len(resp.Output) != 1 || resp.Output[0].Content[0].Type != "output_text" || resp.Usage.TotalTokens != 12 {
		t.Errorf("resp = %s", rec.Body)
	}
}

func TestResponsesStreamToolCall(t *testing.T) {
	srv, bodies := thinkingUpstream(t)
	rec := postV1(srv, "/v1/responses", `{"model":"devin","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"weather?"}]}],
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}},{"type":"web_search"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	names, data := sseEvents(t, rec.Body.String())
	if names[0] != "response.created" || names[1] != "response.in_progress" || names[len(names)-1] != "response.completed" {
		t.Fatalf("events = %v", names)
	}
	for i, d := range data {
		if int(d["sequence_number"].(float64)) != i {
			t.Fatalf("sequence_number %v at %d", d["sequence_number"], i)
		}
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"response.reasoning_summary_text.delta", "response.output_text.delta", "response.function_call_arguments.delta", "response.function_call_arguments.done"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, names)
		}
	}
	final := data[len(data)-1]["response"].(map[string]any)
	output := final["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output = %v", output)
	}
	fc := output[2].(map[string]any)
	if fc["type"] != "function_call" || fc["name"] != "get_weather" || fc["arguments"] != `{"city":"Hue"}` || strings.ContainsAny(fc["call_id"].(string), "#:") {
		t.Errorf("function_call = %v", fc)
	}
	if b := string((*bodies)[0]); strings.Contains(b, "web_search") {
		t.Error("built-in tool sent upstream")
	}
}

// TestResponsesToolHistory: function_call / function_call_output items are
// folded into one assistant turn plus a tool result with the original id.
func TestResponsesToolHistory(t *testing.T) {
	srv, bodies := toolUpstream(t)
	id := encodeToolCallID("get_weather:0#ab")
	rec := postV1(srv, "/v1/responses", `{"model":"devin","input":[
		{"role":"user","content":"weather?"},
		{"type":"reasoning","id":"rs_1","summary":[]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking."}]},
		{"type":"function_call","call_id":"`+id+`","name":"get_weather","arguments":"{\"city\":\"Hue\"}"},
		{"type":"function_call_output","call_id":"`+id+`","output":"sunny"}],
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	b := string((*bodies)[0])
	if strings.Count(b, "get_weather:0#ab") != 2 || !strings.Contains(b, "sunny") || !strings.Contains(b, "Checking.") {
		t.Error("history not forwarded")
	}
}

func TestResponsesRejects(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	for _, body := range []string{
		`{"input":"hi","previous_response_id":"resp_1"}`,
		`{"input":[{"type":"file_search_call"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"f_1"}]}]}`,
		`{}`,
	} {
		if rec := postV1(srv, "/v1/responses", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, rec.Code)
		}
	}
}

func TestAppendAssistantMerging(t *testing.T) {
	var p []devin.Prompt
	p = appendAssistantText(p, "a")
	p = appendAssistantCall(p, devin.ToolCall{ID: "1"})
	p = appendAssistantCall(p, devin.ToolCall{ID: "2"})
	p = append(p, devin.Prompt{Source: devin.SourceToolResult})
	p = appendAssistantText(p, "b")
	if len(p) != 3 || p[0].Text != "a" || len(p[0].ToolCalls) != 2 || p[2].Text != "b" {
		t.Errorf("prompts = %+v", p)
	}
}

func TestMessagesAuthErrorIsAnthropicShaped(t *testing.T) {
	srv := chatTestServer(t, &fakeDevin{}, "eyJtok1")
	srv.tokens.Create(store.TokenOptions{})
	rec := postV1(srv, "/v1/messages", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"authentication_error"`) {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestResponsesStreamIncompleteEvent(t *testing.T) {
	up := &fakeDevin{}
	srv := chatTestServer(t, up, "eyJtok1")
	// Upstream stop reason 1 = length.
	srv.loginServerBase = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(connectFrame(0, devin.PwFieldString(3, "partial")))
		w.Write(connectFrame(0, devin.PwFieldVarint(5, 1)))
		w.Write(connectFrame(2, []byte("{}")))
	})).URL
	rec := postV1(srv, "/v1/responses", `{"stream":true,"input":"hi","max_output_tokens":5}`)
	names, data := sseEvents(t, rec.Body.String())
	last := names[len(names)-1]
	if last != "response.incomplete" || data[len(data)-1]["response"].(map[string]any)["status"] != "incomplete" {
		t.Errorf("last event = %s", last)
	}
}
