package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/metrics"
)

func TestChatCacheWriteAccounting(t *testing.T) {
	event := func(value string) string {
		return fmt.Sprintf("{\"type\":\"cache-write-tokens\",\"cacheWriteTokens\":%s}\n", value)
	}
	for _, tc := range []struct {
		name, total, before, after string
		want                       int
	}{
		{"nested total", "40", "", "", 40},
		{"missing total counter", "", "", event("21"), 21},
		{"zero total counter", "0", "", event("21"), 21},
		{"before finish", "0", event("21"), "", 21},
		{"total wins", "40", event("21"), event("99"), 40},
		{"last valid wins", "0", event("21"), event("22") + event("22") + event("37"), 37},
		{"valid zero replaces event", "0", event("21"), event("0"), 0},
		{"invalid event ignored", "0", event("21"), event("null") + event("-1") + event(`"99"`) + event("1e400"), 21},
		{"invalid nested counter", `"bad"`, event("21"), "", 21},
		{"invalid nested only", "-1", "", "", 0},
		{"no cache writes", "", "", "", 0},
	} {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streamed), func(t *testing.T) {
				total := ""
				if tc.total != "" {
					total = `,"cacheWriteTokens":` + tc.total
				}
				ndjson := `{"type":"text-delta","text":"ok"}` + "\n" + tc.before +
					fmt.Sprintf("{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":100,\"outputTokens\":20,\"inputTokenDetails\":{\"cacheReadTokens\":80%s}}}\n", total) + tc.after
				stats := metrics.New()
				h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: ndjson}, Metrics: stats}, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, authedReq(t, "POST", "/v1/chat/completions",
					fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, streamed)))
				assertChatContent(t, rec, streamed, "ok")
				body := rec.Body.String()
				if !strings.Contains(body, `"cached_tokens":80`) || !strings.Contains(body, `"total_tokens":120`) || strings.Contains(body, "cacheWrite") || strings.Contains(body, "cache_write") {
					t.Fatalf("cache writes changed downstream usage: %s", body)
				}
				counters := stats.Render()
				if !strings.Contains(counters, `commandcode_proxy_tokens_total{kind="cached"} 80`) {
					t.Fatalf("cache reads lost: %s", counters)
				}
				if tc.want == 0 {
					if strings.Contains(counters, `kind="cache_write"`) {
						t.Fatalf("unexpected cache write counter: %s", counters)
					}
				} else if !strings.Contains(counters, fmt.Sprintf("commandcode_proxy_tokens_total{kind=\"cache_write\"} %d\n", tc.want)) {
					t.Fatalf("cache write counter mismatch: %s", counters)
				}
			})
		}
	}
}
