package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

func TestChatIOErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind string
	}{
		{nil, "none"},
		{errUpstreamIdle, "idle_timeout"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{context.Canceled, "canceled"},
		{io.ErrUnexpectedEOF, "unexpected_eof"},
		{io.EOF, "eof"},
		{&net.OpError{Op: "PRIVATE_OPERATION", Err: &os.SyscallError{Syscall: "PRIVATE_SYSCALL", Err: syscall.ECONNRESET}}, "connection_reset"},
		{syscall.ECONNABORTED, "connection_aborted"},
		{syscall.ECONNREFUSED, "connection_refused"},
		{syscall.EPIPE, "broken_pipe"},
		{net.ErrClosed, "connection_closed"},
		{io.ErrClosedPipe, "connection_closed"},
		{&net.DNSError{Name: "PRIVATE_HOST", Err: "PRIVATE_ERROR", IsTimeout: true}, "network_timeout"},
		{&net.DNSError{Name: "PRIVATE_HOST", Err: "PRIVATE_ERROR"}, "dns_error"},
		{&tls.CertificateVerificationError{Err: errors.New("PRIVATE_CERTIFICATE")}, "tls_error"},
		{tls.RecordHeaderError{Msg: "PRIVATE_RECORD"}, "tls_error"},
		{errors.New("PRIVATE_UNEXPECTED_EOF http2: stream error"), "other"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			err := tc.err
			if err != nil {
				err = fmt.Errorf("PRIVATE_URL_AND_KEY: %w", err)
			}
			kind, code := classifyChatIOError(err)
			if kind != tc.kind || code != "" {
				t.Fatalf("kind=%q code=%q; want %q", kind, code, tc.kind)
			}
		})
	}
}

func TestChatStreamProgressDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, data, lastEvent, ioKind, finishReason string
		readErr                                     error
		stall, finish, sameRead                     bool
		pending, events                             int
	}{
		{name: "thinking reset", data: terminalThinking, lastEvent: "reasoning-delta", ioKind: "connection_reset", readErr: fmt.Errorf("PRIVATE_URL: %w", syscall.ECONNRESET), events: 1},
		{name: "partial tool", data: terminalThinking + terminalToolInput("tool-input-start", "PRIVATE_ID"), lastEvent: "tool-input-start", ioKind: "unexpected_eof", readErr: io.ErrUnexpectedEOF, pending: 1, events: 2},
		{name: "finish and unexpected EOF in one read", data: terminalText + strings.TrimSpace(terminalFinish("stop")), lastEvent: "finish", ioKind: "unexpected_eof", readErr: io.ErrUnexpectedEOF, sameRead: true, finish: true, finishReason: "stop", events: 2},
		{name: "idle before first byte", lastEvent: "none", ioKind: "idle_timeout", stall: true},
		{name: "idle midstream", data: terminalThinking, lastEvent: "reasoning-delta", ioKind: "idle_timeout", stall: true, events: 1},
		{name: "idle after finish and metadata", data: terminalText + terminalFinish("stop") + "{\"type\":\"provider-metadata\",\"PRIVATE_METADATA\":\"PRIVATE_VALUE\"}\n", lastEvent: "provider-metadata", ioKind: "idle_timeout", stall: true, finish: true, finishReason: "stop", events: 3},
		{name: "unknown event", data: "{\"type\":\"PRIVATE_EVENT\"}\n", lastEvent: "unknown", ioKind: "eof", events: 1},
		{name: "unknown finish reason", data: terminalFinish("PRIVATE_FINISH"), lastEvent: "finish", ioKind: "none", finish: true, finishReason: "unknown", events: 1},
	} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streamed), func(t *testing.T) {
				logs := captureChatLogs(t)
				cfg := testConfig()
				cfg.StreamIdleTimeout, cfg.NonStreamIdleTimeout = 60*time.Millisecond, 60*time.Millisecond
				up := chatUpstreamFunc(func(ctx context.Context) (io.ReadCloser, error) {
					steps := []readStep{{data: tc.data}, {err: tc.readErr}}
					if tc.sameRead {
						steps = []readStep{{data: tc.data, err: tc.readErr}}
					}
					if tc.stall {
						steps[1].delay = time.Second
					}
					return &scriptedChatBody{ctx: ctx, steps: steps}, nil
				})
				h := New(cfg, Deps{Upstream: up}, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"m","stream":%t,"messages":[]}`, streamed)))
				records := chatFailureLogs(t, logs)
				if len(records) != 1 {
					t.Fatalf("failure logs=%+v", records)
				}
				got := records[0]
				if got["ioErrorKind"] != tc.ioKind || got["lastEvent"] != tc.lastEvent || got["finishSeen"] != tc.finish || got["pendingToolCalls"] != float64(tc.pending) || got["eventCount"] != float64(tc.events) || got["receivedBytes"] != float64(len(tc.data)) || got["idleTimeoutMs"] != float64(60) {
					t.Fatalf("wrong stream diagnostics: %+v", got)
				}
				if _, ok := got["sinceLastByteMs"]; ok != (tc.data != "") {
					t.Fatalf("invented/missing last-byte timing: %+v", got)
				}
				if _, ok := got["sinceLastEventMs"]; ok != (tc.events > 0) {
					t.Fatalf("invented/missing event timing: %+v", got)
				}
				if _, ok := got["sinceFinishMs"]; ok != tc.finish {
					t.Fatalf("invented/missing finish timing: %+v", got)
				}
				if tc.finish && got["finishReason"] != tc.finishReason {
					t.Fatalf("wrong finish reason: %+v", got)
				}
				if tc.stall && got["ioWaitMs"].(float64) < 50 {
					t.Fatalf("idle wait was not measured: %+v", got)
				}
				if strings.Contains(w.Body.String(), `"finish_reason":"`) {
					t.Fatal("diagnostics turned a failed stream into success")
				}
			})
		}
	}
}

func TestChatDiagnosticsHeaderIdle(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprint(streamed), func(t *testing.T) {
			logs := captureChatLogs(t)
			cfg := testConfig()
			cfg.StreamIdleTimeout, cfg.NonStreamIdleTimeout = 60*time.Millisecond, 80*time.Millisecond
			up := chatUpstreamFunc(func(ctx context.Context) (io.ReadCloser, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})
			h := New(cfg, Deps{Upstream: up}, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"m","stream":%t,"messages":[]}`, streamed)))
			records := chatFailureLogs(t, logs)
			if len(records) != 1 {
				t.Fatalf("logs=%+v", records)
			}
			got := records[0]
			idle := cfg.NonStreamIdleTimeout
			if streamed {
				idle = cfg.StreamIdleTimeout
			}
			if got["stage"] != "generate" || got["ioErrorKind"] != "idle_timeout" || got["ioWaitMs"].(float64) < float64(idle.Milliseconds()-5) || got["idleTimeoutMs"] != float64(idle.Milliseconds()) || got["receivedBytes"] != float64(0) || got["eventCount"] != float64(0) || got["lastEvent"] != "none" || got["finishSeen"] != false {
				t.Fatalf("wrong pre-stream timeout: %+v", got)
			}
			if _, ok := got["sinceLastByteMs"]; ok {
				t.Fatal("header timeout invented response bytes")
			}
		})
	}
}

func TestChatDiagnosticsCountsFragmentsAndHeartbeats(t *testing.T) {
	logs := captureChatLogs(t)
	fragments := []string{`: PRIVATE_HEARTBEAT` + "\n", `{"type":"reasoning-delta","text":"`, `PRIVATE_THINKING"}` + "\n", terminalFinish("length")}
	up := chatUpstreamFunc(func(ctx context.Context) (io.ReadCloser, error) {
		steps := make([]readStep, 0, len(fragments)+1)
		for _, data := range fragments {
			steps = append(steps, readStep{data: data})
		}
		steps = append(steps, readStep{err: io.ErrUnexpectedEOF})
		return &scriptedChatBody{ctx: ctx, steps: steps}, nil
	})
	h := New(testConfig(), Deps{Upstream: up}, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`))
	records := chatFailureLogs(t, logs)
	if len(records) != 1 || records[0]["receivedBytes"] != float64(len(strings.Join(fragments, ""))) || records[0]["eventCount"] != float64(2) || records[0]["lastEvent"] != "finish" || records[0]["finishSeen"] != true || records[0]["finishReason"] != "length" || records[0]["ioErrorKind"] != "unexpected_eof" {
		t.Fatalf("byte activity confused with parsed events: %+v", records)
	}
}

// Release a real HTTP/2 RST_STREAM only once the preceding bytes have reached
// the proxy, so the test deterministically exercises a failure after finish.
type http2DiagnosticBody struct {
	io.ReadCloser
	read chan struct{}
	once sync.Once
}

func (b *http2DiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.once.Do(func() { close(b.read) })
	}
	if err != nil {
		err = fmt.Errorf("PRIVATE_TRANSPORT_URL: %w", err)
	}
	return n, err
}

func TestChatHTTP2ResetDiagnostics(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprint(streamed), func(t *testing.T) {
			logs := captureChatLogs(t)
			read := make(chan struct{})
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					t.Error("test did not negotiate HTTP/2")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, terminalText+terminalFinish("stop"))
				w.(http.Flusher).Flush()
				select {
				case <-read:
					panic(http.ErrAbortHandler)
				case <-r.Context().Done():
				}
			}))
			upstream.EnableHTTP2 = true
			upstream.StartTLS()
			defer upstream.Close()
			cfg := testConfig()
			cc := commandcode.NewClient(upstream.URL, func() string { return "test" }, false, upstream.Client())
			client := diagnosticClientFunc(func(ctx context.Context, creds commandcode.Credentials, req *commandcode.GenerateRequest) (io.ReadCloser, error) {
				body, err := cc.Generate(ctx, creds, req)
				if err != nil {
					return nil, err
				}
				return &http2DiagnosticBody{ReadCloser: body, read: read}, nil
			})
			pool := keypool.New(client, session.NewStore("seed"), cfg.APIKeys, keypool.BreakerPolicy{})
			h := New(cfg, Deps{Upstream: pool}, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"m","stream":%t,"messages":[]}`, streamed)))
			assertChatIOFailure(t, w, streamed, 502, "upstream_connection_error")
			records := chatFailureLogs(t, logs)
			if len(records) != 1 || records[0]["ioErrorKind"] != "http2_stream_error" || records[0]["http2ErrorCode"] != "INTERNAL_ERROR" || records[0]["lastEvent"] != "finish" || records[0]["finishSeen"] != true || records[0]["attempts"] != float64(1) {
				t.Fatalf("HTTP/2 reset lost its cause/terminal state: %+v", records)
			}
			if pool.Routing().Next != "key-1" {
				t.Fatal("diagnostics changed circuit breaker state")
			}
		})
	}
}
