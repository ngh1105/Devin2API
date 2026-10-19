package devin

import (
	"bytes"
	"testing"
)

func TestBuildChatRequestTopP(t *testing.T) {
	cfgOf := func(req ChatRequest) []byte {
		r := &pwReader{data: BuildChatRequest("tok", "fp", "s", req)}
		for {
			f, _, val, _, ok := r.Next()
			if !ok {
				return nil
			}
			if f == 8 {
				return val
			}
		}
	}
	p := []Prompt{{Source: SourceUser, Text: "hi"}}
	if !bytes.Contains(cfgOf(ChatRequest{Prompts: p, TopP: 0.5}), PwFieldDouble(8, 0.5)) {
		t.Error("top_p 0.5 not sent")
	}
	if !bytes.Contains(cfgOf(ChatRequest{Prompts: p}), PwFieldDouble(8, float64(float32(0.95)))) {
		t.Error("default top_p not sent")
	}
}
