package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSystemCacheWire(t *testing.T) {
	var req ChatRequest
	err := json.Unmarshal([]byte(`{"model":"m","prompt_cache":"off","messages":[
		{"role":"system","content":"policy"},
		{"role":"developer","content":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}},{"type":"text","text":"tail"}]},
		{"role":"user","content":"hello"}]}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := ToWire(&req, "", 200000, 64000)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"promptCache":"off"`,
		`"system":[{"type":"text","text":"policy\n"},{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}},{"type":"text","text":"tail"}]`,
		`"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("wire missing %s: %s", want, raw)
		}
	}
}

func TestSystemCacheKeepsLegacyString(t *testing.T) {
	for _, content := range []string{`"policy"`, `[{"type":"text","text":"pol"},{"type":"text","text":"icy"}]`} {
		req := &ChatRequest{Model: "m", Messages: []Message{{Role: "system", Content: json.RawMessage(content)}}}
		wire, err := ToWire(req, "", 200000, 64000)
		if err != nil {
			t.Fatal(err)
		}
		if wire.Params.System != "policy" {
			t.Fatalf("system = %#v", wire.Params.System)
		}
		raw, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "promptCache") {
			t.Fatalf("unexpected cache override: %s", raw)
		}
	}
}

func TestRejectUnsupportedCacheSettings(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","prompt_cache":"on"}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"x","cache_control":{"type":"permanent"}}]}]}`,
	} {
		var req ChatRequest
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Fatal(err)
		}
		if _, err := ToWire(&req, "", 200000, 64000); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
