package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/metrics"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stream"
)

func TestChatRejectsTruncatedCompletion(t *testing.T) {
	const step = `{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":10,"outputTokens":2}}` + "\n"
	for _, tc := range []struct {
		name, events string
		visible      bool
	}{
		{"empty", "", false},
		{"step only", step, false},
		{"text", `{"type":"text-delta","text":"partial"}` + "\n", true},
		{"text and step", `{"type":"text-delta","text":"partial"}` + "\n" + step, true},
		{"reasoning", `{"type":"reasoning-delta","text":"partial"}` + "\n" + step, true},
		{"tool call", `{"type":"tool-call","toolCallId":"call_1","toolName":"read","input":{}}` + "\n" + step, true},
		{"done sentinel", step + "[DONE]\n", false},
	} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streamed), func(t *testing.T) {
				stats := metrics.New()
				h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: tc.events}, Metrics: stats}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions",
					fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, streamed)))
				wantStatus := http.StatusBadGateway
				if streamed && tc.visible {
					wantStatus = http.StatusOK // SSE headers already sent.
				}
				body := rec.Body.String()
				if rec.Code != wantStatus || !strings.Contains(body, `"code":"upstream_truncated"`) {
					t.Fatalf("status=%d body=%s", rec.Code, body)
				}
				if strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "Response timeout") {
					t.Fatalf("truncation reported as normal completion or timeout: %s", body)
				}
				if streamed && tc.visible && strings.Count(body, stream.DoneSSE) != 1 {
					t.Fatalf("missing or duplicated terminal frame: %s", body)
				}
				counters := stats.Render()
				if !strings.Contains(counters, `result="truncated_response"} 1`) || strings.Contains(counters, `result="ok"`) || strings.Contains(counters, `commandcode_proxy_tokens_total{`) {
					t.Fatalf("incorrect accounting for failure: %s", counters)
				}
			})
		}
	}
}

func TestChatRecognizesTerminalEvents(t *testing.T) {
	const prefix = `{"type":"text-delta","text":"ok"}
{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":10,"outputTokens":2}}
`
	for _, terminal := range []string{
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":2}}`,
	} {
		for _, streamed := range []bool{false, true} {
			h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: prefix + terminal}}, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions",
				fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, streamed)))
			// A successful terminal event need not have a trailing newline.
			assertChatContent(t, rec, streamed, "ok")
		}
	}
}
