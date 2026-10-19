package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ngh1105/Devin2API/internal/devin"
)

// toolChoice is a tool_choice setting, normalised across the three APIs.
// Upstream has no option to force a tool call, so "required" and "named"
// are best effort: the tool list is narrowed and the system prompt tells
// the model to call a tool.
type toolChoice struct {
	mode string // "auto" (or ""), "none", "required", "named"
	name string // for "named"
}

// openAIToolChoice parses the chat/completions form: "auto" | "none" |
// "required" | {"type":"function","function":{"name":...}}.
func openAIToolChoice(raw json.RawMessage) (toolChoice, error) {
	return parseToolChoice(raw, func(obj map[string]json.RawMessage) string {
		var fn struct {
			Name string `json:"name"`
		}
		json.Unmarshal(obj["function"], &fn)
		return fn.Name
	})
}

// responsesToolChoice parses the Responses form, where a named function is
// {"type":"function","name":...}.
func responsesToolChoice(raw json.RawMessage) (toolChoice, error) {
	return parseToolChoice(raw, func(obj map[string]json.RawMessage) string {
		var name string
		json.Unmarshal(obj["name"], &name)
		return name
	})
}

func parseToolChoice(raw json.RawMessage, namedFn func(map[string]json.RawMessage) string) (toolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return toolChoice{}, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none", "required":
			return toolChoice{mode: s}, nil
		}
		return toolChoice{}, fmt.Errorf("tool_choice %q is not supported", s)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return toolChoice{}, fmt.Errorf("tool_choice must be a string or an object")
	}
	var typ string
	json.Unmarshal(obj["type"], &typ)
	if typ != "function" {
		return toolChoice{}, fmt.Errorf("tool_choice type %q is not supported", typ)
	}
	name := namedFn(obj)
	if name == "" {
		return toolChoice{}, fmt.Errorf("tool_choice: function name is required")
	}
	return toolChoice{mode: "named", name: name}, nil
}

// anthropicToolChoice maps {"type":"auto"|"any"|"tool"|"none","name":...}.
func anthropicToolChoice(typ, name string) (toolChoice, error) {
	switch typ {
	case "", "auto":
		return toolChoice{}, nil
	case "none":
		return toolChoice{mode: "none"}, nil
	case "any":
		return toolChoice{mode: "required"}, nil
	case "tool":
		if name == "" {
			return toolChoice{}, fmt.Errorf("tool_choice: name is required for type \"tool\"")
		}
		return toolChoice{mode: "named", name: name}, nil
	}
	return toolChoice{}, fmt.Errorf("tool_choice type %q is not supported", typ)
}

// applyToolChoice narrows dreq's tools and instructs the model. Call it after
// dreq.Tools and dreq.System are set.
func applyToolChoice(dreq *devin.ChatRequest, tc toolChoice) error {
	var rule string
	switch tc.mode {
	case "", "auto":
		return nil
	case "none":
		dreq.Tools = nil
		return nil
	case "required":
		if len(dreq.Tools) == 0 {
			return fmt.Errorf("tool_choice requires at least one tool")
		}
		rule = "Tool use is required for this reply: respond by calling one or more of the provided tools, not with a plain text answer."
	case "named":
		var picked []devin.Tool
		for _, t := range dreq.Tools {
			if t.Name == tc.name {
				picked = append(picked, t)
			}
		}
		if len(picked) == 0 {
			return fmt.Errorf("tool_choice names %q, which is not in tools", tc.name)
		}
		dreq.Tools = picked
		rule = fmt.Sprintf("Tool use is required for this reply: respond by calling the %q tool, not with a plain text answer.", tc.name)
	}
	dreq.System = strings.TrimSpace(dreq.System + "\n\n" + rule)
	return nil
}
