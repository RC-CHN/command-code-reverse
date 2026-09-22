package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

const jevNoulRequest = `{"model":"jev","state":{"value":2},"questions":{"q":{"type":"noul","instructions":"Is value greater than one?"}}}`
const jevNoulResponse = `{"model":"typesafe/jev","answers":{"q":{"type":"noul","noul":0.99}},"usage":{"input_tokens":5,"output_tokens":2},"extra":9007199254740993}`

func TestJevPrimitiveResponses(t *testing.T) {
	for _, tc := range []struct{ name, request, response string }{
		{"noul", jevNoulRequest, jevNoulResponse},
		{"zero probability", jevNoulRequest, `{"answers":{"q":{"type":"noul","noul":0}},"usage":{"output_tokens":0}}`},
		{"zero score", `{"model":"jev","state":{},"questions":{"q":{"type":"score","instructions":"Rate it","criteria":["low","high"]}}}`, `{"answers":{"q":{"type":"score","score":0,"legend":{"0":"low","1":"high"},"probabilities":{"0":1,"1":0},"confidence":1}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := systemOneHandler(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-cmd-zdr") != "" {
					t.Error("ZDR should be off")
				}
				_, _ = io.WriteString(w, tc.response)
			})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/systemone", tc.request))
			if rec.Code != 200 || rec.Body.String() != tc.response {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func systemOneHandler(t *testing.T, cfg *config.Config, handler http.HandlerFunc) http.Handler {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	client := commandcode.NewClient(upstream.URL, func() string { return "1.62.1" }, cfg.ZDR, upstream.Client())
	sessions := session.NewStore("test-seed")
	return New(cfg, Deps{
		SystemOne: keypool.NewSystemOne(client, sessions, []string{"k1", "k2"}, keypool.BreakerPolicy{}, cfg.JevMaxResponseBytes),
		Upstream:  keypool.New(client, sessions, []string{"k1", "k2"}, keypool.BreakerPolicy{}),
		Sessions:  sessions,
	}, nil)
}

func choiceRequest(t *testing.T, count int) string {
	t.Helper()
	criteria := map[string]string{}
	for i := range count {
		criteria[fmt.Sprintf("option_%03d", i)] = "Synthetic option"
	}
	body, err := json.Marshal(map[string]any{
		"model": "jev-latest", "state": json.RawMessage(`{"value":9007199254740993}`),
		"extension": json.RawMessage(`{"value":9007199254740993}`),
		"questions": map[string]any{"pick": map[string]any{"type": "choice", "instructions": "Pick one", "criteria": criteria}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestJevOptionLimitAndAuth(t *testing.T) {
	for _, mode := range []config.AuthMode{config.AuthManaged, config.AuthPassthrough} {
		for _, tc := range []struct {
			count  int
			unlock bool
			code   string
		}{
			{20, false, ""}, {21, false, "jev_choice_options_locked"}, {255, false, "jev_choice_options_locked"},
			{21, true, ""}, {255, true, ""}, {256, true, "jev_choice_options_exceeded"}, {256, false, "jev_choice_options_exceeded"},
		} {
			t.Run(fmt.Sprintf("%s/%d/unlock=%v", mode, tc.count, tc.unlock), func(t *testing.T) {
				cfg := testConfig()
				cfg.AuthMode = mode
				cfg.JevUnlockMaxOptions = tc.unlock
				cfg.ZDR = true
				var calls atomic.Int32
				response := `{"answers":{"pick":{"type":"choice","choice":"option_000","confidence":1}},"extra":9007199254740993}`
				h := systemOneHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					wantKey := "k1"
					if mode == config.AuthPassthrough {
						wantKey = "caller-key"
					}
					if r.URL.Path != "/provider/v1/systemone" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+wantKey || r.Header.Get("x-cmd-zdr") != "1" {
						t.Error("route/auth/ZDR mismatch")
					}
					if r.Header.Get("x-session-id") == "" || r.Header.Get("traceparent") == "" || r.Header.Get("x-taste-learning") != "false" {
						t.Error("missing CLI headers")
					}
					raw, _ := io.ReadAll(r.Body)
					if strings.Count(string(raw), "9007199254740993") != 2 {
						t.Error("large integers or extensions changed")
					}
					var wire struct {
						Model     string
						Questions map[string]struct{ Criteria map[string]string }
					}
					if json.Unmarshal(raw, &wire) != nil || wire.Model != "typesafe/jev" || len(wire.Questions["pick"].Criteria) != tc.count {
						t.Error("wire body mismatch")
					}
					_, _ = io.WriteString(w, response)
				})
				req := authedReq(t, "POST", "/v1/systemone", choiceRequest(t, tc.count))
				if mode == config.AuthPassthrough {
					req.Header.Set("Authorization", "Bearer caller-key")
				}
				req.Header.Set("x-jev-unlock-max-options", "true")
				req.Header.Set("x-cmd-zdr", "0")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if tc.code == "" {
					if rec.Code != 200 || rec.Body.String() != response || calls.Load() != 1 {
						t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
					}
				} else {
					if rec.Code != 422 || calls.Load() != 0 {
						t.Fatalf("rejected request reached upstream: status=%d calls=%d", rec.Code, calls.Load())
					}
					assertErrorBody(t, rec, "invalid_request_error", tc.code)
					if !strings.Contains(rec.Body.String(), `"param":"questions.pick.criteria"`) {
						t.Fatal("missing error field path")
					}
				}
			})
		}
	}
}

func TestJevLocalRejections(t *testing.T) {
	for _, tc := range []struct {
		name, body, path, key, code string
		status                      int
		limit                       int64
	}{
		{"auth", jevNoulRequest, "/v1/systemone", "wrong", "", 401, 1024},
		{"malformed", "{", "/v1/systemone", "proxy-key", "invalid_json", 400, 1024},
		{"trailing", jevNoulRequest + "{}", "/v1/systemone", "proxy-key", "invalid_json", 400, 1024},
		{"too large", jevNoulRequest, "/v1/systemone", "proxy-key", "request_body_too_large", 413, 16},
		{"invalid fields", `{"model":"jev","state":{}}`, "/v1/systemone", "proxy-key", "invalid_request_error", 422, 1024},
		{"wrong endpoint", `{"model":"jev","messages":[]}`, "/v1/chat/completions", "proxy-key", "unsupported_endpoint", 400, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MaxBodyBytes = tc.limit
			var calls atomic.Int32
			h := systemOneHandler(t, cfg, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) })
			req := authedReq(t, "POST", tc.path, tc.body)
			req.Header.Set("Authorization", "Bearer "+tc.key)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
			}
			if tc.code != "" {
				assertErrorBody(t, rec, "invalid_request_error", tc.code)
			}
		})
	}
}

func TestJevUpstreamErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             int
		code, typ          string
		wantStatus         int
		wantType, wantCode string
		attempts           int
		retry              string
	}{
		{"bad request", 400, "invalid_request_error", "", 400, "invalid_request_error", "invalid_request_error", 1, ""},
		{"invalid key", 401, "invalid_api_key", "", 502, "upstream_auth_error", "invalid_api_key", 2, ""},
		{"auth forbidden", 403, "", "authentication_error", 502, "upstream_auth_error", "authentication_error", 2, ""},
		{"credits", 402, "insufficient_credits", "", 402, "billing_error", "insufficient_credits", 2, ""},
		{"upgrade", 403, "upgrade_required", "", 403, "plan_error", "upgrade_required", 2, ""},
		{"permission", 403, "forbidden", "", 403, "permission_error", "forbidden", 1, ""},
		{"unsupported", 400, "unsupported_model", "", 400, "invalid_request_error", "unsupported_model", 1, ""},
		{"not found", 404, "not_found", "", 404, "invalid_request_error", "not_found", 1, ""},
		{"408", 408, "", "", 408, "invalid_request_error", "invalid_request_error", 1, ""},
		{"413", 413, "", "", 413, "invalid_request_error", "invalid_request_error", 1, ""},
		{"415", 415, "", "", 415, "invalid_request_error", "invalid_request_error", 1, ""},
		{"validation", 422, "", "validation_error", 422, "invalid_request_error", "validation_error", 1, ""},
		{"rate limit", 429, "rate_limit_error", "", 429, "rate_limit_error", "rate_limit_error", 2, "45"},
		{"ZDR", 422, "CMD_ZDR_NO_PROVIDERS", "", 403, "zdr_error", "CMD_ZDR_NO_PROVIDERS", 1, ""},
		{"ZDR type", 403, "", "cmd_zdr_no_providers", 403, "zdr_error", "CMD_ZDR_NO_PROVIDERS", 1, ""},
		{"spend cap", 429, "USAGE_EXCEEDED", "", 403, "spend_limit_error", "USAGE_EXCEEDED", 1, ""},
		{"500", 500, "internal", "", 500, "server_error", "internal", 1, ""},
		{"502", 502, "bad_gateway", "", 502, "server_error", "bad_gateway", 1, ""},
		{"503", 503, "unavailable", "", 503, "server_error", "unavailable", 1, "45"},
		{"504", 504, "timeout", "", 504, "server_error", "timeout", 1, ""},
		{"529", 529, "", "overloaded_error", 529, "server_error", "overloaded_error", 1, "45"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			h := systemOneHandler(t, testConfig(), func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "45")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": tc.code, "type": tc.typ, "message": "Synthetic upstream failure", "param": "questions.q", "status": tc.status}})
			})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, "POST", "/v1/systemone", jevNoulRequest))
			if rec.Code != tc.wantStatus || int(calls.Load()) != tc.attempts || rec.Header().Get("Retry-After") != tc.retry {
				t.Fatalf("status=%d calls=%d retry=%s body=%s", rec.Code, calls.Load(), rec.Header().Get("Retry-After"), rec.Body.String())
			}
			assertErrorBody(t, rec, tc.wantType, tc.wantCode)
			if !strings.Contains(rec.Body.String(), "Synthetic upstream failure") || !strings.Contains(rec.Body.String(), `"param":"questions.q"`) {
				t.Fatal("error details lost")
			}
		})
	}
}

func TestJevDeadlineAndDisconnect(t *testing.T) {
	for _, upstreamStatus := range []int{0, 200, 429} {
		for _, disconnect := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/disconnect=%v", upstreamStatus, disconnect), func(t *testing.T) {
				cfg := testConfig()
				cfg.JevTimeout = 100 * time.Millisecond
				started, release := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				h := systemOneHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) > 1 {
						if r.Header.Get("Authorization") != "Bearer k1" {
							t.Error("timeout poisoned key")
						}
						_, _ = io.WriteString(w, jevNoulResponse)
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					if upstreamStatus != 0 {
						w.WriteHeader(upstreamStatus)
						w.(http.Flusher).Flush()
					}
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				})
				defer close(release)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				rec := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					h.ServeHTTP(rec, authedReq(t, "POST", "/v1/systemone", jevNoulRequest).WithContext(ctx))
				}()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("upstream did not start")
				}
				if disconnect {
					cancel()
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("request did not cancel")
				}
				if calls.Load() != 1 {
					t.Fatal("cancellation rotated keys")
				}
				if disconnect {
					if rec.Body.Len() != 0 {
						t.Fatal("wrote response after disconnect")
					}
				} else {
					if rec.Code != 504 {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					assertErrorBody(t, rec, "server_error", "upstream_timeout")
				}
				recovered := httptest.NewRecorder()
				h.ServeHTTP(recovered, authedReq(t, "POST", "/v1/systemone", jevNoulRequest))
				if recovered.Code != 200 || calls.Load() != 2 {
					t.Fatal("key was not reusable after timeout/cancel")
				}
			})
		}
	}
}
