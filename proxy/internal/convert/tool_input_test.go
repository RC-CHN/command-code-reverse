package convert

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestToWirePreservesEmptyToolInput(t *testing.T) {
	for _, tc := range []struct{ name, args, want string }{
		{"empty object", `{}`, `{}`},
		{"whitespace", ` { } `, `{}`},
		{"missing arguments", ``, `{}`},
		{"null arguments", `null`, `{}`},
		{"malformed arguments", `{`, `{}`},
		{"array arguments", `[]`, `{}`},
		{"scalar arguments", `42`, `{}`},
		{"nonempty object", `{"count":0,"enabled":false,"nested":{},"items":[]}`, `{"count":0,"enabled":false,"nested":{},"items":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := ToolCall{ID: "call_empty", Type: "function"}
			call.Function.Name, call.Function.Arguments = "noop", tc.args
			wire, err := ToWire(&ChatRequest{Model: "m", Messages: []Message{
				{Role: "assistant", ReasoningContent: "need tool", Content: json.RawMessage(`"checking"`), ToolCalls: []ToolCall{call}},
				{Role: "tool", ToolCallID: "call_empty", Content: json.RawMessage(`"done"`)},
			}}, "", 200000, 64000)
			if err != nil {
				t.Fatal(err)
			}
			// Inspect serialized JSON: checking the Go map alone misses omitempty.
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var sent struct {
				Params struct {
					Messages []struct{ Content []map[string]json.RawMessage }
				}
			}
			if err := json.Unmarshal(raw, &sent); err != nil {
				t.Fatal(err)
			}
			parts := sent.Params.Messages[0].Content
			if len(parts) != 3 {
				t.Fatalf("parts=%+v", parts)
			}
			var got, want map[string]any
			if err := json.Unmarshal(parts[2]["input"], &got); err != nil {
				t.Fatalf("tool input omitted/invalid: %s (%v)", raw, err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if got == nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("input=%v want=%v", got, want)
			}
			for _, part := range []map[string]json.RawMessage{parts[0], parts[1], sent.Params.Messages[1].Content[0]} {
				if _, ok := part["input"]; ok {
					t.Fatalf("input leaked into another content type: %s", raw)
				}
			}
		})
	}
}
