package stream

import (
	"errors"
	"io"
	"strings"
	"testing"
)

const sampleNDJSON = `{"type":"start"}
{"type":"start-step","request":{"body":{"maxOutputTokens":32}},"warnings":[]}
{"type":"reasoning-start","id":"reasoning-0"}
{"type":"reasoning-delta","id":"reasoning-0","text":"think"}
{"type":"text-start","id":"txt-0"}
{"type":"reasoning-end","id":"reasoning-0"}
{"type":"text-delta","id":"txt-0","text":"Hi"}
{"type":"text-end","id":"txt-0"}

{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":95,"outputTokens":25,"cachedInputTokens":80}}
{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":95,"outputTokens":25,"cachedInputTokens":80}}
{"type":"provider-metadata","providerMetadata":{}}
`

func TestReaderEventSequence(t *testing.T) {
	r := NewReader(strings.NewReader(sampleNDJSON))
	var types []string
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		types = append(types, ev.Type)
	}
	want := []string{"start", "start-step", "reasoning-start", "reasoning-delta",
		"text-start", "reasoning-end", "text-delta", "text-end",
		"finish-step", "finish", "provider-metadata"}
	if len(types) != len(want) {
		t.Fatalf("types = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("types[%d] = %q, want %q", i, types[i], want[i])
		}
	}
}

func TestReaderSkipsNoise(t *testing.T) {
	r := NewReader(strings.NewReader("\n[DONE]\n: keepalive\nnot json\n{\"type\":\"text-delta\",\"text\":\"a\"}\n"))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Type != "text-delta" || ev.Text != "a" {
		t.Errorf("ev = %+v", ev)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestTerminalErrorMarkers(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantHit bool
	}{
		{"premium exhausted", `{"type":"error","error":{"message":"premium_credits_exhausted"}}`, true},
		{"model not in plan", `{"type":"error","error":{"message":"model_not_in_plan: claude-x"}}`, true},
		{"insufficient embedded", `{"type":"error","error":{"message":"400 {\"error\":\"insufficient credits\"}"}}`, true},
		{"generic error", `{"type":"error","error":{"message":"upstream overloaded"}}`, false},
		{"not an error event", `{"type":"text-delta","text":"premium_credits_exhausted"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(strings.NewReader(tc.line + "\n"))
			ev, err := r.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			got := TerminalError(ev)
			if tc.wantHit && got == nil {
				t.Error("expected terminal marker, got nil")
			}
			if !tc.wantHit && got != nil {
				t.Errorf("unexpected marker: %+v", got)
			}
		})
	}
}

func TestUsageNormalization(t *testing.T) {
	u := UsageFromUpstream(&Usage{InputTokens: 95, OutputTokens: 25, CachedInputTokens: 80})
	if u.PromptTokens != 95 || u.CompletionTokens != 25 || u.TotalTokens != 120 {
		t.Errorf("usage = %+v", u)
	}
	if u.PromptTokensDetails.CachedTokens != 80 {
		t.Errorf("cached = %d", u.PromptTokensDetails.CachedTokens)
	}

	// Zero-output rule: everything zeroed (anti false-billing).
	z := UsageFromUpstream(&Usage{InputTokens: 95, OutputTokens: 0, CachedInputTokens: 80})
	if z.PromptTokens != 0 || z.CompletionTokens != 0 || z.PromptTokensDetails.CachedTokens != 0 {
		t.Errorf("zero-output usage = %+v", z)
	}
}

func TestMapFinishReason(t *testing.T) {
	for in, want := range map[string]string{
		"tool-calls": "tool_calls",
		"tool_use":   "tool_calls",
		"length":     "length",
		"stop":       "stop",
		"end_turn":   "stop",
		"unknown":    "stop",
	} {
		if got := MapFinishReason(in); got != want {
			t.Errorf("MapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSEFormat(t *testing.T) {
	finish := "stop"
	c := NewChunk("chatcmpl-x", 123, "m", map[string]any{"content": "Hi"}, &finish, nil)
	s := SSE(c)
	if !strings.HasPrefix(s, "data: {") || !strings.HasSuffix(s, "}\n\n") {
		t.Errorf("SSE = %q", s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Errorf("SSE missing finish_reason: %q", s)
	}
}
