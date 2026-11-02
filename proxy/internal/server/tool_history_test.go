package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

type toolHistoryTransport func(*http.Request) (*http.Response, error)

func (f toolHistoryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEmptyToolHistoryReachesUpstream(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprint(streamed), func(t *testing.T) {
			calls := 0
			transport := toolHistoryTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				defer func() { _ = r.Body.Close() }()
				if r.Method != "POST" || r.URL.Path != "/alpha/generate" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var sent struct {
					Params struct {
						Messages []struct{ Content []map[string]json.RawMessage }
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Fatal(err)
				}
				if len(sent.Params.Messages) != 5 || len(sent.Params.Messages[3].Content) != 3 {
					t.Fatal("wrong fixture shape")
				}
				part := sent.Params.Messages[3].Content[2]
				status, body := 200, chatNDJSON
				if string(part["input"]) != "{}" {
					// Same failing field as the reported upstream union validation.
					status = 400
					body = `{"error":{"message":"Invalid request error. Validation error: Invalid input: expected nonoptional, received undefined at params.messages[3].content[2].input"}}`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			cfg := testConfig()
			client := commandcode.NewClient("https://upstream.invalid", func() string { return "test" }, false, &http.Client{Transport: transport})
			pool := keypool.New(client, session.NewStore("seed"), cfg.APIKeys, keypool.BreakerPolicy{})
			h := New(cfg, Deps{Upstream: pool}, nil)
			request := fmt.Sprintf(`{"model":"m","stream":%t,"messages":[
				{"role":"user","content":"hello"},
				{"role":"assistant","content":"hello"},
				{"role":"user","content":"check status"},
				{"role":"assistant","reasoning_content":"need tool","content":"checking","tool_calls":[{"id":"call_empty","type":"function","function":{"name":"noop","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_empty","content":"done"}
			]}`, streamed)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", request))
			assertChatContent(t, w, streamed, "Hello world")
			if calls != 1 || pool.Routing().Next != "key-1" {
				t.Fatal("unexpected replay or breaker change")
			}
		})
	}
}
