package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
)

func TestReasoningToolRoundTrip(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streamed), func(t *testing.T) {
			up := &stubUpstream{ndjson: `{"type":"reasoning-start","id":"r-0"}
{"type":"reasoning-delta","id":"r-0","text":"need "}
{"type":"reasoning-delta","id":"r-0","text":"tool\n"}
{"type":"reasoning-end","id":"r-0"}
{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"id":1}}
{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":10,"outputTokens":5}}
`}
			h := New(testConfig(), Deps{Upstream: up}, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions",
				fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":"look up 1"}]}`, streamed)))
			if rec.Code != 200 {
				t.Fatalf("first turn: status=%d body=%s", rec.Code, rec.Body.String())
			}
			message := collectReasoningToolMessage(t, rec.Body.String(), streamed)
			if message["reasoning_content"] != "need tool\n" {
				t.Fatalf("first turn lost reasoning: %+v", message)
			}

			// Replay exactly what a client received, followed by its tool result.
			body, err := json.Marshal(map[string]any{
				"model": "m", "stream": streamed,
				"messages": []any{
					map[string]any{"role": "user", "content": "look up 1"},
					message,
					map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "42"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			up.ndjson = `{"type":"text-delta","text":"42"}
{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":20,"outputTokens":1}}
`
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", string(body)))
			assertChatContent(t, rec, streamed, "42")
			want := []commandcode.WireMessage{
				{Role: "user", Content: []commandcode.WireContentPart{{Type: "text", Text: "look up 1"}}},
				{Role: "assistant", Content: []commandcode.WireContentPart{
					{Type: "reasoning", Text: "need tool\n"},
					{Type: "tool-call", ToolCallID: "call_1", ToolName: "lookup", Input: map[string]any{"id": float64(1)}},
				}},
				{Role: "tool", Content: []commandcode.WireContentPart{
					{Type: "tool-result", ToolCallID: "call_1", ToolName: "lookup", Output: &commandcode.WireOutput{Type: "text", Value: "42"}},
				}},
			}
			if !reflect.DeepEqual(up.gotReq.Params.Messages, want) {
				t.Fatalf("history changed on replay: got %+v, want %+v", up.gotReq.Params.Messages, want)
			}
		})
	}
}

// collectReasoningToolMessage assembles the single tool call emitted by this
// fixture, retaining reasoning_content as an OpenAI-compatible client would.
func collectReasoningToolMessage(t *testing.T, body string, streamed bool) map[string]any {
	t.Helper()
	type choice struct {
		Message map[string]any `json:"message"`
		Delta   map[string]any `json:"delta"`
	}
	decode := func(data string) choice {
		t.Helper()
		var response struct {
			Choices []choice `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Choices) != 1 {
			t.Fatalf("unexpected choices: %s", data)
		}
		return response.Choices[0]
	}
	if !streamed {
		return decode(body).Message
	}
	message := map[string]any{"role": "assistant", "content": nil}
	var reasoning strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		delta := decode(strings.TrimPrefix(line, "data: ")).Delta
		if text, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(text)
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, call := range calls {
				delete(call.(map[string]any), "index")
			}
			message["tool_calls"] = calls
		}
	}
	message["reasoning_content"] = reasoning.String()
	return message
}
