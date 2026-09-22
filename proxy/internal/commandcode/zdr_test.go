package commandcode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestZDRHeadersOnAllUpstreamRoutes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				values := r.Header.Values("x-cmd-zdr")
				if enabled && (len(values) != 1 || values[0] != "1") || !enabled && len(values) != 0 {
					t.Errorf("%s ZDR headers = %v, enabled = %v", r.URL.Path, values, enabled)
				}
				if r.Header.Get("Authorization") != "Bearer k1" {
					t.Errorf("%s missing auth", r.URL.Path)
				}
				if r.URL.Path == "/provider/v1/models" {
					_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
				} else {
					_, _ = io.WriteString(w, `{}`)
				}
			}))
			defer srv.Close()
			client := NewClient(srv.URL, func() string { return "test" }, enabled, srv.Client())
			ctx := t.Context()
			body, err := client.Generate(ctx, Credentials{APIKey: "k1"}, &GenerateRequest{})
			if err != nil {
				t.Fatal(err)
			}
			_ = body.Close()
			if err := client.Whoami(ctx, "k1"); err != nil {
				t.Fatal(err)
			}
			if _, err := client.ProviderModels(ctx, "k1"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := client.Billing(ctx, "k1"); err != nil {
				t.Fatal(err)
			}
			for _, call := range []func(context.Context, string, any) error{client.RecordFingerprint, client.RecordLifecycleEvent} {
				if err := call(ctx, "k1", nil); err != nil {
					t.Fatal(err)
				}
			}
			if got := calls.Load(); got != 7 {
				t.Fatalf("upstream calls = %d, want 7", got)
			}
		})
	}
}

func TestParseZDRPolicyError(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"CMD_ZDR_NO_PROVIDERS","message":"No ZDR route"}}`,
		`{"error":{"type":"cmd_zdr_no_providers","message":"No ZDR route"}}`,
		`{"error":{"code":"CMD_ZDR_NO_PROVIDERS"}}`,
		`{"error":{"type":"cmd_zdr_no_providers"}}`,
	} {
		response := &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(body))}
		err := parseAPIError(response)
		_ = response.Body.Close()
		ae, ok := err.(*APIError)
		if !ok || !ae.IsZDRUnavailable() || ae.Message == "" {
			t.Fatalf("policy error lost: %v for %s", err, body)
		}
	}
}

// This envelope was observed from /alpha/generate with Qwen and ZDR enabled.
func TestParseLiveZDRRejection(t *testing.T) {
	body := `{"error":{"code":"CMD_ZDR_NO_PROVIDERS","status":422,"message":"This model has no zero-data-retention upstream. Disable CMD_ZDR or choose a different model.","docs":"https://commandcode.ai/docs/reference/errors/cmd_zdr_no_providers"}}`
	response := &http.Response{StatusCode: 422, Body: io.NopCloser(strings.NewReader(body))}
	err := parseAPIError(response)
	_ = response.Body.Close()
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 422 || !ae.IsZDRUnavailable() {
		t.Fatalf("live ZDR rejection not recognized: %v", err)
	}
}
