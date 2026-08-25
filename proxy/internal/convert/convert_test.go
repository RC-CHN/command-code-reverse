package convert

import (
	"encoding/json"
	"testing"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestToWireBasic(t *testing.T) {
	req := &ChatRequest{
		Model: "deepseek/deepseek-v4-flash",
		Messages: []Message{
			{Role: "system", Content: mustJSON(t, "You are helpful.")},
			{Role: "user", Content: mustJSON(t, "hi")},
		},
	}
	w, err := ToWire(req, "11111111-2222-3333-4444-555555555555", 200000, 64000)
	if err != nil {
		t.Fatalf("ToWire: %v", err)
	}
	if w.Mode != "agent" {
		t.Errorf("Mode = %q", w.Mode)
	}
	if w.PermissionMode != "standard" {
		t.Errorf("PermissionMode = %q", w.PermissionMode)
	}
	if w.ThreadID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("ThreadID = %q", w.ThreadID)
	}
	if w.Memory != nil || w.Taste != nil || w.Skills != nil {
		t.Errorf("memory/taste/skills must be null, got %v/%v/%v", w.Memory, w.Taste, w.Skills)
	}
	if w.Params.System != "You are helpful." {
		t.Errorf("System = %q", w.Params.System)
	}
	if !w.Params.Stream {
		t.Error("upstream stream must always be true")
	}
	if w.Params.MaxTokens != 64000 {
		t.Errorf("MaxTokens = %d, want default 64000", w.Params.MaxTokens)
	}
	if len(w.Params.Messages) != 1 || w.Params.Messages[0].Role != "user" {
		t.Fatalf("Messages = %+v", w.Params.Messages)
	}
	p := w.Params.Messages[0].Content[0]
	if p.Type != "text" || p.Text != "hi" {
		t.Errorf("part = %+v", p)
	}
}

func TestToWireMaxTokensClamp(t *testing.T) {
	big := 999999
	req := &ChatRequest{
		Model:     "m",
		Messages:  []Message{{Role: "user", Content: mustJSON(t, "x")}},
		MaxTokens: &big,
	}
	w, err := ToWire(req, "", 200000, 64000)
	if err != nil {
		t.Fatalf("ToWire: %v", err)
	}
	if w.Params.MaxTokens != 200000 {
		t.Errorf("MaxTokens = %d, want clamped 200000", w.Params.MaxTokens)
	}
}

func TestToWireAssistantToolCalls(t *testing.T) {
	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "assistant", Content: mustJSON(t, "let me check"), ToolCalls: []ToolCall{func() ToolCall {
				var tc ToolCall
				tc.ID = "call_1"
				tc.Type = "function"
				tc.Function.Name = "get_weather"
				tc.Function.Arguments = `{"city":"bj"}`
				return tc
			}()}},
			{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: mustJSON(t, "sunny")},
		},
	}
	w, err := ToWire(req, "", 200000, 64000)
	if err != nil {
		t.Fatalf("ToWire: %v", err)
	}
	if len(w.Params.Messages) != 2 {
		t.Fatalf("Messages = %+v", w.Params.Messages)
	}
	asst := w.Params.Messages[0]
	if asst.Role != "assistant" || len(asst.Content) != 2 {
		t.Fatalf("assistant = %+v", asst)
	}
	tc := asst.Content[1]
	if tc.Type != "tool-call" || tc.ToolCallID != "call_1" || tc.ToolName != "get_weather" {
		t.Errorf("tool-call part = %+v", tc)
	}
	if tc.Input["city"] != "bj" {
		t.Errorf("input = %v", tc.Input)
	}
	tool := w.Params.Messages[1]
	if tool.Role != "tool" || tool.Content[0].Type != "tool-result" || tool.Content[0].Output.Value != "sunny" {
		t.Errorf("tool msg = %+v", tool)
	}
}

func TestToWireToolsAndChoice(t *testing.T) {
	req := &ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: mustJSON(t, "x")}},
		Tools: []Tool{func() Tool {
			var tool Tool
			tool.Type = "function"
			tool.Function.Name = "f"
			tool.Function.Description = "d"
			tool.Function.Parameters = mustJSON(t, map[string]any{"type": "object"})
			return tool
		}()},
		ToolChoice: mustJSON(t, "required"),
	}
	w, err := ToWire(req, "", 200000, 64000)
	if err != nil {
		t.Fatalf("ToWire: %v", err)
	}
	if len(w.Params.Tools) != 1 || w.Params.Tools[0].Name != "f" || w.Params.Tools[0].Type != "function" {
		t.Errorf("tools = %+v", w.Params.Tools)
	}
	// ToolChoice is `any`; re-marshal to check the wire value ("required"→"any").
	b, _ := json.Marshal(w.Params.ToolChoice)
	var v map[string]string
	_ = json.Unmarshal(b, &v)
	if v["type"] != "any" {
		t.Errorf("tool_choice = %s", b)
	}
}

func TestToWireToolChoiceFunction(t *testing.T) {
	req := &ChatRequest{
		Model:      "m",
		Messages:   []Message{{Role: "user", Content: mustJSON(t, "x")}},
		ToolChoice: mustJSON(t, map[string]any{"type": "function", "function": map[string]string{"name": "f"}}),
	}
	w, err := ToWire(req, "", 200000, 64000)
	if err != nil {
		t.Fatalf("ToWire: %v", err)
	}
	b, _ := json.Marshal(w.Params.ToolChoice)
	var v map[string]string
	_ = json.Unmarshal(b, &v)
	if v["type"] != "tool" || v["name"] != "f" {
		t.Errorf("tool_choice = %s", b)
	}
}

func TestToWireImageMessage(t *testing.T) {
	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "user", Content: mustJSON(t, []map[string]any{
				{"type": "text", "text": "what is this"},
				{"type": "image_url", "image_url": map[string]string{"url": "data:image/jpeg;base64,AAAA"}},
			})},
		},
	}
	w, err := ToWire(req, "", 200000, 64000)
	if err != nil {
		t.Fatalf("ToWire: %v", err)
	}
	parts := w.Params.Messages[0].Content
	if len(parts) != 2 || parts[1].Type != "image" || parts[1].MimeType != "image/jpeg" {
		t.Errorf("parts = %+v", parts)
	}
}

func TestToWireRequiresModel(t *testing.T) {
	req := &ChatRequest{Messages: []Message{{Role: "user", Content: mustJSON(t, "x")}}}
	if _, err := ToWire(req, "", 200000, 64000); err == nil {
		t.Fatal("expected error for empty model")
	}
}
