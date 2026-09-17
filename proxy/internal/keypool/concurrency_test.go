package keypool

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

type generateFunc func(context.Context, commandcode.Credentials, *commandcode.GenerateRequest) (io.ReadCloser, error)

func (f generateFunc) Generate(ctx context.Context, creds commandcode.Credentials, req *commandcode.GenerateRequest) (io.ReadCloser, error) {
	return f(ctx, creds, req)
}

func TestLateResponseCannotClearNewBreaker(t *testing.T) {
	for _, outcome := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"server error", &commandcode.APIError{Status: 503}},
		{"plan error", &commandcode.APIError{Status: 403, Message: "MODEL_NOT_IN_PLAN"}},
		{"spend cap", &commandcode.APIError{Status: 403, Code: "USAGE_EXCEEDED"}},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			var calls atomic.Int32
			client := generateFunc(func(ctx context.Context, _ commandcode.Credentials, req *commandcode.GenerateRequest) (io.ReadCloser, error) {
				calls.Add(1)
				if req.ThreadID == "slow" {
					close(started)
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-release:
					}
					if outcome.err != nil {
						return nil, outcome.err
					}
					return io.NopCloser(strings.NewReader("")), nil
				}
				return nil, &commandcode.APIError{Status: 429}
			})
			p := New(client, session.NewStore("seed"), []string{"k1"}, BreakerPolicy{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				body, _ := p.Generate(ctx, "", commandcode.CallMeta{}, &commandcode.GenerateRequest{ThreadID: "slow"})
				if body != nil {
					_ = body.Close()
				}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("first request did not start")
			}
			_, _ = p.Generate(ctx, "", commandcode.CallMeta{}, &commandcode.GenerateRequest{})
			release <- struct{}{}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("late request did not finish")
			}
			_, err := p.Generate(ctx, "", commandcode.CallMeta{}, &commandcode.GenerateRequest{})
			var unavailable *UnavailableError
			if !errors.As(err, &unavailable) || calls.Load() != 2 {
				t.Fatalf("late result reopened traffic: err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestOldGenerationCannotAffectHalfOpenProbe(t *testing.T) {
	now := time.Now()
	p := New(&fakeClient{}, session.NewStore("seed"), []string{"k1"}, BreakerPolicy{Now: func() time.Time { return now }})
	old, _ := p.acquireCandidate(nil)
	failed, _ := p.acquireCandidate(nil)
	p.reportFailure(failed, &commandcode.APIError{Status: 429})
	now = now.Add(time.Minute)
	probe, _ := p.acquireCandidate(nil)
	if probe == nil {
		t.Fatal("missing probe")
	}
	// Old successes, cancellations, and failures must all leave the probe alone.
	p.reportSuccess(old)
	p.reportFailure(old, context.Canceled)
	p.reportFailure(old, &commandcode.APIError{Status: 401})
	if candidate, _ := p.acquireCandidate(nil); candidate != nil {
		t.Fatal("old response released an active probe")
	}
	p.reportSuccess(probe)
	p.reportFailure(old, &commandcode.APIError{Status: 429})
	if candidate, _ := p.acquireCandidate(nil); candidate == nil {
		t.Fatal("old failure broke a recovered key")
	}
}

func TestHalfOpenAdmitsOnlyOneConcurrentRequest(t *testing.T) {
	now := time.Now()
	p := New(&fakeClient{}, session.NewStore("seed"), []string{"k1"}, BreakerPolicy{Now: func() time.Time { return now }})
	initial, _ := p.acquireCandidate(nil)
	p.reportFailure(initial, &commandcode.APIError{Status: 429})
	now = now.Add(time.Minute)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lease, _ := p.acquireCandidate(nil); lease != nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("admitted %d probes", admitted.Load())
	}
}

func TestCanceledRequestDoesNotSpill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	client := generateFunc(func(context.Context, commandcode.Credentials, *commandcode.GenerateRequest) (io.ReadCloser, error) {
		calls++
		cancel()
		return nil, &commandcode.APIError{Status: 429}
	})
	p := New(client, session.NewStore("seed"), []string{"k1", "k2"}, BreakerPolicy{})
	_, err := p.Generate(ctx, "", commandcode.CallMeta{}, &commandcode.GenerateRequest{})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestCanceledProbeDoesNotDeclareKeyRecovered(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		now := time.Now()
		p := New(&fakeClient{}, session.NewStore("seed"), []string{"k1"}, BreakerPolicy{Now: func() time.Time { return now }})
		initial, _ := p.acquireCandidate(nil)
		p.reportFailure(initial, &commandcode.APIError{Status: 429})
		now = now.Add(time.Minute)
		probe, _ := p.acquireCandidate(nil)
		if p.reportFailure(probe, err) {
			t.Fatalf("canceled probe rotated: %v", err)
		}
		next, _ := p.acquireCandidate(nil)
		if next == nil || !next.state.probing {
			t.Fatalf("canceled probe declared recovery: %v", err)
		}
		if extra, _ := p.acquireCandidate(nil); extra != nil {
			t.Fatal("concurrent probe admitted")
		}
	}
}
