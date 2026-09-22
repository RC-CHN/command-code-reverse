package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

func TestZDRPolicyAcrossAuthModes(t *testing.T) {
	for _, mode := range []config.AuthMode{config.AuthManaged, config.AuthPassthrough} {
		for _, field := range []string{"code", "type"} {
			for _, upstreamStatus := range []int{400, 403, 422, 429} {
				t.Run(string(mode)+"/"+field+"/"+strconv.Itoa(upstreamStatus), func(t *testing.T) {
					var calls atomic.Int32
					var reject atomic.Bool
					reject.Store(true)
					wantKey := "pool-key-1"
					if mode == config.AuthPassthrough {
						wantKey = "caller-key"
					}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.Header.Get("x-cmd-zdr") != "1" {
							t.Error("ZDR requirement was dropped")
						}
						if r.Header.Get("Authorization") != "Bearer "+wantKey {
							t.Error("wrong upstream credential or key rotated")
						}
						if reject.Load() {
							value := "CMD_ZDR_NO_PROVIDERS"
							if field == "type" {
								value = "cmd_zdr_no_providers"
							}
							w.WriteHeader(upstreamStatus)
							_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
								field: value, "message": "No zero-data-retention route for this model",
							}})
							return
						}
						_, _ = io.WriteString(w, `{"type":"text-delta","text":"ok"}`+"\n"+
							`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":2}}`+"\n")
					}))
					defer upstream.Close()
					cfg := testConfig()
					cfg.AuthMode, cfg.ZDR = mode, true
					client := commandcode.NewClient(upstream.URL, func() string { return "test" }, cfg.ZDR, upstream.Client())
					sessions := session.NewStore("test-seed")
					pool := keypool.New(client, sessions, []string{"pool-key-1", "pool-key-2"}, keypool.BreakerPolicy{})
					handler := New(cfg, Deps{Upstream: pool, Sessions: sessions}, nil)
					run := func() *httptest.ResponseRecorder {
						req := authedReq(t, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
						if mode == config.AuthPassthrough {
							req.Header.Set("Authorization", "Bearer caller-key")
						}
						req.Header.Set("x-cmd-zdr", "0")
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, req)
						return rec
					}
					rec := run()
					if rec.Code != 403 || calls.Load() != 1 || rec.Header().Get("Retry-After") != "" {
						t.Fatalf("status=%d calls=%d headers=%v body=%s", rec.Code, calls.Load(), rec.Header(), rec.Body.String())
					}
					assertErrorBody(t, rec, "zdr_error", "CMD_ZDR_NO_PROVIDERS")
					reject.Store(false)
					rec = run()
					if rec.Code != 200 || calls.Load() != 2 {
						t.Fatalf("key unusable after ZDR rejection: status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
					}
				})
			}
		}
	}
}
