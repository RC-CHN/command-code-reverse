package fingerprint

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type reportCall struct {
	key, session, platform string
	fp                     any
}

type telemetryStub struct {
	fingerprint func(context.Context, string, any, string) error
	session     func(context.Context, string, string, string, string) error
}

func (s telemetryStub) RecordFingerprint(ctx context.Context, key string, fp any, version string) error {
	return s.fingerprint(ctx, key, fp, version)
}

func (s telemetryStub) RecordCLISession(ctx context.Context, key, id, platform, version string) error {
	return s.session(ctx, key, id, platform, version)
}

func receiveReport(t *testing.T, calls <-chan reportCall) reportCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("telemetry was not delivered")
		return reportCall{}
	}
}

func TestReporterConcurrentKeysAndFailure(t *testing.T) {
	calls := make(chan reportCall, 128)
	fp := &Fingerprint{Thumbmark: "device", Components: Components{Platform: "linux", Arch: "x64"}}
	checkDeadline := func(ctx context.Context) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > reportTimeout {
			t.Error("telemetry needs a bounded deadline")
		}
	}
	client := telemetryStub{
		fingerprint: func(ctx context.Context, key string, fp any, _ string) error {
			checkDeadline(ctx)
			calls <- reportCall{key: key, fp: fp}
			return errors.New("synthetic report failure")
		},
		session: func(ctx context.Context, key, id, platform, _ string) error {
			checkDeadline(ctx)
			calls <- reportCall{key: key, session: id, platform: platform}
			return errors.New("synthetic lifecycle failure")
		},
	}
	reporter := NewReporter(t.Context(), client, fp, time.Minute, "seed")
	t.Cleanup(reporter.Close)
	if len(calls) != 0 {
		t.Fatal("inactive keys must not be reported at startup")
	}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			touch(reporter, fmt.Sprintf("key-%d", i%3))
		}()
	}
	wg.Wait()
	fingerprints, sessions := map[string]int{}, map[string]string{}
	for range 6 {
		call := receiveReport(t, calls)
		if call.fp != nil {
			fingerprints[call.key]++
			if call.fp != fp {
				t.Error("keys on the same device must share a fingerprint")
			}
		} else {
			if _, ok := sessions[call.key]; ok {
				t.Fatal("duplicate lifecycle event")
			}
			if !regexp.MustCompile(`^sess_[0-9a-f]{16}$`).MatchString(call.session) || call.platform != "linux-x64" {
				t.Fatalf("invalid lifecycle metadata: %+v", call)
			}
			sessions[call.key] = call.session
		}
	}
	ids := map[string]bool{}
	for i := range 3 {
		key := fmt.Sprintf("key-%d", i)
		if fingerprints[key] != 1 || sessions[key] == "" || ids[sessions[key]] {
			t.Fatal("expected one attempt and a distinct telemetry session per key")
		}
		ids[sessions[key]] = true
		touch(reporter, key) // Failed reports are still attempted only once.
	}
	touch(reporter, "")
	reporter.Close()
	if len(calls) != 0 {
		t.Fatal("reports were retried")
	}
}

func TestReporterQueueAndShutdown(t *testing.T) {
	started := make(chan reportCall, reportWorkers)
	client := telemetryStub{
		fingerprint: func(ctx context.Context, key string, _ any, _ string) error {
			started <- reportCall{key: key}
			<-ctx.Done()
			return ctx.Err()
		},
		session: func(context.Context, string, string, string, string) error {
			t.Error("shutdown must not begin another report")
			return nil
		},
	}
	reporter := NewReporter(t.Context(), client, &Fingerprint{}, time.Minute, "seed")
	t.Cleanup(reporter.Close)
	for i := range reportWorkers {
		touch(reporter, fmt.Sprintf("active-%d", i))
		receiveReport(t, started)
	}
	for i := range reportQueueSize {
		touch(reporter, fmt.Sprintf("queued-%d", i))
	}
	// With all workers blocked and queue full, Observe must return promptly.
	done := make(chan struct{})
	go func() {
		touch(reporter, "overflow")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("telemetry blocked the request path")
	}
	if a := reporter.active[sha256.Sum256([]byte("overflow"))]; a == nil || a.queued {
		t.Fatal("queue overflow should allow a later enqueue")
	}
	if len(reporter.queue) != reportQueueSize || len(reporter.active) != reportWorkers+reportQueueSize+1 {
		t.Fatal("unexpected queue/dedup bounds")
	}
	reporter.Close()
	touch(reporter, "after-close")
	if len(reporter.queue) != 0 || len(started) != 0 {
		t.Fatal("shutdown retained or started queued work")
	}
}

func TestReporterKeyCapacity(t *testing.T) {
	reporter := NewReporter(t.Context(), telemetryStub{}, &Fingerprint{}, time.Minute, "seed")
	t.Cleanup(reporter.Close)
	for i := range maxReportedKeys {
		reporter.active[sha256.Sum256([]byte(fmt.Sprint(i)))] = &activity{inflight: 1}
	}
	touch(reporter, "new-key")
	if len(reporter.active) != maxReportedKeys || len(reporter.queue) != 0 {
		t.Fatal("key capacity must skip new keys without evicting earlier identities")
	}
}

func touch(r *Reporter, key string) {
	if finish := r.Use(key, "1.62.1"); finish != nil {
		finish()
	}
}

func TestReporterIdleInflightAndVersionLifecycle(t *testing.T) {
	calls := make(chan reportCall, 20)
	client := telemetryStub{
		fingerprint: func(_ context.Context, key string, fp any, _ string) error {
			calls <- reportCall{key: key, fp: fp}
			return nil
		},
		session: func(_ context.Context, key, session, _, version string) error {
			calls <- reportCall{key: key, session: session, platform: version}
			return nil
		},
	}
	r := NewReporter(t.Context(), client, &Fingerprint{}, time.Minute, "seed")
	t.Cleanup(r.Close)
	var nanos atomic.Int64
	nanos.Store(time.Now().UnixNano())
	r.now = func() time.Time { return time.Unix(0, nanos.Load()) }
	readSession := func(version string) string {
		t.Helper()
		var id string
		for range 2 {
			call := receiveReport(t, calls)
			if call.session != "" {
				id = call.session
				if call.platform != version {
					t.Fatal("session lost its inference version")
				}
			}
		}
		if id == "" {
			t.Fatal("missing session event")
		}
		return id
	}
	finish := r.Use("key", "v1")
	first := readSession("v1")
	nanos.Add(int64(2 * time.Hour))
	concurrentFinish := r.Use("key", "v1")
	concurrentFinish()
	if len(calls) != 0 {
		t.Fatal("long-running inference must keep its session active")
	}
	finish()
	finish() // Releasing a request twice must not corrupt in-flight counts.
	if r.active[sha256.Sum256([]byte("key"))].inflight != 0 {
		t.Fatal("unbalanced activity")
	}
	nanos.Add(int64(10 * time.Second))
	r.Use("key", "v1")()
	if len(calls) != 0 {
		t.Fatal("recent activity must reuse its session")
	}
	nanos.Add(int64(2 * time.Hour))
	if len(calls) != 0 {
		t.Fatal("idle expiration must not send traffic")
	}
	secondFinish := r.Use("key", "v1")
	second := readSession("v1")
	if first == second {
		t.Fatal("resuming after idle must create a new telemetry session")
	}
	// A CLI update starts a new logical client, even with an older stream open.
	thirdFinish := r.Use("key", "v2")
	third := readSession("v2")
	if third == second {
		t.Fatal("version change retained an old CLI session")
	}
	secondFinish()
	if r.active[sha256.Sum256([]byte("key"))].inflight != 1 {
		t.Fatal("old completion changed the new session")
	}
	thirdFinish()
}

func TestIdleWindowStablePerSeedAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := telemetryStub{
		fingerprint: func(ctx context.Context, _ string, _ any, _ string) error { <-ctx.Done(); return ctx.Err() },
		session:     func(context.Context, string, string, string, string) error { return nil },
	}
	a := NewReporter(ctx, client, &Fingerprint{}, 30*time.Minute, "same-seed")
	b := NewReporter(ctx, client, &Fingerprint{}, 30*time.Minute, "same-seed")
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	windows := map[time.Duration]bool{}
	for _, key := range []string{"a", "b", "c", "d"} {
		a.Use(key, "v1")()
		b.Use(key, "v1")()
		id := sha256.Sum256([]byte(key))
		x, y := a.active[id].idle, b.active[id].idle
		if x != y || x < 24*time.Minute || x > 36*time.Minute {
			t.Fatal("idle window is unstable or out of bounds")
		}
		windows[x] = true
	}
	if len(windows) == 1 {
		t.Fatal("all accounts share one idle window")
	}
}

func TestQueuedIdleSessionIsNotReported(t *testing.T) {
	started := make(chan reportCall, 16)
	release := make(chan struct{})
	var nanos atomic.Int64
	nanos.Store(time.Now().UnixNano())
	client := telemetryStub{
		fingerprint: func(ctx context.Context, key string, _ any, _ string) error {
			started <- reportCall{key: key}
			if strings.HasPrefix(key, "block-") {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
		session: func(_ context.Context, key, session, _, _ string) error {
			started <- reportCall{key: key, session: session}
			return nil
		},
	}
	r := NewReporter(t.Context(), client, &Fingerprint{}, time.Minute, "seed")
	t.Cleanup(r.Close)
	r.now = func() time.Time { return time.Unix(0, nanos.Load()) }
	for i := range reportWorkers {
		touch(r, fmt.Sprintf("block-%d", i))
		receiveReport(t, started)
	}
	touch(r, "expired-queue")
	nanos.Add(int64(time.Hour))
	touch(r, "fresh")
	close(release)
	for range 2 {
		if call := receiveReport(t, started); call.key != "fresh" {
			t.Fatalf("inactive session was reported: %+v", call)
		}
	}
	r.Close()
	if len(started) != 0 {
		t.Fatal("extra stale reports")
	}
}
