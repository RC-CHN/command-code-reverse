package fingerprint_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/fingerprint"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelemetryAcrossRotationJevAndPassthrough(t *testing.T) {
	type event struct{ key, kind string }
	events := make(chan event, 32)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	hc := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		status, body := 200, `{}`
		switch r.URL.Path {
		case "/alpha/fingerprint/record":
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			var fp fingerprint.Fingerprint
			if err := json.NewDecoder(r.Body).Decode(&fp); err != nil || fp.Thumbmark != "shared-device" {
				t.Errorf("fingerprint not shared across accounts: %v", err)
			}
			events <- event{key, "fingerprint"}
			// A telemetry 429 must not affect either inference circuit breaker.
			status, body = 429, `{"error":{"code":"rate_limited"}}`
		case "/alpha/lifecycle-events":
			events <- event{key, "session"}
		case "/alpha/generate":
			if key == "k1" {
				status, body = 401, `{"error":{"code":"invalid_api_key"}}`
			}
		case "/provider/v1/systemone":
			body = `{"answers":{"q":{"type":"noul","noul":1}}}`
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	client := commandcode.NewClient("https://synthetic.invalid", func() string { return "1.62.1" }, true, hc)
	reporter := fingerprint.NewReporter(t.Context(), client, &fingerprint.Fingerprint{Thumbmark: "shared-device"}, time.Minute, "seed")
	t.Cleanup(reporter.Close)
	client.SetKeyObserver(reporter.Use)
	sessions := session.NewStore("test-seed")
	keys := []string{"k1", "k2", "unused-key"}
	chat := keypool.New(client, sessions, keys, keypool.BreakerPolicy{})
	jev := keypool.NewSystemOne(client, sessions, keys, keypool.BreakerPolicy{}, 1024)
	meta := commandcode.CallMeta{Root: "conversation"}
	done := make(chan error, 1)
	go func() {
		body, err := chat.Generate(t.Context(), "", meta, &commandcode.GenerateRequest{})
		if err == nil {
			_ = body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked telemetry delayed chat/rotation")
	}
	req, err := commandcode.ParseSystemOneRequest([]byte(`{"model":"jev","state":{},"questions":{"q":{"type":"noul","instructions":"?"}}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jev.SystemOne(t.Context(), "", meta, req); err != nil {
		t.Fatal(err)
	}
	body, err := chat.Generate(t.Context(), "passthrough-key", meta, &commandcode.GenerateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	unblock()
	counts := map[event]int{}
	for range 6 {
		select {
		case e := <-events:
			counts[e]++
		case <-time.After(5 * time.Second):
			t.Fatal("missing telemetry")
		}
	}
	// All previous keys remain deduplicated after a telemetry rate limit.
	body, err = chat.Generate(t.Context(), "", meta, &commandcode.GenerateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	reporter.Close()
	for _, key := range []string{"k1", "k2", "passthrough-key"} {
		if counts[event{key, "fingerprint"}] != 1 || counts[event{key, "session"}] != 1 {
			t.Fatalf("missing/duplicate account telemetry: %v", counts)
		}
	}
	if len(events) != 0 || len(counts) != 6 {
		t.Fatal("inactive keys, retries, or telemetry recursively triggered reports")
	}
	if !chat.Snapshot()[0]["broken"].(bool) || chat.Snapshot()[1]["broken"].(bool) {
		t.Fatal("telemetry affected inference breakers")
	}
}
