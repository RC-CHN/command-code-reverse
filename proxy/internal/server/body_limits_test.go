package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
)

// echoRequestUpstream reproduces the start-step echo of the converted image
// request, so the regression exercises both request and response size limits.
type echoRequestUpstream struct {
	gotReq *commandcode.GenerateRequest
}

func (u *echoRequestUpstream) Generate(_ context.Context, _ string, _ commandcode.CallMeta, req *commandcode.GenerateRequest) (io.ReadCloser, error) {
	u.gotReq = req
	event, err := json.Marshal(map[string]any{
		"type":    "start-step",
		"request": map[string]any{"body": req},
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.MultiReader(strings.NewReader(string(event)), strings.NewReader("\n"+
		`{"type":"text-delta","text":"images received"}`+"\n"+
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":2}}`+"\n"))), nil
}

func TestChatCompletionsMultipleLargeImages(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("COMMAND_CODE_API_KEY", "pool-key")
	t.Setenv("PROXY_API_KEY", "proxy-key")
	t.Setenv("AUTH_MODE", "managed")
	t.Setenv("MAX_BODY_BYTES", "")
	defaults, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	imageURL := "data:image/png;base64," + strings.Repeat("A", 6*1024*1024)
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streamed), func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":[{"type":"text","text":"Compare these images"},{"type":"image_url","image_url":{"url":%q}},{"type":"image_url","image_url":{"url":%q}}]}]}`, streamed, imageURL, imageURL)
			if len(body) <= 10*1024*1024 {
				t.Fatal("fixture must exceed the old 10 MiB request limit")
			}
			cfg := testConfig()
			cfg.MaxBodyBytes = defaults.MaxBodyBytes
			up := &echoRequestUpstream{}
			h := New(cfg, Deps{Upstream: up}, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(t, http.MethodPost, "/v1/chat/completions", body))
			assertChatContent(t, rec, streamed, "images received")
			if up.gotReq == nil || len(up.gotReq.Params.Messages) != 1 || len(up.gotReq.Params.Messages[0].Content) != 3 {
				t.Fatal("image content parts were lost")
			}
			for _, part := range up.gotReq.Params.Messages[0].Content[1:] {
				if part.Type != "image" || part.Image != imageURL || part.MimeType != "image/png" {
					t.Fatal("image data was changed while forwarding")
				}
			}
		})
	}
}

func TestChatRequestBodyLimit(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		for _, unknownLength := range []bool{false, true} {
			for _, overLimit := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/unknownLength=%t/overLimit=%t", streamed, unknownLength, overLimit), func(t *testing.T) {
					body := fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":%q}]}`, streamed, strings.Repeat("x", 1024))
					cfg := testConfig()
					cfg.MaxBodyBytes = int64(len(body))
					if overLimit {
						cfg.MaxBodyBytes--
					}
					up := &stubUpstream{ndjson: chatNDJSON}
					h := New(cfg, Deps{Upstream: up}, nil)
					req := authedReq(t, http.MethodPost, "/v1/chat/completions", body)
					if unknownLength {
						req.ContentLength = -1
						req.TransferEncoding = []string{"chunked"}
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if !overLimit {
						assertChatContent(t, rec, streamed, "Hello world")
						return
					}
					if rec.Code != http.StatusRequestEntityTooLarge {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					var failure openAIError
					if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil {
						t.Fatal(err)
					}
					if failure.Error.Type != "invalid_request_error" || !strings.Contains(failure.Error.Message, fmt.Sprint(cfg.MaxBodyBytes)) || !strings.Contains(failure.Error.Message, "MAX_BODY_BYTES") {
						t.Fatalf("unhelpful size error: %s", rec.Body.String())
					}
					if up.gotReq != nil {
						t.Fatal("oversized request reached upstream")
					}
				})
			}
		}
	}
}

func TestChatMalformedJSONRemainsBadRequest(t *testing.T) {
	up := &stubUpstream{ndjson: chatNDJSON}
	h := New(testConfig(), Deps{Upstream: up}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(t, http.MethodPost, "/v1/chat/completions", `{"model":]`))
	if rec.Code != http.StatusBadRequest || up.gotReq != nil {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
