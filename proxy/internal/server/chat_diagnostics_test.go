package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

type diagnosticClientFunc func(context.Context, commandcode.Credentials, *commandcode.GenerateRequest) (io.ReadCloser, error)

func (f diagnosticClientFunc) Generate(ctx context.Context, creds commandcode.Credentials, req *commandcode.GenerateRequest) (io.ReadCloser, error) {
	return f(ctx, creds, req)
}

func captureChatLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &out
}

func chatFailureLogs(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	if strings.Contains(out.String(), "PRIVATE_") || strings.Contains(out.String(), "keyPrefix") {
		t.Fatalf("private content/credential prefix in logs: %s", out.String())
	}
	var records []map[string]any
	decoder := json.NewDecoder(out)
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "chat request failed" {
			records = append(records, record)
		}
	}
	return records
}

func TestChatFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, events, code   string
		generateErr, readErr error
		status               int
	}{
		{name: "HTTP limit", generateErr: &commandcode.APIError{Status: 429, Message: "PRIVATE_MESSAGE"}, status: 429, code: "upstream_rate_limited"},
		{name: "stream limit", events: `{"type":"error","error":{"statusCode":429,"message":"PRIVATE_MESSAGE"}}`, status: 429, code: "upstream_rate_limited"},
		{name: "unknown upstream fields", events: `{"type":"error","error":{"statusCode":503,"code":"PRIVATE_CODE","type":"PRIVATE_TYPE","message":"PRIVATE_MESSAGE"}}`, status: 503, code: "upstream_server_error"},
		{name: "request size", events: `{"type":"error","error":{"statusCode":413,"message":"PRIVATE_MESSAGE"}}`, status: 413, code: "upstream_request_too_large"},
		{name: "abort", events: `{"type":"abort","reason":"PRIVATE_REASON"}`, status: 502, code: "upstream_aborted"},
		{name: "truncated", status: 502, code: "upstream_truncated"},
		{name: "malformed", events: `{"type":"finish","PRIVATE_BROKEN_JSON":`, status: 502, code: "upstream_malformed_event"},
		{name: "tool input", events: terminalToolInput("tool-input-start", "PRIVATE_TOOL_ID") + terminalFinish("tool-calls"), status: 502, code: "upstream_invalid_tool_call"},
		{name: "connection", generateErr: fmt.Errorf("PRIVATE_TRANSPORT_URL"), status: 502, code: "upstream_connection_error"},
		{name: "read timeout", readErr: &net.DNSError{Err: "PRIVATE_DNS_ERROR", Name: "PRIVATE_HOST", IsTimeout: true}, status: 504, code: "upstream_timeout"},
		{name: "idle timeout", readErr: errUpstreamIdle, status: 504, code: "upstream_idle_timeout"},
		{name: "credit marker", events: `{"type":"error","error":"premium_credits_exhausted PRIVATE_MESSAGE"}`, status: 402, code: "upstream_insufficient_credits"},
	} {
		for _, streamed := range []bool{false, true} {
			for _, partial := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/partial=%t", tc.name, streamed, partial), func(t *testing.T) {
					logs := captureChatLogs(t)
					cfg := testConfig()
					cfg.APIKeys = []string{"PRIVATE_UPSTREAM_KEY"}
					calls := 0
					var traceID string
					client := diagnosticClientFunc(func(ctx context.Context, creds commandcode.Credentials, _ *commandcode.GenerateRequest) (io.ReadCloser, error) {
						calls++
						traceID = creds.TraceID
						if tc.generateErr != nil {
							return nil, tc.generateErr
						}
						events := tc.events
						if partial {
							events = `{"type":"reasoning-delta","text":"PRIVATE_REASONING"}` + "\n" + events
						}
						return &scriptedChatBody{ctx: ctx, steps: []readStep{{data: events}, {err: tc.readErr}}}, nil
					})
					pool := keypool.New(client, session.NewStore("seed"), cfg.APIKeys, keypool.BreakerPolicy{})
					h := New(cfg, Deps{Upstream: pool}, nil)
					r := authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`, streamed))
					r.Header.Set("X-Request-ID", "PRIVATE_CALLER_ID")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					started := streamed && partial && tc.generateErr == nil
					wantStatus := tc.status
					if started {
						wantStatus = 200
					}
					if w.Code != wantStatus || calls != 1 {
						t.Fatalf("changed response/replay: status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
					}
					records := chatFailureLogs(t, logs)
					if len(records) != 1 {
						t.Fatalf("diagnostics=%+v", records)
					}
					got := records[0]
					stage := "stream"
					if tc.generateErr != nil {
						stage = "generate"
					}
					if got["code"] != tc.code || got["errorStatus"] != float64(tc.status) || got["stage"] != stage || got["responseStarted"] != started || got["keyId"] != "key-1" || got["attempts"] != float64(1) || got["stream"] != streamed {
						t.Fatalf("wrong diagnostics: %+v", got)
					}
					if len(traceID) != 32 || got["requestId"] != traceID || w.Header().Get("X-Request-ID") != traceID {
						t.Fatalf("broken correlation: %+v", got)
					}
					if _, ok := got["recordId"]; ok {
						t.Fatal("dashboard unexpectedly enabled")
					}
					// Only the pre-stream HTTP 429 opens the breaker, as before.
					wantNext := "key-1"
					if tc.name == "HTTP limit" {
						wantNext = ""
					}
					if pool.Routing().Next != wantNext {
						t.Fatal("diagnostics changed breaker behavior")
					}
				})
			}
		}
	}
}

func TestChatDiagnosticsAfterRotation(t *testing.T) {
	for _, dashboard := range []bool{false, true} {
		t.Run(fmt.Sprint(dashboard), func(t *testing.T) {
			logs := captureChatLogs(t)
			cfg := dashboardConfig()
			cfg.DashboardEnabled = dashboard
			cfg.APIKeys = []string{"PRIVATE_KEY_1", "PRIVATE_KEY_2"}
			client := diagnosticClientFunc(func(_ context.Context, creds commandcode.Credentials, _ *commandcode.GenerateRequest) (io.ReadCloser, error) {
				if creds.APIKey == cfg.APIKeys[0] {
					return nil, &commandcode.APIError{Status: 401, Message: "PRIVATE_AUTH_MESSAGE"}
				}
				return io.NopCloser(strings.NewReader(terminalThinking + `{"type":"error","error":{"statusCode":429,"message":"PRIVATE_MESSAGE"}}`)), nil
			})
			pool := keypool.New(client, session.NewStore("seed"), cfg.APIKeys, keypool.BreakerPolicy{})
			h := New(cfg, Deps{Upstream: pool}, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			records := chatFailureLogs(t, logs)
			if len(records) != 1 || records[0]["keyId"] != "key-2" || records[0]["attempts"] != float64(2) || records[0]["upstreamStatus"] != float64(429) {
				t.Fatalf("wrong final key: %+v", records)
			}
			if dashboard {
				recent := dashboardSnapshot(t, h).Recent
				if len(recent) != 1 || records[0]["recordId"] != recent[0].ID || recent[0].Result != "upstream_error" || len(recent[0].Attempts) != 2 {
					t.Fatal("dashboard correlation/accounting changed")
				}
			}
			if pool.Routing().Next != "key-2" {
				t.Fatal("stream error changed routing")
			}
		})
	}
}

func TestChatDiagnosticsConcurrentRequests(t *testing.T) {
	logs := captureChatLogs(t)
	client := diagnosticClientFunc(func(_ context.Context, _ commandcode.Credentials, _ *commandcode.GenerateRequest) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(terminalThinking + `{"type":"error","error":{"statusCode":503,"message":"PRIVATE_MESSAGE"}}`)), nil
	})
	cfg := testConfig()
	pool := keypool.New(client, session.NewStore("seed"), cfg.APIKeys, keypool.BreakerPolicy{})
	h := New(cfg, Deps{Upstream: pool}, nil)
	var wg sync.WaitGroup
	ids := make(chan string, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			ids <- w.Header().Get("X-Request-ID")
		}()
	}
	wg.Wait()
	close(ids)
	want := map[string]bool{}
	for id := range ids {
		if len(id) != 32 || want[id] {
			t.Fatal("duplicate/missing request ID")
		}
		want[id] = true
	}
	records := chatFailureLogs(t, logs)
	if len(records) != len(want) {
		t.Fatalf("lost logs: %d", len(records))
	}
	for _, got := range records {
		id, _ := got["requestId"].(string)
		if !want[id] || got["keyId"] != "key-1" || got["attempts"] != float64(1) {
			t.Fatalf("mixed diagnostics: %+v", got)
		}
		delete(want, id)
	}
}

func TestChatDiagnosticsPassthroughAndUnavailable(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			logs := captureChatLogs(t)
			cfg := testConfig()
			cfg.APIKeys = nil
			if passthrough {
				cfg.AuthMode = config.AuthPassthrough
			}
			client := diagnosticClientFunc(func(_ context.Context, creds commandcode.Credentials, _ *commandcode.GenerateRequest) (io.ReadCloser, error) {
				if creds.APIKey != "PRIVATE_CALLER_KEY" {
					t.Fatal("wrong passthrough credential")
				}
				return nil, &commandcode.APIError{Status: 503, Message: "PRIVATE_ERROR"}
			})
			pool := keypool.New(client, session.NewStore("seed"), cfg.APIKeys, keypool.BreakerPolicy{})
			h := New(cfg, Deps{Upstream: pool}, nil)
			r := authedReq(t, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
			if passthrough {
				r.Header.Set("Authorization", "Bearer PRIVATE_CALLER_KEY")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			records := chatFailureLogs(t, logs)
			if w.Code != 503 || len(records) != 1 {
				t.Fatalf("status=%d logs=%+v", w.Code, records)
			}
			keyID, attempts, code := "unselected", 0, "keypool_unavailable"
			if passthrough {
				keyID, attempts, code = "passthrough", 1, "upstream_server_error"
			}
			if records[0]["keyId"] != keyID || records[0]["attempts"] != float64(attempts) || records[0]["code"] != code {
				t.Fatalf("wrong identity: %+v", records)
			}
		})
	}
}
