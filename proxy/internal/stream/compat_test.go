package stream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNestedUsageCounters(t *testing.T) {
	for _, tc := range []struct {
		raw               string
		cached, reasoning int
	}{
		{`{"inputTokens":100,"outputTokens":20,"inputTokenDetails":{"cacheReadTokens":80},"outputTokenDetails":{"reasoningTokens":5}}`, 80, 5},
		{`{"inputTokens":100,"outputTokens":20,"cachedInputTokens":70,"reasoningTokens":4}`, 70, 4},
		{`{"inputTokens":100,"outputTokens":20,"cachedInputTokens":70,"reasoningTokens":4,"inputTokenDetails":{"cacheReadTokens":0},"outputTokenDetails":{"reasoningTokens":0}}`, 0, 0},
		{`{"inputTokens":100,"outputTokens":20,"cachedInputTokens":70,"reasoningTokens":4,"inputTokenDetails":{"cacheReadTokens":null},"outputTokenDetails":{}}`, 70, 4},
	} {
		var u Usage
		if err := json.Unmarshal([]byte(tc.raw), &u); err != nil {
			t.Fatal(err)
		}
		got := UsageFromUpstream(&u)
		if got.PromptTokens != 100 || got.TotalTokens != 120 || got.PromptTokensDetails.CachedTokens != tc.cached || got.CompletionTokensDetails.ReasoningTokens != tc.reasoning {
			t.Fatalf("usage = %+v for %s", got, tc.raw)
		}
	}
}

func TestStreamErrorForms(t *testing.T) {
	for _, tc := range []struct {
		line, message, code string
		status              int
	}{
		{`{"type":"error","error":"overloaded"}`, "overloaded", "", 502},
		{`{"type":"error","error":{"message":"overloaded","statusCode":503}}`, "overloaded", "", 503},
		{`{"type":"error","message":"failure"}`, "failure", "", 502},
		{`{"type":"error","error":"403 {\"error\":{\"code\":\"USAGE_EXCEEDED\",\"message\":\"Org cap reached\"}}"}`, "Org cap reached", "USAGE_EXCEEDED", 403},
	} {
		ev, err := NewReader(strings.NewReader(tc.line)).Next()
		if err != nil {
			t.Fatal(err)
		}
		got := ev.APIError()
		if got == nil || got.Message != tc.message || got.Code != tc.code || got.Status != tc.status {
			t.Fatalf("APIError = %+v", got)
		}
	}
	ev, err := NewReader(strings.NewReader(`{"type":"error","error":"premium_credits_exhausted"}`)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if TerminalError(ev) == nil {
		t.Fatal("string billing marker was lost")
	}
}
