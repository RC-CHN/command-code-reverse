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

func TestSpendCapDoesNotRotateOrBreakKey(t *testing.T) {
	for _, status := range []int{400, 403, 429} {
		capErr := &commandcode.APIError{Status: status, Code: "USAGE_EXCEEDED", Message: "Org model spend cap reached"}
		fc := &fakeClient{failWith: map[string]error{"k1": capErr}}
		p := newTestPool(fc, "k1", "k2")
		_, err := p.Generate(context.Background(), "", commandcode.CallMeta{}, &commandcode.GenerateRequest{})
		if !errors.Is(err, capErr) || len(fc.calls) != 1 {
			t.Fatalf("status=%d err=%v calls=%v", status, err, fc.calls)
		}
		fc.failWith = nil
		body, err := p.Generate(context.Background(), "", commandcode.CallMeta{}, &commandcode.GenerateRequest{})
		if err != nil {
			t.Fatal(err)
		}
		_ = body.Close()
		if fc.calls[1] != "k1" {
			t.Fatalf("key was broken: %v", fc.calls)
		}
	}
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
	if _, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(fc.calls) != 2 || fc.calls[0] != "k1" || fc.calls[1] != "k2" {
		t.Fatalf("calls = %v, want [k1 k2]", fc.calls)
	}

	// k1 must NOT be circuit-broken: the next request starts at k1 again.
	fc.failWith = nil
	if _, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err != nil {
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
	if _, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err == nil {
		t.Fatal("expected the model error to surface")
	}
	if len(fc.calls) != 1 {
		t.Fatalf("calls = %v, want exactly one attempt (no rotation)", fc.calls)
	}

	// And no circuit: the key stays healthy for valid models.
	fc.failWith = nil
	if _, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err != nil {
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
		if _, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err != nil {
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

	if _, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(fc.calls) != 2 || fc.calls[0] != "k1" || fc.calls[1] != "k2" {
		t.Fatalf("calls = %v", fc.calls)
	}

	// k1 is now circuit-broken: next request goes straight to k2.
	fc.calls = nil
	if _, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(fc.calls) != 1 || fc.calls[0] != "k2" {
		t.Fatalf("calls = %v, want [k2] only", fc.calls)
	}
}

func TestAllBroken(t *testing.T) {
	broken := &commandcode.APIError{Status: 400, Message: "insufficient credits"}
	fc := &fakeClient{failWith: map[string]error{"k1": broken, "k2": broken}}
	now := time.Now()
	p := New(fc, session.NewStore("test-secret"), []string{"k1", "k2"}, BreakerPolicy{Now: func() time.Time { return now }})

	// First call breaks both keys (spill chain), second finds none healthy.
	_, _ = p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{})
	_, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{})
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("err = %T %v, want *UnavailableError", err, err)
	}
	if unavailable.KeyCount != 2 || unavailable.RetryAfter != time.Hour {
		t.Fatalf("unavailable = %+v", unavailable)
	}
}

func TestNoRotateOnBadRequest(t *testing.T) {
	fc := &fakeClient{failWith: map[string]error{
		"k1": &commandcode.APIError{Status: 400, Code: "BAD_REQUEST", Message: "invalid mode"},
	}}
	p := newTestPool(fc, "k1", "k2")

	_, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(fc.calls) != 1 {
		t.Fatalf("calls = %v, want no rotation on request-shape errors", fc.calls)
	}
}

func TestServerFailureDoesNotCircuitOrRotate(t *testing.T) {
	fc := &fakeClient{failWith: map[string]error{
		"k1": &commandcode.APIError{Status: 500, Message: "internal server error"},
	}}
	p := newTestPool(fc, "k1", "k2")

	_, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{})
	if err == nil {
		t.Fatal("expected upstream 500")
	}
	if len(fc.calls) != 1 || fc.calls[0] != "k1" {
		t.Fatalf("calls = %v, want [k1] (no key rotation)", fc.calls)
	}
	if snap := p.Snapshot(); snap[0]["broken"].(bool) {
		t.Fatal("upstream 500 must not circuit-break k1")
	}

	delete(fc.failWith, "k1")
	if _, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err != nil {
		t.Fatalf("Generate after recovery: %v", err)
	}
	if fc.calls[1] != "k1" {
		t.Fatalf("k1 was skipped after upstream 500, calls = %v", fc.calls)
	}
}

func TestTransportFailureDoesNotCircuitOrRotate(t *testing.T) {
	fc := &fakeClient{failWith: map[string]error{"k1": errors.New("connection reset")}}
	p := newTestPool(fc, "k1", "k2")

	_, err := p.Generate(context.Background(), "", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if len(fc.calls) != 1 || fc.calls[0] != "k1" {
		t.Fatalf("calls = %v, want [k1] (no key rotation)", fc.calls)
	}
	if snap := p.Snapshot(); snap[0]["broken"].(bool) {
		t.Fatal("transport failure must not circuit-break k1")
	}
}

func TestExpiredBreakerAllowsSingleHalfOpenProbe(t *testing.T) {
	fc := &fakeClient{}
	now := time.Now()
	p := New(fc, session.NewStore("test-secret"), []string{"k1"}, BreakerPolicy{Now: func() time.Time { return now }})
	p.open(p.keys[0], time.Minute, "test")
	now = now.Add(time.Minute)

	probe, unavailable := p.acquireCandidate(nil)
	if probe == nil || unavailable != nil {
		t.Fatalf("first acquire = (%v, %v), want half-open probe", probe, unavailable)
	}
	second, unavailable := p.acquireCandidate(nil)
	if second != nil || unavailable == nil || unavailable.RetryAfter != time.Second {
		t.Fatalf("concurrent acquire = (%v, %+v), want temporary unavailability", second, unavailable)
	}

	p.reportSuccess(probe)
	next, unavailable := p.acquireCandidate(nil)
	if next != probe || unavailable != nil || next.probing {
		t.Fatalf("acquire after successful probe = (%v, %v)", next, unavailable)
	}
}

func TestPassthroughBypassesPool(t *testing.T) {
	fc := &fakeClient{}
	p := newTestPool(fc, "k1")

	if _, err := p.Generate(context.Background(), "downstream-key", commandcode.CallMeta{Root: "root1"}, &commandcode.GenerateRequest{}); err != nil {
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
