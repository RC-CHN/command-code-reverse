package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/convert"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/metrics"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stream"
)

const terminalText = `{"type":"text-delta","text":"answer"}` + "\n"
const terminalThinking = `{"type":"reasoning-delta","text":"synthetic private reasoning"}` + "\n"
const terminalTool2 = `{"type":"tool-call","toolCallId":"call_2","toolName":"lookup","input":{"id":2}}` + "\n"
const terminalTool = `{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"id":1}}` + "\n"

func terminalToolInput(kind, id string) string {
	return fmt.Sprintf(`{"type":%q,"id":%q,"delta":"{}"}`+"\n", kind, id)
}

func terminalFinish(reason string) string {
	return fmt.Sprintf(`{"type":"finish","finishReason":%q,"totalUsage":{"inputTokens":10,"outputTokens":2}}`+"\n", reason)
}

func TestIncompleteTerminalNeverSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name, events, code string
		visible            bool
	}{
		{"abort", `{"type":"abort"}`, "upstream_aborted", false},
		{"thinking abort", terminalThinking + `{"type":"abort"}`, "upstream_aborted", true},
		{"text then abort", terminalText + `{"type":"abort"}`, "upstream_aborted", true},
		{"finish then abort", terminalText + terminalFinish("stop") + `{"type":"abort"}`, "upstream_aborted", true},
		{"finish then error", terminalText + terminalFinish("stop") + `{"type":"error","error":"synthetic failure"}`, "", true},
		{"unknown finish", terminalText + terminalFinish("private-unknown"), "upstream_invalid_finish", true},
		{"error finish", terminalText + terminalFinish("error"), "upstream_invalid_finish", true},
		{"step masks error", terminalText + `{"type":"finish-step","finishReason":"stop"}` + "\n" + terminalFinish("error"), "upstream_invalid_finish", true},
		{"error step", terminalText + `{"type":"finish-step","finishReason":"error"}` + "\n" + terminalFinish("stop"), "upstream_invalid_finish", true},
		{"thinking only", terminalThinking + terminalFinish("stop"), "upstream_incomplete_response", true},
		{"thinking length then abort", terminalThinking + terminalFinish("length") + `{"type":"abort"}`, "upstream_aborted", true},
		{"thinking length without finish", terminalThinking + `{"type":"finish-step","finishReason":"length"}`, "upstream_truncated", true},
		{"thinking length with pending tool", terminalThinking + terminalToolInput("tool-input-start", "call_1") + terminalFinish("length"), "upstream_invalid_tool_call", true},
		{"empty finish", terminalFinish("stop"), "upstream_incomplete_response", false},
		{"whitespace only", `{"type":"text-delta","text":" \n\t"}` + "\n" + terminalFinish("stop"), "upstream_incomplete_response", true},
		{"missing tool", terminalThinking + terminalFinish("tool-calls"), "upstream_invalid_tool_call", true},
		{"partial tool", `{"type":"tool-input-start","id":"call_1","toolName":"lookup"}` + "\n" + `{"type":"tool-input-delta","id":"call_1","delta":"{\"id\":"}` + "\n" + terminalFinish("tool-calls"), "upstream_invalid_tool_call", false},
	} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streamed), func(t *testing.T) {
				counters := metrics.New()
				h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: tc.events}, Metrics: counters}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[{"role":"user","content":"synthetic"}]}`, streamed)))
				body := rec.Body.String()
				wantStatus := http.StatusBadGateway
				if streamed && tc.visible {
					wantStatus = http.StatusOK
				}
				if rec.Code != wantStatus || !strings.Contains(body, `"error":`) || strings.Contains(body, `"finish_reason":"`) {
					t.Fatalf("status=%d body=%s", rec.Code, body)
				}
				if tc.code != "" && !strings.Contains(body, `"code":"`+tc.code+`"`) {
					t.Fatalf("missing failure code: %s", body)
				}
				if strings.Contains(body, "private-unknown") {
					t.Fatal("raw unknown finish reason leaked")
				}
				if streamed && tc.visible && strings.Count(body, stream.DoneSSE) != 1 {
					t.Fatalf("missing/duplicate DONE: %s", body)
				}
				rendered := counters.Render()
				if strings.Contains(rendered, `result="ok"`) || strings.Contains(rendered, "commandcode_proxy_tokens_total{") {
					t.Fatalf("failure counted as success: %s", rendered)
				}
			})
		}
	}
}

func TestInvalidToolCallIsNeverEmitted(t *testing.T) {
	for _, event := range []string{
		`{"type":"tool-call","toolName":"lookup","input":{}}`,
		`{"type":"tool-call","toolCallId":"call_1","input":{}}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup"}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":null}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":[]}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":"{unfinished"}`,
	} {
		for _, streamed := range []bool{false, true} {
			h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: terminalThinking + event + "\n" + terminalFinish("tool-calls")}}, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[{"role":"user","content":"synthetic"}]}`, streamed)))
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"upstream_invalid_tool_call"`) || strings.Contains(body, `"tool_calls"`) || strings.Contains(body, `"finish_reason":"`) {
				t.Fatalf("invalid tool escaped: %s", body)
			}
		}
	}
}

func TestPendingToolsAndPostFinishOutputNeverSucceed(t *testing.T) {
	start1 := terminalToolInput("tool-input-start", "call_1")
	start2 := terminalToolInput("tool-input-start", "call_2")
	delta1 := terminalToolInput("tool-input-delta", "call_1")
	end1 := terminalToolInput("tool-input-end", "call_1")
	for _, tc := range []struct {
		name, events, code string
		priorTools         int
	}{
		{"text with pending tool", terminalText + start1 + delta1 + terminalFinish("stop"), "upstream_invalid_tool_call", 0},
		{"complete and pending tools", terminalTool + start2 + terminalFinish("tool-calls"), "upstream_invalid_tool_call", 1},
		{"end is not complete", terminalText + start1 + delta1 + end1 + terminalFinish("stop"), "upstream_invalid_tool_call", 0},
		{"end without start", terminalText + end1 + terminalFinish("stop"), "upstream_invalid_tool_call", 0},
		{"delta without start", terminalText + delta1 + terminalFinish("length"), "upstream_invalid_tool_call", 0},
		{"wrong call id", start2 + terminalTool + terminalFinish("tool-calls"), "upstream_invalid_tool_call", 1},
		{"second id not cleared", start1 + start2 + terminalTool + terminalFinish("tool-calls"), "upstream_invalid_tool_call", 1},
		{"filter cannot hide pending tool", terminalText + start1 + terminalFinish("content-filter"), "upstream_invalid_tool_call", 0},
		{"raw refusal cannot hide pending tool", terminalText + start1 + `{"type":"finish","finishReason":"stop","rawFinishReason":"refusal"}`, "upstream_invalid_tool_call", 0},
		{"start without id", terminalText + terminalToolInput("tool-input-start", "") + terminalFinish("stop"), "upstream_invalid_tool_call", 0},
		{"delta without id", terminalText + terminalToolInput("tool-input-delta", "") + terminalFinish("stop"), "upstream_invalid_tool_call", 0},
		{"end without id", terminalText + terminalToolInput("tool-input-end", "") + terminalFinish("stop"), "upstream_invalid_tool_call", 0},
		{"tool after finish", terminalText + terminalFinish("stop") + terminalTool, "upstream_invalid_sequence", 0},
		{"pending tool after finish", terminalText + terminalFinish("stop") + start1, "upstream_invalid_sequence", 0},
		{"text after finish", terminalText + terminalFinish("stop") + terminalText, "upstream_invalid_sequence", 0},
		{"thinking after finish", terminalText + terminalFinish("stop") + terminalThinking, "upstream_invalid_sequence", 0},
		{"late tool cannot rescue empty finish", terminalThinking + terminalFinish("stop") + terminalTool, "upstream_invalid_sequence", 0},
		{"conflicting stop then length", terminalText + terminalFinish("stop") + terminalFinish("length"), "upstream_invalid_finish", 0},
		{"conflicting length then stop", terminalText + terminalFinish("length") + terminalFinish("stop"), "upstream_invalid_finish", 0},
		{"conflicting stop then raw refusal", terminalText + terminalFinish("stop") + `{"type":"finish","rawFinishReason":"refusal"}`, "upstream_invalid_finish", 0},
	} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streamed), func(t *testing.T) {
				h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: tc.events}}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[{"role":"user","content":"synthetic"}]}`, streamed)))
				body := rec.Body.String()
				wantStatus, wantTools := http.StatusBadGateway, 0
				if streamed {
					wantStatus, wantTools = http.StatusOK, tc.priorTools
				}
				if rec.Code != wantStatus || !strings.Contains(body, `"code":"`+tc.code+`"`) || strings.Contains(body, `"finish_reason":"`) {
					t.Fatalf("incomplete sequence succeeded: %d %s", rec.Code, body)
				}
				if got := strings.Count(body, `"tool_calls":`); got != wantTools {
					t.Fatalf("unexpected tool emission: got %d, want %d: %s", got, wantTools, body)
				}
			})
		}
	}
}

func TestMalformedCriticalEventsCannotBecomeSuccess(t *testing.T) {
	for _, event := range []string{
		`{"type":"tool-input-start","id":123}`,
		`{"type":"tool-input-delta","id":{}}`,
		`{"type":"tool-input-end","id":[]}`,
		`{"type":"tool-call","toolCallId":123,"toolName":"private-name","input":{}}`,
		`{"type":"tool-call","toolCallId":"private-id","toolName":{},"input":{}}`,
		`{"type":"finish","finishReason":123}`,
		`{"type":"finish","rawFinishReason":123}`,
		`{"type":"finish-step","usage":{"outputTokens":"private-value"}}`,
		`{"type":"abort","id":123}`,
		`{"type":"error","error":true}`,
		`{"type":"tool-call","input":{"private-argument":`,
		`{"type":"finish","finishReason":`,
		`{"type":"finish"`,
		`{"type":"error","error":`,
	} {
		for _, prefix := range []string{"", terminalText, terminalText + terminalFinish("stop")} {
			for _, streamed := range []bool{false, true} {
				counters := metrics.New()
				h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: prefix + event + "\n" + terminalText + terminalFinish("stop")}, Metrics: counters}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[{"role":"user","content":"synthetic"}]}`, streamed)))
				body := rec.Body.String()
				wantStatus := http.StatusBadGateway
				if streamed && prefix != "" {
					wantStatus = http.StatusOK
				}
				if rec.Code != wantStatus || !strings.Contains(body, `"code":"upstream_malformed_event"`) || strings.Contains(body, `"finish_reason":"`) || strings.Contains(body, `"tool_calls":`) || strings.Contains(body, "private-") {
					t.Fatalf("malformed event escaped: %d %s", rec.Code, body)
				}
				rendered := counters.Render()
				if strings.Contains(rendered, `result="ok"`) || strings.Contains(rendered, `result="timeout"`) {
					t.Fatalf("malformed event misattributed: %s", rendered)
				}
			}
		}
	}
}

func TestValidTerminalSemanticsRemain(t *testing.T) {
	for _, tc := range []struct{ name, events, reason string }{
		{"stop", terminalText + terminalFinish("stop"), "stop"},
		{"omitted reason", terminalText + `{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":2}}`, "stop"},
		{"tool", terminalThinking + terminalTool + terminalFinish("tool-calls"), "tool_calls"},
		{"complete tool lifecycle", terminalToolInput("tool-input-start", "call_1") + terminalToolInput("tool-input-delta", "call_1") + terminalToolInput("tool-input-end", "call_1") + terminalTool + terminalFinish("tool-calls"), "tool_calls"},
		{"interleaved tool ids", terminalToolInput("tool-input-start", "call_1") + terminalToolInput("tool-input-start", "call_2") + terminalToolInput("tool-input-end", "call_1") + terminalTool + terminalToolInput("tool-input-delta", "call_2") + terminalToolInput("tool-input-end", "call_2") + terminalTool2 + terminalFinish("tool-calls"), "tool_calls"},
		{"length text", terminalText + terminalFinish("length"), "length"},
		{"length tool", terminalTool + terminalFinish("max_tokens"), "length"},
		{"length thinking", terminalThinking + terminalFinish("length"), "length"},
		{"max tokens thinking", terminalThinking + terminalFinish("max_tokens"), "length"},
		{"length empty", terminalFinish("length"), "length"},
		{"length zero usage", `{"type":"finish","finishReason":"length","totalUsage":{"inputTokens":10,"outputTokens":0}}`, "length"},
		{"length missing usage", terminalThinking + `{"type":"finish","finishReason":"length"}`, "length"},
		{"thinking inherits step length", terminalThinking + `{"type":"finish-step","finishReason":"length"}` + "\n" + `{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":2}}`, "length"},
		{"filter empty", terminalFinish("content-filter"), "content_filter"},
		{"refusal alias", terminalFinish("refusal"), "content_filter"},
		{"refusal normalized", terminalFinish(" REFUSAL "), "content_filter"},
		{"raw refusal", `{"type":"finish","finishReason":"stop","rawFinishReason":"content_filter","totalUsage":{"outputTokens":0}}`, "content_filter"},
		{"raw refusal only", `{"type":"finish","rawFinishReason":"refusal"}`, "content_filter"},
		{"raw refusal with other reason", `{"type":"finish","finishReason":"other","rawFinishReason":"refusal"}`, "content_filter"},
		{"raw refusal overrides step", terminalText + `{"type":"finish-step","finishReason":"stop"}` + "\n" + `{"type":"finish","rawFinishReason":"refusal"}`, "content_filter"},
		{"raw refusal inherits step", `{"type":"finish-step","finishReason":"stop","rawFinishReason":"refusal"}` + "\n" + `{"type":"finish"}`, "content_filter"},
		{"filter zero usage", `{"type":"finish","finishReason":"content_filter","totalUsage":{"inputTokens":10,"outputTokens":0}}`, "content_filter"},
		{"final overrides step", terminalText + `{"type":"finish-step","finishReason":"stop"}` + "\n" + terminalFinish("length"), "length"},
		{"late step preserves final", terminalText + terminalFinish("length") + `{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":10,"outputTokens":2}}`, "length"},
		{"omitted final inherits step", terminalText + `{"type":"finish-step","finishReason":"length"}` + "\n" + `{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":2}}`, "length"},
		{"repeated finish", terminalText + terminalFinish("stop") + terminalFinish("stop"), "stop"},
		{"repeated finish alias", terminalTool + terminalFinish("tool-calls") + terminalFinish("tool_calls"), "tool_calls"},
		{"repeated finish omitted", terminalText + terminalFinish("length") + terminalFinish(""), "length"},
	} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streamed), func(t *testing.T) {
				h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: tc.events}}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[{"role":"user","content":"synthetic"}]}`, streamed)))
				body := rec.Body.String()
				if rec.Code != 200 || !strings.Contains(body, `"finish_reason":"`+tc.reason+`"`) || strings.Contains(body, `"error":`) {
					t.Fatalf("valid finish changed: %d %s", rec.Code, body)
				}
				if strings.Count(body, `"finish_reason":"`+tc.reason+`"`) != 1 {
					t.Fatalf("duplicate final finish: %s", body)
				}
				if streamed && strings.Count(body, stream.DoneSSE) != 1 {
					t.Fatalf("invalid terminal count: %s", body)
				}
			})
		}
	}
}

func TestReasoningLengthPreservesResponseAndAccounting(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streamed), func(t *testing.T) {
			upstream := &stubUpstream{ndjson: terminalThinking +
				`{"type":"finish","finishReason":"length","totalUsage":{"inputTokens":32,"outputTokens":16,"outputTokenDetails":{"reasoningTokens":16}}}` + "\n"}
			counters := metrics.New()
			h := New(dashboardConfig(), Deps{Upstream: upstream, Metrics: counters}, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, streamed)))
			body := rec.Body.String()
			if rec.Code != http.StatusOK || strings.Contains(body, `"error":`) || !strings.Contains(body, `"finish_reason":"length"`) {
				t.Fatalf("token limit treated as failure: %d %s", rec.Code, body)
			}
			for _, field := range []string{`"reasoning_content":"synthetic private reasoning"`, `"completion_tokens":16`, `"reasoning_tokens":16`} {
				if !strings.Contains(body, field) {
					t.Fatalf("lost %s: %s", field, body)
				}
			}
			if streamed {
				if strings.Contains(body, `"content":`) || strings.Count(body, stream.DoneSSE) != 1 {
					t.Fatalf("unexpected content or stream terminator: %s", body)
				}
			} else if !strings.Contains(body, `"content":null`) {
				t.Fatalf("invented final answer: %s", body)
			}
			if upstream.gotReq.Params.MaxTokens != 16 {
				t.Fatal("downstream output budget was changed")
			}
			out := dashboardSnapshot(t, h)
			if out.Totals.Succeeded != 1 || out.Totals.Failed != 0 || out.Totals.Tokens.Output != 16 || out.Totals.Tokens.Reasoning != 16 || out.Recent[0].Result != "ok" {
				t.Fatalf("length accounting lost: %+v", out)
			}
			if rendered := counters.Render(); !strings.Contains(rendered, `result="ok"`) || !strings.Contains(rendered, `commandcode_proxy_tokens_total{kind="output"} 16`) {
				t.Fatalf("length metrics lost: %s", rendered)
			}
		})
	}
}

func TestFinishIsNotPublishedUntilTailCompletes(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streamed), func(t *testing.T) {
			body := io.MultiReader(strings.NewReader(terminalText+terminalFinish("stop")), iotest.ErrReader(io.ErrUnexpectedEOF))
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			s := &Server{cfg: testConfig()}
			req := &convert.ChatRequest{Model: "synthetic", Stream: streamed}
			timer := time.NewTimer(time.Hour)
			defer timer.Stop()
			if streamed {
				s.serveStream(rec, r, body, req, time.Now(), timer, time.Hour)
			} else {
				s.serveNonStream(rec, r, body, req, time.Now(), timer, time.Hour)
			}
			if !strings.Contains(rec.Body.String(), `"error":`) || strings.Contains(rec.Body.String(), `"finish_reason":"`) {
				t.Fatalf("tail failure hidden: %s", rec.Body.String())
			}
		})
	}
}

func TestConcurrentTerminalValidationIsolation(t *testing.T) {
	for i := 0; i < 16; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			events := terminalThinking + terminalFinish("stop")
			wantError := i%2 == 0
			if !wantError {
				events = terminalTool + terminalFinish("tool-calls")
			}
			h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: events}}, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", `{"model":"synthetic","stream":true,"messages":[{"role":"user","content":"synthetic"}]}`))
			if strings.Contains(rec.Body.String(), `"error":`) != wantError {
				t.Fatal("cross-request terminal state")
			}
		})
	}
}
