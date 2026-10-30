package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

func TestChatUpstreamRequestTooLargeHTTP(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{413, "<html>synthetic private upstream page</html>"},
		{400, `{"error":{"message":"Request body exceeds the 32 MB limit"}}`},
		{403, `{"error":{"code":"request_too_large","message":"synthetic private payload"}}`},
		{429, `{"error":{"type":"request_too_large","message":"synthetic private payload"}}`},
	} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/stream=%t", tc.status, streamed), func(t *testing.T) {
				var calls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var req commandcode.GenerateRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Errorf("decode upstream request: %v", err)
						return
					}
					if req.Params.Model != "minimax/minimax-m3.1-flash:free" || req.Params.MaxTokens != 77 || req.Params.ReasoningEffort != "high" {
						t.Error("changed downstream model, token budget or effort")
					}
					messages, _ := json.Marshal(req.Params.Messages)
					if !strings.Contains(string(messages), "synthetic reasoning") || !strings.Contains(string(messages), "data:image/png;base64,aGVsbG8=") {
						t.Error("dropped downstream thinking or image")
					}
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer up.Close()
				cfg := dashboardConfig()
				sessions := session.NewStore("synthetic")
				client := commandcode.NewClient(up.URL, func() string { return "test" }, false, up.Client())
				pool := keypool.New(client, sessions, cfg.APIKeys, keypool.BreakerPolicy{})
				h := New(cfg, Deps{Upstream: pool, Sessions: sessions}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"minimax/minimax-m3.1-flash:free","max_tokens":77,"reasoning_effort":"high","stream":%t,"messages":[{"role":"assistant","content":"answer","reasoning_content":"synthetic reasoning"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`, streamed)))
				if rec.Code != 413 || rec.Header().Get("Retry-After") != "" {
					t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
				}
				assertErrorBody(t, rec, "invalid_request_error", "upstream_request_too_large")
				if strings.Contains(rec.Body.String(), "synthetic private") {
					t.Fatal("exposed raw upstream size-error body")
				}
				if calls.Load() != 1 {
					t.Fatalf("request was replayed %d times", calls.Load())
				}
				for _, key := range pool.Routing().Keys {
					if !key.Available || key.Probing || key.RetryAt != nil {
						t.Fatal("request size error poisoned a key")
					}
				}
				if snapshot := dashboardSnapshot(t, h); snapshot.Totals.Failed != 1 || snapshot.Totals.Succeeded != 0 {
					t.Fatalf("incorrect error accounting: %+v", snapshot.Totals)
				}
			})
		}
	}
}
