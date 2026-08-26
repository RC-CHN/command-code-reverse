package keypool

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

type fakeClient struct {
	// per-key behavior: key → error to return (nil = success)
	failWith map[string]error
	calls    []string
}

func (f *fakeClient) Generate(ctx context.Context, creds commandcode.Credentials, req *commandcode.GenerateRequest) (io.ReadCloser, error) {
	f.calls = append(f.calls, creds.APIKey)
	if err := f.failWith[creds.APIKey]; err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(`{"type":"start"}` + "\n")), nil
}

func newTestPool(fc *fakeClient, keys ...string) *Pool {
	return New(fc, session.NewStore("test-secret"), keys, BreakerPolicy{})
}

func TestModelNotInPlanKeepsKeyHealthy(t *testing.T) {
	planErr := &commandcode.APIError{
		Status:  403,
		Code:    "FORBIDDEN",
		Message: "MODEL_NOT_IN_PLAN: Claude Haiku 4.5 available in Pro and above plans",
	}
	fc := &fakeClient{failWith: map[string]error{"k1": planErr}}
	p := newTestPool(fc, "k1", "k2")

	// Spills to k2 within the request (a higher-tier key may succeed).
	if _, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(fc.calls) != 2 || fc.calls[0] != "k1" || fc.calls[1] != "k2" {
		t.Fatalf("calls = %v, want [k1 k2]", fc.calls)
	}

	// k1 must NOT be circuit-broken: the next request starts at k1 again.
	fc.failWith = nil
	if _, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate after plan mismatch: %v", err)
	}
	if fc.calls[2] != "k1" {
		t.Fatalf("k1 was circuit-broken on a plan mismatch, calls = %v", fc.calls)
	}
}

func TestModelNotRecognizedKeepsKeyHealthy(t *testing.T) {
	notFound := &commandcode.APIError{
		Status:  403,
		Code:    "FORBIDDEN",
		Message: "Model/provider not recognized: anthropic:deepseek-v4-pro",
	}
	fc := &fakeClient{failWith: map[string]error{"k1": notFound}}
	p := newTestPool(fc, "k1", "k2")

	// No rotation: every key sees the same model catalog.
	if _, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{}); err == nil {
		t.Fatal("expected the model error to surface")
	}
	if len(fc.calls) != 1 {
		t.Fatalf("calls = %v, want exactly one attempt (no rotation)", fc.calls)
	}

	// And no circuit: the key stays healthy for valid models.
	fc.failWith = nil
	if _, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate after model-not-recognized: %v", err)
	}
	if fc.calls[1] != "k1" {
		t.Fatalf("k1 was circuit-broken on a bad model ID, calls = %v", fc.calls)
	}
}

func TestFillFirstUsesFirstKey(t *testing.T) {
	fc := &fakeClient{}
	p := newTestPool(fc, "k1", "k2", "k3")

	for range 3 {
		if _, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
	for i, k := range fc.calls {
		if k != "k1" {
			t.Fatalf("call %d used %q, want k1 (fill-first)", i, k)
		}
	}
}

func TestSpillOnInsufficientCredits(t *testing.T) {
	fc := &fakeClient{failWith: map[string]error{
		"k1": &commandcode.APIError{Status: 400, Message: "insufficient credits"},
	}}
	p := newTestPool(fc, "k1", "k2")

	if _, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(fc.calls) != 2 || fc.calls[0] != "k1" || fc.calls[1] != "k2" {
		t.Fatalf("calls = %v", fc.calls)
	}

	// k1 is now circuit-broken: next request goes straight to k2.
	fc.calls = nil
	if _, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(fc.calls) != 1 || fc.calls[0] != "k2" {
		t.Fatalf("calls = %v, want [k2] only", fc.calls)
	}
}

func TestAllBroken(t *testing.T) {
	broken := &commandcode.APIError{Status: 400, Message: "insufficient credits"}
	fc := &fakeClient{failWith: map[string]error{"k1": broken, "k2": broken}}
	p := newTestPool(fc, "k1", "k2")

	// First call breaks both keys (spill chain), second finds none healthy.
	_, _ = p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{})
	_, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{})
	if err == nil || !strings.Contains(err.Error(), "circuit-broken") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoRotateOnBadRequest(t *testing.T) {
	fc := &fakeClient{failWith: map[string]error{
		"k1": &commandcode.APIError{Status: 400, Code: "BAD_REQUEST", Message: "invalid mode"},
	}}
	p := newTestPool(fc, "k1", "k2")

	_, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(fc.calls) != 1 {
		t.Fatalf("calls = %v, want no rotation on request-shape errors", fc.calls)
	}
}

func TestBackoffEscalatesOnServerFailures(t *testing.T) {
	fc := &fakeClient{failWith: map[string]error{
		"k1": &commandcode.APIError{Status: 502, Message: "bad gateway"},
	}}
	now := time.Now()
	p := New(fc, session.NewStore("test-secret"), []string{"k1"}, BreakerPolicy{Now: func() time.Time { return now }})

	_, _ = p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{})
	snap := p.Snapshot()
	if !snap[0]["broken"].(bool) {
		t.Fatal("k1 should be broken after 502")
	}

	// After the first backoff window passes, one retry is allowed and fails
	// again → longer backoff.
	now = now.Add(61 * time.Second)
	_, _ = p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{})
	snap = p.Snapshot()
	if snap[0]["consecFail"].(int) != 2 {
		t.Fatalf("consecFail = %v", snap[0]["consecFail"])
	}
}

func TestPassthroughBypassesPool(t *testing.T) {
	fc := &fakeClient{}
	p := newTestPool(fc, "k1")

	if _, err := p.Generate(context.Background(), "downstream-key", "root1", &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if fc.calls[0] != "downstream-key" {
		t.Fatalf("calls = %v", fc.calls)
	}
}

func TestReportTerminalBreaksKey(t *testing.T) {
	fc := &fakeClient{}
	p := newTestPool(fc, "k1", "k2")

	p.ReportTerminal("", commandcode.MarkerPremiumCreditsExhausted)
	snap := p.Snapshot()
	if !snap[0]["broken"].(bool) {
		t.Fatal("first key should be broken after terminal marker")
	}
	if snap[1]["broken"].(bool) {
		t.Fatal("second key should stay healthy")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	fc := &fakeClient{failWith: map[string]error{
		"k1": &commandcode.APIError{Status: 502, Message: "x"},
	}}
	now := time.Now()
	p := New(fc, session.NewStore("test-secret"), []string{"k1"}, BreakerPolicy{Now: func() time.Time { return now }})

	_, _ = p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{})
	delete(fc.failWith, "k1") // upstream recovers
	now = now.Add(61 * time.Second)

	if _, err := p.Generate(context.Background(), "", "root1", &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if snap := p.Snapshot(); snap[0]["consecFail"].(int) != 0 {
		t.Fatalf("consecFail = %v, want reset", snap[0]["consecFail"])
	}
}

var _ = errors.Is // keep errors import if assertion helpers change
