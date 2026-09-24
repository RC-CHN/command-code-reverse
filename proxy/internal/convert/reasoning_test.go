package convert

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
)

func TestAssistantReasoningHistory(t *testing.T) {
	for _, tc := range []struct{ name, message, want string }{
		{"field", `{"role":"assistant","content":"answer","reasoning_content":" think\n"}`, `[{"type":"reasoning","text":" think\n"},{"type":"text","text":"answer"}]`},
		{"null content", `{"role":"assistant","content":null,"reasoning_content":"thought"}`, `[{"type":"reasoning","text":"thought"}]`},
		{"missing content", `{"role":"assistant","reasoning_content":"thought"}`, `[{"type":"reasoning","text":"thought"}]`},
		{"blocks preserve order", `{"role":"assistant","content":[{"type":"reasoning","text":"first"},{"type":"text","text":"a"},{"type":"reasoning","text":"second"},{"type":"text","text":"b"}]}`, `[{"type":"reasoning","text":"first"},{"type":"text","text":"a"},{"type":"reasoning","text":"second"},{"type":"text","text":"b"}]`},
		{"duplicate representations", `{"role":"assistant","reasoning_content":"firstsecond","content":[{"type":"reasoning","text":"first"},{"type":"text","text":"a"},{"type":"reasoning","text":"second"}]}`, `[{"type":"reasoning","text":"first"},{"type":"text","text":"a"},{"type":"reasoning","text":"second"}]`},
		{"distinct representations", `{"role":"assistant","reasoning_content":"extra","content":[{"type":"reasoning","text":"block"},{"type":"text","text":"a"}]}`, `[{"type":"reasoning","text":"extra"},{"type":"reasoning","text":"block"},{"type":"text","text":"a"}]`},
		{"empty field", `{"role":"assistant","content":"a","reasoning_content":""}`, `[{"type":"text","text":"a"}]`},
		{"null field", `{"role":"assistant","content":"a","reasoning_content":null}`, `[{"type":"text","text":"a"}]`},
		{"empty block", `{"role":"assistant","content":[{"type":"reasoning","text":""},{"type":"text","text":"a"}]}`, `[{"type":"text","text":"a"}]`},
		{"tool call without answer", `{"role":"assistant","content":null,"reasoning_content":"need tool","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"id\":1}"}}]}`, `[{"type":"reasoning","text":"need tool"},{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"id":1}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m Message
			if err := json.Unmarshal([]byte(tc.message), &m); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(m)
			wire, err := ToWire(&ChatRequest{Model: "m", Messages: []Message{m}}, "", 200000, 64000)
			if err != nil {
				t.Fatal(err)
			}
			var want []commandcode.WireContentPart
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if len(wire.Params.Messages) != 1 || !reflect.DeepEqual(wire.Params.Messages[0].Content, want) {
				t.Fatalf("reasoning history lost/reordered: got %+v; want %s", wire.Params.Messages, tc.want)
			}
			after, _ := json.Marshal(m)
			if string(before) != string(after) {
				t.Fatal("conversion mutated input history")
			}
		})
	}
}

func TestReasoningIsAssistantOnly(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"system","content":"system","reasoning_content":"ignore system"},{"role":"user","reasoning_content":"ignore user","content":[{"type":"reasoning","text":"ignore block"},{"type":"text","text":"question"}]}]}`), &req); err != nil {
		t.Fatal(err)
	}
	wire, err := ToWire(&req, "", 200000, 64000)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Params.System != "system" || len(wire.Params.Messages) != 1 || len(wire.Params.Messages[0].Content) != 1 || wire.Params.Messages[0].Content[0].Text != "question" {
		t.Fatalf("reasoning leaked into a different role: %+v", wire.Params)
	}
}
