package convert

import (
	"encoding/json"
	"strings"
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

func TestConversationRoot(t *testing.T) {
	mk := func(texts ...string) []Message {
		msgs := make([]Message, 0, len(texts))
		for i, text := range texts {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			msgs = append(msgs, Message{Role: role, Content: mustJSON(t, text)})
		}
		return msgs
	}
	base := ConversationRoot(mk("hello", "hi there"))
	if len(base) != 32 {
		t.Fatalf("root length = %d", len(base))
	}
	// Stable from the very first request: one message and grown chain agree.
	if ConversationRoot(mk("hello")) != base {
		t.Error("first request (single message) must already produce the final root")
	}
	if ConversationRoot(mk("hello", "hi there", "next question")) != base {
		t.Error("root must survive prefix growth")
	}
	if ConversationRoot(mk("other", "hi")) == base {
		t.Error("different opening must yield a different root")
	}

	// System context participates: same opening under another client
	// (different system prompt) is a different conversation.
	withSys := []Message{
		{Role: "system", Content: mustJSON(t, "you are agent X")},
		{Role: "user", Content: mustJSON(t, "hello")},
	}
	if ConversationRoot(withSys) == base {
		t.Error("system prompt must differentiate otherwise-identical openings")
	}
	// And it stays stable across prefix growth.
	grownSys := append(withSys,
		Message{Role: "assistant", Content: mustJSON(t, "hi")},
		Message{Role: "user", Content: mustJSON(t, "more")},
	)
	if ConversationRoot(grownSys) != ConversationRoot(withSys) {
		t.Error("root with system must survive prefix growth")
	}
	// Mid-conversation system injections must NOT move the root.
	injected := append(grownSys, Message{Role: "system", Content: mustJSON(t, "late injection")})
	if ConversationRoot(injected) != ConversationRoot(withSys) {
		t.Error("mid-conversation system messages must not move the root")
	}

	// Array-form content with the same text yields the SAME root — a client
	// switching encodings mid-conversation must not break identity.
	parts := []Message{{Role: "user", Content: mustJSON(t, []map[string]any{{"type": "text", "text": "hello"}})}}
	if ConversationRoot(parts) != base {
		t.Error("array-form and string encodings of the same text must share the root")
	}
}

func TestToolResultNameResolution(t *testing.T) {
	mkCall := func(id, name string) ToolCall {
		var tc ToolCall
		tc.ID = id
		tc.Function.Name = name
		tc.Function.Arguments = "{}"
		return tc
	}
	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "user", Content: mustJSON(t, "go")},
			// assistant with two parallel tool calls
			{Role: "assistant", ToolCalls: []ToolCall{mkCall("c1", "read_file"), mkCall("c2", "grep")}},
			// 1. explicit name wins
			{Role: "tool", ToolCallID: "c1", Name: "custom_name", Content: mustJSON(t, "r1")},
			// 2. missing name resolves via the id→name map
			{Role: "tool", ToolCallID: "c2", Content: mustJSON(t, "r2")},
			// 3. unknown id falls back to "unknown"
			{Role: "tool", ToolCallID: "c9", Content: mustJSON(t, "r3")},
		},
	}
	w, err := ToWire(req, "", 200000, 64000)
	if err != nil {
		t.Fatalf("ToWire: %v", err)
	}
	msgs := w.Params.Messages
	got := []string{msgs[2].Content[0].ToolName, msgs[3].Content[0].ToolName, msgs[4].Content[0].ToolName}
	want := []string{"custom_name", "grep", "unknown"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("toolName[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestToWireRejectsRemoteImageURL(t *testing.T) {
	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "user", Content: mustJSON(t, []map[string]any{
				{"type": "image_url", "image_url": map[string]string{"url": "https://example.com/cat.jpg"}},
			})},
		},
	}
	_, err := ToWire(req, "", 200000, 64000)
	if err == nil || !strings.Contains(err.Error(), "data URL") {
		t.Fatalf("expected data-URL rejection, got %v", err)
	}
}

func TestToWireRequiresModel(t *testing.T) {
	req := &ChatRequest{Messages: []Message{{Role: "user", Content: mustJSON(t, "x")}}}
	if _, err := ToWire(req, "", 200000, 64000); err == nil {
		t.Fatal("expected error for empty model")
	}
}
