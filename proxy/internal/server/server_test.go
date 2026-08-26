package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
)

// stubUpstream replays canned NDJSON or a canned error.
type stubUpstream struct {
	ndjson string
	err    error

	gotHint string
	gotMeta commandcode.CallMeta
	gotReq  *commandcode.GenerateRequest
}

func (u *stubUpstream) Generate(ctx context.Context, hint string, meta commandcode.CallMeta, req *commandcode.GenerateRequest) (io.ReadCloser, error) {
	u.gotHint = hint
	u.gotMeta = meta
	u.gotReq = req
	if u.err != nil {
		return nil, u.err
	}
	return io.NopCloser(strings.NewReader(u.ndjson)), nil
}

func testConfig() *config.Config {
	return &config.Config{
		APIKeys:              []string{"pool-key"},
		AuthMode:             config.AuthManaged,
		ProxyAPIKey:          "proxy-key",
		MaxBodyBytes:         1 << 20,
		MaxTokensClamp:       200000,
		StreamIdleTimeout:    5_000_000_000, // 5s
		NonStreamIdleTimeout: 5_000_000_000,
	}
}

func authedReq(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer proxy-key")
	return req
}

const chatNDJSON = `{"type":"start"}
{"type":"start-step","request":{"body":{}},"warnings":[]}
{"type":"reasoning-start","id":"r-0"}
{"type":"reasoning-delta","id":"r-0","text":"thinking "}
{"type":"text-start","id":"t-0"}
{"type":"reasoning-end","id":"r-0"}
{"type":"text-delta","id":"t-0","text":"Hello"}
{"type":"text-delta","id":"t-0","text":" world"}
{"type":"text-end","id":"t-0"}
{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":8}}
{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":8}}
{"type":"provider-metadata","providerMetadata":{"gateway":{"cost":"0.0001"}}}
`

func TestChatCompletionsStream(t *testing.T) {
	up := &stubUpstream{ndjson: chatNDJSON}
	h := New(testConfig(), Deps{Upstream: up}, nil)

	req := authedReq(t, "POST", "/v1/chat/completions",
		`{"model":"deepseek/deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, rec.Body.String())
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"content":"Hello"`,
		`"content":" world"`,
		`"reasoning_content":"thinking "`,
		`"finish_reason":"stop"`,
		`"cached_tokens":8`,
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q:\n%s", want, body)
		}
	}
	// Internal events must never leak downstream.
	for _, banned := range []string{"start-step", "provider-metadata", "0.0001"} {
		if strings.Contains(body, banned) {
			t.Errorf("stream body leaked %q", banned)
		}
	}
	// Wire shape assertions.
	if up.gotReq.Mode != "agent" || up.gotReq.PermissionMode != "standard" {
		t.Errorf("wire = %+v", up.gotReq)
	}
	if up.gotReq.ThreadID == "" {
		t.Error("ThreadID should be generated")
	}
}

func TestChatCompletionsNonStream(t *testing.T) {
	up := &stubUpstream{ndjson: chatNDJSON}
	h := New(testConfig(), Deps{Upstream: up}, nil)

	req := authedReq(t, "POST", "/v1/chat/completions",
		`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Result().StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", rec.Result().StatusCode, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"object":"chat.completion"`,
		`"content":"Hello world"`,
		`"reasoning_content":"thinking "`,
		`"prompt_tokens":10`,
		`"completion_tokens":5`,
		`"cached_tokens":8`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestChatCompletionsAuth(t *testing.T) {
	up := &stubUpstream{ndjson: chatNDJSON}
	h := New(testConfig(), Deps{Upstream: up}, nil)

	// No key → 401.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != 401 {
		t.Errorf("no-key status = %d", rec.Result().StatusCode)
	}

	// Wrong key → 401.
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != 401 {
		t.Errorf("wrong-key status = %d", rec.Result().StatusCode)
	}
}

func TestChatCompletionsTerminalMarker(t *testing.T) {
	up := &stubUpstream{ndjson: `{"type":"start"}
{"type":"error","error":{"message":"premium_credits_exhausted"}}
`}
	h := New(testConfig(), Deps{Upstream: up}, nil)

	req := authedReq(t, "POST", "/v1/chat/completions",
		`{"model":"anthropic/claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Pre-content terminal marker → JSON 402, not a mystery 429.
	if rec.Result().StatusCode != 402 {
		t.Fatalf("status = %d, body = %s", rec.Result().StatusCode, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "billing_error") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestChatCompletionsUpstreamInsufficientCredits(t *testing.T) {
	up := &stubUpstream{err: &commandcode.APIError{Status: 400, Code: "BAD_REQUEST", Message: "insufficient credits"}}
	h := New(testConfig(), Deps{Upstream: up}, nil)

	req := authedReq(t, "POST", "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Result().StatusCode != 402 {
		t.Fatalf("status = %d", rec.Result().StatusCode)
	}
}

func TestModelsFallbackAndAuth(t *testing.T) {
	up := &stubUpstream{}
	h := New(testConfig(), Deps{Upstream: up}, nil)

	req := authedReq(t, "GET", "/v1/models", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != 200 {
		t.Fatalf("status = %d", rec.Result().StatusCode)
	}
	if !strings.Contains(rec.Body.String(), "deepseek/deepseek-v4-flash") {
		t.Errorf("fallback catalog missing: %s", rec.Body.String())
	}
}

func TestModelsDynamicFetch(t *testing.T) {
	up := &stubUpstream{}
	fetch := func(ctx context.Context, key string) ([]commandcode.ModelInfo, error) {
		// Managed mode: downstream hint must be empty (proxy key never
		// travels upstream); the wiring layer substitutes the pool key.
		if key != "" {
			t.Errorf("managed-mode downstream hint leaked: %q", key)
		}
		return []commandcode.ModelInfo{{ID: "dynamic/model-1", Name: "Dynamic One", ContextLength: 131072}}, nil
	}
	h := New(testConfig(), Deps{Upstream: up, FetchModels: fetch}, nil)

	req := authedReq(t, "GET", "/v1/models", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "dynamic/model-1") {
		t.Errorf("dynamic catalog missing: %s", body)
	}
	if !strings.Contains(body, `"context_length":131072`) || !strings.Contains(body, `"name":"Dynamic One"`) {
		t.Errorf("enriched fields missing: %s", body)
	}
}

func TestModelGetByID(t *testing.T) {
	up := &stubUpstream{}
	h := New(testConfig(), Deps{Upstream: up}, nil)

	// Fallback catalog hit (ID contains a slash — wildcard route must match).
	req := authedReq(t, "GET", "/v1/models/deepseek/deepseek-v4-flash", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", rec.Result().StatusCode, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"deepseek/deepseek-v4-flash"`) {
		t.Errorf("body = %s", rec.Body.String())
	}

	// Unknown model → OpenAI-shaped 404.
	req = authedReq(t, "GET", "/v1/models/nope/not-a-model", "")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != 404 {
		t.Fatalf("status = %d", rec.Result().StatusCode)
	}
	if !strings.Contains(rec.Body.String(), "does not exist") {
		t.Errorf("body = %s", rec.Body.String())
	}

	// Unauthenticated → 401.
	req = httptest.NewRequest("GET", "/v1/models/deepseek/deepseek-v4-flash", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != 401 {
		t.Errorf("unauthenticated model get = %d", rec.Result().StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	h := New(testConfig(), Deps{Upstream: &stubUpstream{}}, nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != 200 {
		t.Errorf("healthz = %d", rec.Result().StatusCode)
	}
}
