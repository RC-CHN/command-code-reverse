package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Measure the complete relay, discarding downstream bytes to exclude the
// recorder's own full-response buffering from the allocation measurements.
func BenchmarkChatRelay(b *testing.B) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previous)
	ndjson := strings.Repeat(`{"type":"text-delta","text":"0123456789abcdef0123456789abcdef"}`+"\n"+
		`{"type":"reasoning-delta","text":"0123456789abcdef0123456789abcdef"}`+"\n", 1024) +
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":2048}}` + "\n"
	for _, streamed := range []bool{false, true} {
		b.Run(fmt.Sprintf("stream=%t", streamed), func(b *testing.B) {
			h := New(testConfig(), Deps{Upstream: &stubUpstream{ndjson: ndjson}}, nil)
			body := fmt.Sprintf(`{"model":"bench","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, streamed)
			b.ReportAllocs()
			b.SetBytes(64 << 10)
			b.ResetTimer()
			for range b.N {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer proxy-key")
				h.ServeHTTP(&discardResponse{header: make(http.Header)}, req)
			}
		})
	}
}

type discardResponse struct{ header http.Header }

func (w *discardResponse) Header() http.Header       { return w.header }
func (*discardResponse) WriteHeader(int)             {}
func (*discardResponse) Flush()                      {}
func (*discardResponse) Write(p []byte) (int, error) { return len(p), nil }
