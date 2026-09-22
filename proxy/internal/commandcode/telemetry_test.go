package commandcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

type telemetryTransport func(*http.Request) (*http.Response, error)

func (f telemetryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCLISessionPinsInferenceVersion(t *testing.T) {
	var versions atomic.Int32
	var requests int
	hc := &http.Client{Transport: telemetryTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		var event struct {
			Type     string            `json:"eventType"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "cli_session_exists" || event.Metadata["sessionId"] != "sess_0123456789abcdef" ||
			event.Metadata["mode"] != "non-interactive" || event.Metadata["os"] != "linux-x64" {
			t.Fatalf("unexpected event: %+v", event)
		}
		wantVersion := fmt.Sprintf("v%d", requests)
		if event.Metadata["cliVersion"] != wantVersion || r.Header.Get("x-command-code-version") != wantVersion {
			t.Fatalf("version snapshot mismatch: body=%s header=%s", event.Metadata["cliVersion"], r.Header.Get("x-command-code-version"))
		}
		if r.URL.Path != "/alpha/lifecycle-events" || r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("x-cmd-zdr") != "1" {
			t.Fatal("telemetry route/auth/ZDR changed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	client := NewClient("https://synthetic.invalid", func() string {
		return fmt.Sprintf("v%d", versions.Add(1))
	}, true, hc)
	client.SetKeyObserver(func(string, string) func() { t.Error("telemetry must not trigger its own observer"); return nil })
	for i := range 2 {
		if err := client.RecordCLISession(t.Context(), "key", "sess_0123456789abcdef", "linux-x64", fmt.Sprintf("v%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if versions.Load() != 0 {
		t.Fatal("event must retain the inference version even if the tracker has refreshed")
	}
}

func TestKeyObserverOnlyAcceptedInference(t *testing.T) {
	var observed []string
	hc := &http.Client{Transport: telemetryTransport(func(r *http.Request) (*http.Response, error) {
		body := `{}`
		status := 200
		if r.Header.Get("Authorization") == "Bearer rejected-key" {
			status = 401
		}
		switch r.URL.Path {
		case "/provider/v1/models":
			body = `{"data":[{"id":"m"}]}`
		case "/provider/v1/systemone":
			body = systemOneAnswer
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	client := NewClient("https://synthetic.invalid", func() string { return "1.62.1" }, false, hc)
	client.SetKeyObserver(func(key, version string) func() {
		observed = append(observed, key)
		if version != "1.62.1" {
			t.Error("observer lost inference version")
		}
		return nil
	})
	ctx := t.Context()
	body, err := client.Generate(ctx, Credentials{APIKey: "chat-key"}, &GenerateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	req, err := ParseSystemOneRequest([]byte(systemOneNoul), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.SystemOne(ctx, Credentials{APIKey: "jev-key"}, req, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err = client.Whoami(ctx, "probe-key"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.ProviderModels(ctx, "models-key"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = client.Billing(ctx, "billing-key"); err != nil {
		t.Fatal(err)
	}
	if err = client.RecordFingerprint(ctx, "telemetry-key", nil, "1.62.1"); err != nil {
		t.Fatal(err)
	}
	if err = client.RecordCLISession(ctx, "telemetry-key", "session", "linux-x64", "1.62.1"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Generate(ctx, Credentials{APIKey: "rejected-key"}, &GenerateRequest{}); err == nil {
		t.Fatal("rejected request succeeded")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = client.Generate(canceled, Credentials{APIKey: "canceled-key"}, &GenerateRequest{}); err == nil {
		t.Fatal("canceled request succeeded")
	}
	want := []string{"chat-key", "jev-key"}
	if !slices.Equal(observed, want) {
		t.Fatalf("observed %v, want %v", observed, want)
	}
}

type terminalBody struct{ err error }

func (b terminalBody) Read(p []byte) (int, error) { return copy(p, "last-event"), b.err }
func (terminalBody) Close() error                 { return nil }

func TestInferenceActivityReleasedAfterBody(t *testing.T) {
	for _, terminal := range []error{io.EOF, io.ErrUnexpectedEOF, nil} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			active, finished := 0, 0
			hc := &http.Client{Transport: telemetryTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: terminalBody{terminal}, Header: make(http.Header)}, nil
			})}
			client := NewClient("https://synthetic.invalid", func() string { return "v1" }, false, hc)
			client.SetKeyObserver(func(string, string) func() {
				active++
				return func() { active--; finished++ }
			})
			body, err := client.Generate(t.Context(), Credentials{APIKey: "key"}, &GenerateRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if active != 1 || finished != 0 {
				t.Fatal("response body was not marked active")
			}
			buf := make([]byte, 32)
			n, err := body.Read(buf)
			if string(buf[:n]) != "last-event" || err != terminal {
				t.Fatal("activity tracking changed final data/error")
			}
			if terminal != nil && active != 0 || terminal == nil && active != 1 {
				t.Fatal("wrong body activity lifetime")
			}
			_ = body.Close()
			_ = body.Close()
			if active != 0 || finished != 1 {
				t.Fatal("body activity must release exactly once")
			}
		})
	}
}
