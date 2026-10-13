package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stats"
)

func dashboardConfig() *config.Config {
	c := testConfig()
	c.DashboardEnabled, c.DashboardAPIKey, c.DashboardRecent = true, "admin-secret", 3
	c.APIKeys = []string{"private-upstream-1", "private-upstream-2"}
	return c
}

func dashboardGet(h http.Handler, path, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func dashboardSnapshot(t *testing.T, h http.Handler) stats.Snapshot {
	t.Helper()
	r := dashboardGet(h, "/dashboard/api/stats", "admin-secret")
	var data stats.Snapshot
	if r.Code != 200 {
		t.Fatalf("stats status %d: %s", r.Code, r.Body.String())
	}
	if err := json.Unmarshal(r.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDashboardOptInAndSeparateAuth(t *testing.T) {
	if r := dashboardGet(New(testConfig(), Deps{}, nil), "/dashboard/api/stats", "proxy-key"); r.Code != 404 {
		t.Fatal("dashboard enabled by default")
	}
	h := New(dashboardConfig(), Deps{}, nil)
	for _, key := range []string{"", "proxy-key", "private-upstream-1", "admin-secret-extra"} {
		if r := dashboardGet(h, "/dashboard/api/stats", key); r.Code != 401 {
			t.Fatalf("admin auth accepted %q", key)
		}
	}
	for _, path := range []string{"/dashboard/", "/dashboard/app.js", "/dashboard/style.css"} {
		r := dashboardGet(h, path, "")
		if r.Code != 200 || r.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("static asset %s: %d", path, r.Code)
		}
		if strings.Contains(r.Body.String(), "admin-secret") {
			t.Fatal("admin credential embedded in assets")
		}
	}
	if r := dashboardGet(h, "/dashboard/api/stats?endpoint=invalid", "admin-secret"); r.Code != 400 {
		t.Fatal("invalid scope accepted")
	}
	data := dashboardSnapshot(t, h)
	if data.Totals.Requests != 0 {
		t.Fatal("admin polling counted as inference")
	}
}

type dashboardGenerateClient struct{ ndjson string }

func (c dashboardGenerateClient) Generate(_ context.Context, creds commandcode.Credentials, _ *commandcode.GenerateRequest) (io.ReadCloser, error) {
	if creds.APIKey == "private-upstream-1" {
		return nil, &commandcode.APIError{Status: 401, Message: "private-upstream-1 rejected"}
	}
	return io.NopCloser(strings.NewReader(c.ndjson)), nil
}

func TestDashboardTracksActualRotationAndUsage(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprint(streamed), func(t *testing.T) {
			cfg := dashboardConfig()
			pool := keypool.New(dashboardGenerateClient{ndjson: chatNDJSON}, session.NewStore("seed"), cfg.APIKeys, keypool.BreakerPolicy{})
			jev := keypool.NewSystemOne(nil, session.NewStore("seed"), cfg.APIKeys, keypool.BreakerPolicy{}, 1024)
			h := New(cfg, Deps{Upstream: pool, Routing: pool.Routing, JevRouting: jev.Routing}, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authedReq(t, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`, streamed)))
			if w.Code != 200 || streamed && !w.Flushed {
				t.Fatal("stream response changed or not flushed")
			}
			out := dashboardSnapshot(t, h)
			r := out.Recent[0]
			if r.KeyID != "key-2" || len(r.Attempts) != 2 || r.Attempts[0].KeyID != "key-1" || r.Attempts[0].Result != "failed" {
				t.Fatalf("actual route: %+v", r)
			}
			if r.Usage == nil || r.Usage.Input != 10 || r.CostUSD == nil || *r.CostUSD != .0001 || out.Totals.Succeeded != 1 {
				t.Fatalf("usage: %+v", out)
			}
			if out.Keys[0].CostReports != 0 || out.Keys[1].CostReports != 1 || out.Keys[0].InFlight != 0 {
				t.Fatal("cost attributed to failed attempt")
			}
			if pool.Routing().Next != "key-2" || jev.Routing().Next != "key-1" {
				t.Fatal("routing preview changed breaker state or mixed Jev and LLM")
			}
			raw, _ := json.Marshal(out)
			for _, secret := range []string{"PRIVATE_PROMPT", "Hello", "thinking ", "private-upstream-1", "private-upstream-2"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("retained request content/credential %s", secret)
				}
			}
		})
	}
}

func TestDashboardTracksStreamFailureCancellationAndAuth(t *testing.T) {
	for _, tc := range []struct {
		name, ndjson, result string
		cancel               bool
		auth                 string
	}{
		{"truncated", `{"type":"text-delta","text":"partial"}` + "\n", "truncated_response", false, "proxy-key"},
		{"in-band error", `{"type":"text-delta","text":"partial"}` + "\n" + `{"type":"error","error":"upstream failed"}` + "\n", "upstream_error", false, "proxy-key"},
		{"canceled", "", "canceled", true, "proxy-key"},
		{"unauthenticated", "", "http_401", false, "wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(dashboardConfig(), Deps{Upstream: &stubUpstream{ndjson: tc.ndjson}}, nil)
			r := authedReq(t, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			r.Header.Set("Authorization", "Bearer "+tc.auth)
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			h.ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
			out := dashboardSnapshot(t, h)
			if len(out.Recent) != 1 || out.Recent[0].Result != tc.result || out.Totals.Succeeded != 0 || out.Totals.InFlight != 0 {
				t.Fatalf("failure accounting: %+v", out)
			}
		})
	}
}

func TestDashboardMissingCostIsNotZeroCost(t *testing.T) {
	for _, cost := range []string{"", "0", "NaN", "0.1junk", "-1"} {
		n := `{"type":"text-delta","text":"ok"}` + "\n" + `{"type":"finish","totalUsage":{"inputTokens":2,"outputTokens":1}}` + "\n"
		if cost != "" {
			n += fmt.Sprintf("{\"type\":\"provider-metadata\",\"providerMetadata\":{\"gateway\":{\"cost\":%q}}}\n", cost)
		}
		h := New(dashboardConfig(), Deps{Upstream: &stubUpstream{ndjson: n}}, nil)
		h.ServeHTTP(httptest.NewRecorder(), authedReq(t, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		out := dashboardSnapshot(t, h)
		if (out.Recent[0].CostUSD != nil) != (cost == "0") {
			t.Fatalf("missing/invalid cost treated as zero: %q", cost)
		}
	}
}

func TestDashboardJevUsageAndPassthroughIdentity(t *testing.T) {
	cfg := dashboardConfig()
	cfg.AuthMode = config.AuthPassthrough
	h := systemOneHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-caller-key" {
			t.Error("passthrough changed")
		}
		_, _ = io.WriteString(w, jevNoulResponse)
	})
	r := authedReq(t, "POST", "/v1/systemone", jevNoulRequest)
	r.Header.Set("Authorization", "Bearer private-caller-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("jev failed: %s", w.Body.String())
	}
	resp := dashboardGet(h, "/dashboard/api/stats?endpoint=jev", "admin-secret")
	var out stats.Snapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Totals.Requests != 1 || out.Totals.Tokens.Input != 5 || out.Totals.Tokens.Output != 2 || out.Totals.CostReports != 0 || out.Recent[0].KeyID != "passthrough" {
		t.Fatalf("jev stats: %+v", out)
	}
	if strings.Contains(resp.Body.String(), "private-caller-key") || dashboardSnapshot(t, h).Totals.Requests != 0 {
		t.Fatal("identity leak or mixed endpoint stats")
	}
}

func TestDashboardAccountQueriesAreCachedAndUseSelectedKey(t *testing.T) {
	var calls atomic.Int32
	h := New(dashboardConfig(), Deps{FetchUsage: func(_ context.Context, key string) (*commandcode.AccountUsage, error) {
		if key != "private-upstream-2" {
			t.Errorf("wrong billing key: %q", key)
		}
		calls.Add(1)
		return &commandcode.AccountUsage{FetchedAt: time.Now()}, nil
	}}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := dashboardGet(h, "/dashboard/api/keys/key-2/usage", "admin-secret"); r.Code != 200 {
				t.Errorf("billing status: %d", r.Code)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate upstream billing queries: %d", calls.Load())
	}
	if r := dashboardGet(h, "/dashboard/api/keys/key-999/usage", "admin-secret"); r.Code != 404 || calls.Load() != 1 {
		t.Fatal("unknown key triggered upstream fetch")
	}
	if dashboardSnapshot(t, h).Totals.Requests != 0 {
		t.Fatal("billing queries counted as inference")
	}
	cfg := dashboardConfig()
	cfg.AuthMode = config.AuthPassthrough
	if r := dashboardGet(New(cfg, Deps{}, nil), "/dashboard/api/keys/key-1/usage", "admin-secret"); r.Code != 400 {
		t.Fatal("passthrough queried configured credentials")
	}
}
