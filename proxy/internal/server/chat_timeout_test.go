package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/metrics"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stream"
)

type chatUpstreamFunc func(context.Context) (io.ReadCloser, error)

func (f chatUpstreamFunc) Generate(ctx context.Context, _ string, _ commandcode.CallMeta, _ *commandcode.GenerateRequest) (io.ReadCloser, error) {
	return f(ctx)
}

type readStep struct {
	delay time.Duration
	data  string
	err   error
}

// scriptedChatBody models network fragments, pauses, and reads that contain
// both data and an error. It honors cancellation like an HTTP response body.
type scriptedChatBody struct {
	ctx   context.Context
	steps []readStep
}

func (b *scriptedChatBody) Read(p []byte) (int, error) {
	if len(b.steps) == 0 {
		return 0, io.EOF
	}
	s := &b.steps[0]
	if s.delay > 0 {
		timer := time.NewTimer(s.delay)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case <-timer.C:
		}
		s.delay = 0
	}
	if b.ctx.Err() != nil {
		return 0, b.ctx.Err()
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	if s.data != "" {
		return n, nil
	}
	err := s.err
	b.steps = b.steps[1:]
	return n, err
}

func (b *scriptedChatBody) Close() error { return nil }

func TestChatNetworkActivityKeepsAlive(t *testing.T) {
	for _, kind := range []string{"fragments", "heartbeats", "events"} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", kind, streamed), func(t *testing.T) {
				t.Parallel()
				cfg := dashboardConfig()
				cfg.StreamIdleTimeout, cfg.NonStreamIdleTimeout = 200*time.Millisecond, 200*time.Millisecond
				up := chatUpstreamFunc(func(ctx context.Context) (io.ReadCloser, error) {
					var steps []readStep
					if kind == "fragments" {
						steps = append(steps, readStep{data: `{"type":"text-delta","text":"`})
					}
					for range 8 {
						data := ": keepalive\n\n"
						switch kind {
						case "fragments":
							data = "a"
						case "events":
							data = terminalThinking
						}
						steps = append(steps, readStep{delay: 50 * time.Millisecond, data: data})
					}
					if kind == "fragments" {
						steps = append(steps, readStep{data: "\"}\n"})
					}
					steps = append(steps, readStep{data: terminalText + terminalFinish("stop")})
					return &scriptedChatBody{ctx: ctx, steps: steps}, nil
				})
				h := New(cfg, Deps{Upstream: up}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[]}`, streamed)))
				if rec.Code != 200 || strings.Contains(rec.Body.String(), `"error":`) || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
					t.Fatalf("active upstream canceled: %d %s", rec.Code, rec.Body.String())
				}
				if out := dashboardSnapshot(t, h); out.Totals.Succeeded != 1 || out.Totals.Failed != 0 {
					t.Fatalf("incorrect activity accounting: %+v", out.Totals)
				}
			})
		}
	}
}

func TestChatIdleTimeoutClassification(t *testing.T) {
	for _, phase := range []string{"headers", "first read", "midstream", "partial tool", "zero reads", "after finish"} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", phase, streamed), func(t *testing.T) {
				t.Parallel()
				cfg := dashboardConfig()
				cfg.StreamIdleTimeout, cfg.NonStreamIdleTimeout = 100*time.Millisecond, 100*time.Millisecond
				up := chatUpstreamFunc(func(ctx context.Context) (io.ReadCloser, error) {
					if phase == "headers" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					var steps []readStep
					if phase == "midstream" || phase == "after finish" {
						steps = append(steps, readStep{data: terminalThinking})
					}
					if phase == "after finish" {
						steps = append(steps, readStep{data: terminalFinish("length")})
					}
					if phase == "partial tool" {
						steps = append(steps, readStep{data: `{"type":"tool-call","input":`})
					}
					if phase == "zero reads" {
						for range 20 {
							steps = append(steps, readStep{delay: 20 * time.Millisecond})
						}
					} else {
						steps = append(steps, readStep{delay: time.Second})
					}
					steps = append(steps, readStep{data: terminalText + terminalFinish("stop")})
					return &scriptedChatBody{ctx: ctx, steps: steps}, nil
				})
				counters := metrics.New()
				h := New(cfg, Deps{Upstream: up, Metrics: counters}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[]}`, streamed)))
				started := streamed && (phase == "midstream" || phase == "after finish")
				assertChatIOFailure(t, rec, started, 504, "upstream_idle_timeout")
				out := dashboardSnapshot(t, h)
				if out.Totals.Failed != 1 || out.Recent[0].Result != "timeout" || !strings.Contains(counters.Render(), `result="timeout"`) {
					t.Fatalf("idle timeout accounting: %+v", out)
				}
			})
		}
	}
}

func assertChatIOFailure(t *testing.T, rec *httptest.ResponseRecorder, started bool, status int, code string) {
	t.Helper()
	if started {
		status = 200
	}
	body := rec.Body.String()
	if rec.Code != status || !strings.Contains(body, `"code":"`+code+`"`) || !strings.Contains(body, `"type":"server_error"`) || strings.Contains(body, `"finish_reason":"`) {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	if started && strings.Count(body, stream.DoneSSE) != 1 {
		t.Fatalf("missing/duplicate DONE: %s", body)
	}
}

func TestChatTransportErrorsAreNotIdleTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unexpected EOF", io.ErrUnexpectedEOF, 502, "upstream_connection_error"},
		{"reset", errors.New("synthetic private connection failure"), 502, "upstream_connection_error"},
		{"unrelated cancellation", context.Canceled, 502, "upstream_connection_error"},
		{"deadline", context.DeadlineExceeded, 504, "upstream_timeout"},
		{"network timeout", &net.DNSError{Err: "synthetic private timeout", IsTimeout: true}, 504, "upstream_timeout"},
	} {
		for _, phase := range []string{"headers", "first read", "midstream", "partial tool", "after finish"} {
			for _, streamed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, phase, streamed), func(t *testing.T) {
					up := &stubUpstream{readErr: tc.err}
					if phase == "headers" {
						up.err = tc.err
					}
					if phase == "midstream" || phase == "after finish" {
						up.ndjson = terminalThinking
					}
					if phase == "after finish" {
						up.ndjson += terminalFinish("length")
					}
					if phase == "partial tool" {
						up.ndjson = `{"type":"tool-call","input":`
					}
					counters := metrics.New()
					h := New(dashboardConfig(), Deps{Upstream: up, Metrics: counters}, nil)
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[]}`, streamed)))
					assertChatIOFailure(t, rec, streamed && (phase == "midstream" || phase == "after finish"), tc.status, tc.code)
					if strings.Contains(rec.Body.String(), "private connection") || strings.Contains(rec.Body.String(), "private timeout") || strings.Contains(rec.Body.String(), "reducing context") {
						t.Fatal("raw transport error or misleading context hint exposed")
					}
					out := dashboardSnapshot(t, h)
					want := "upstream_connection_error"
					if tc.status == 504 {
						want = "timeout"
					}
					if out.Totals.Failed != 1 || out.Recent[0].Result != want || !strings.Contains(counters.Render(), `result="`+want+`"`) {
						t.Fatalf("transport accounting: %+v", out)
					}
				})
			}
		}
	}
}

type slowChatWriter struct {
	*httptest.ResponseRecorder
	delayed bool
}

func (w *slowChatWriter) Write(p []byte) (int, error) {
	if !w.delayed {
		w.delayed = true
		time.Sleep(300 * time.Millisecond)
	}
	return w.ResponseRecorder.Write(p)
}

func (w *slowChatWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestChatSlowDownstreamIsNotUpstreamIdle(t *testing.T) {
	cfg := testConfig()
	cfg.StreamIdleTimeout = 100 * time.Millisecond
	up := chatUpstreamFunc(func(ctx context.Context) (io.ReadCloser, error) {
		return &scriptedChatBody{ctx: ctx, steps: []readStep{{data: terminalText}, {data: terminalFinish("stop")}}}, nil
	})
	h := New(cfg, Deps{Upstream: up}, nil)
	w := &slowChatWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", `{"model":"synthetic","stream":true,"messages":[]}`))
	if w.Code != 200 || strings.Contains(w.Body.String(), `"error":`) || !strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("downstream stall canceled upstream: %d %s", w.Code, w.Body.String())
	}
}

func TestChatDownstreamCancellation(t *testing.T) {
	for _, phase := range []string{"headers", "first read", "midstream"} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", phase, streamed), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				up := chatUpstreamFunc(func(upstreamCtx context.Context) (io.ReadCloser, error) {
					if phase == "headers" {
						cancel()
						return nil, upstreamCtx.Err()
					}
					var prefix io.Reader = strings.NewReader("")
					if phase == "midstream" {
						prefix = strings.NewReader(terminalThinking)
					}
					return io.NopCloser(io.MultiReader(prefix, cancelChatReader{cancel: cancel, ctx: upstreamCtx})), nil
				})
				counters := metrics.New()
				h := New(dashboardConfig(), Deps{Upstream: up, Metrics: counters}, nil)
				rec := httptest.NewRecorder()
				req := authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"synthetic","stream":%t,"messages":[]}`, streamed)).WithContext(ctx)
				h.ServeHTTP(rec, req)
				if strings.Contains(rec.Body.String(), `"error":`) || strings.Contains(rec.Body.String(), `"finish_reason":"`) || strings.Contains(rec.Body.String(), stream.DoneSSE) {
					t.Fatalf("wrote a completion/error after cancellation: %s", rec.Body.String())
				}
				out := dashboardSnapshot(t, h)
				if out.Totals.Canceled != 1 || out.Totals.Failed != 0 || !strings.Contains(counters.Render(), `result="canceled"`) {
					t.Fatalf("cancellation accounting: %+v", out)
				}
			})
		}
	}
}

type cancelChatReader struct {
	cancel context.CancelFunc
	ctx    context.Context
}

func (r cancelChatReader) Read([]byte) (int, error) {
	r.cancel()
	return 0, r.ctx.Err()
}

func TestChatIdleCancelsHTTPWithoutRotatingKeys(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(fmt.Sprintf("headers=%t", headers), func(t *testing.T) {
			var calls atomic.Int32
			disconnected := make(chan struct{}, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				// Consume the request so HTTP/1 cancellation is observable even
				// when the response headers have not been written yet.
				_, _ = io.Copy(io.Discard, r.Body)
				if headers {
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				disconnected <- struct{}{}
			}))
			defer upstream.Close()
			cfg := dashboardConfig()
			cfg.StreamIdleTimeout = 200 * time.Millisecond
			sessions := session.NewStore("synthetic")
			client := commandcode.NewClient(upstream.URL, func() string { return "test" }, false, upstream.Client())
			pool := keypool.New(client, sessions, cfg.APIKeys, keypool.BreakerPolicy{})
			h := New(cfg, Deps{Upstream: pool, Sessions: sessions}, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", `{"model":"synthetic","stream":true,"messages":[]}`))
			assertChatIOFailure(t, rec, false, 504, "upstream_idle_timeout")
			select {
			case <-disconnected:
			case <-time.After(2 * time.Second):
				t.Fatal("idle request did not close the upstream HTTP connection")
			}
			if calls.Load() != 1 {
				t.Fatalf("timed out request was replayed %d times", calls.Load())
			}
			for _, key := range pool.Routing().Keys {
				if !key.Available || key.Probing || key.RetryAt != nil {
					t.Fatal("idle timeout poisoned a key breaker")
				}
			}
		})
	}
}

func TestChatIdleIgnoresStaleCallback(t *testing.T) {
	watch := newChatIdle(t.Context(), time.Minute)
	defer watch.close()
	watch.begin()
	_ = watch.end(nil)
	watch.begin()
	// Simulate an already queued callback from the preceding I/O operation.
	watch.expire()
	if watch.ctx.Err() != nil {
		t.Fatal("old callback canceled a fresh read")
	}
	_ = watch.end(nil)
	watch.expire()
	if watch.ctx.Err() != nil {
		t.Fatal("old callback canceled a paused reader")
	}
	watch.close()
	watch.expire()
	if !errors.Is(context.Cause(watch.ctx), context.Canceled) {
		t.Fatal("late callback replaced normal cleanup with an idle timeout")
	}
}
