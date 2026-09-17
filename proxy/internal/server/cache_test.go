package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
)

func TestCachedFetchCoalescesAndAllowsCancellation(t *testing.T) {
	var c cachedFetch[int]
	policy := cachePolicy{ttl: time.Minute, failureTTL: time.Minute, timeout: 5 * time.Second}
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	fetch := func(ctx context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-release:
			return 42, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	defer cancelLeader()
	leaderDone := make(chan error, 1)
	go func() { _, err := c.get(leaderCtx, policy, fetch); leaderDone <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("fetch did not start")
	}
	cancelLeader()
	select {
	case err := <-leaderDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader err=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("canceled leader is stuck waiting")
	}
	// A second canceled waiter must also return while the shared fetch blocks.
	waitCtx, cancelWait := context.WithCancel(ctx)
	waitDone := make(chan error, 1)
	go func() { _, err := c.get(waitCtx, policy, fetch); waitDone <- err }()
	cancelWait()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("canceled follower is stuck waiting")
	}
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.get(ctx, policy, fetch)
			if v != 42 || err != nil {
				t.Errorf("result=(%d,%v)", v, err)
			}
		}()
	}
	select {
	case release <- struct{}{}:
	case <-ctx.Done():
		t.Fatal("shared fetch canceled with its original caller")
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fetch calls=%d", calls.Load())
	}
}

func TestCachedFetchStaleRefreshAndFailureBackoff(t *testing.T) {
	var c cachedFetch[int]
	policy := cachePolicy{ttl: time.Minute, failureTTL: time.Minute, timeout: 5 * time.Second, serveStale: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = c.get(ctx, policy, func(context.Context) (int, error) { return 7, nil })
	c.mu.Lock()
	c.nextRefresh = time.Time{}
	c.mu.Unlock()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	fetch := func(ctx context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-release:
			return 0, errors.New("offline")
		}
	}
	// These calls must return before the refresh is unblocked.
	for range 64 {
		v, err := c.get(ctx, policy, fetch)
		if v != 7 || err != nil {
			t.Fatalf("stale result=(%d,%v)", v, err)
		}
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("refresh not started")
	}
	c.mu.Lock()
	done := c.flight.done
	c.mu.Unlock()
	release <- struct{}{}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("refresh not completed")
	}
	for range 64 {
		v, err := c.get(ctx, policy, fetch)
		if v != 7 || err == nil {
			t.Fatalf("failed refresh lost cached value: (%d,%v)", v, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("outage caused %d fetches", calls.Load())
	}
}

func TestCachedFetchTimeoutAndColdFailureBackoff(t *testing.T) {
	var c cachedFetch[int]
	policy := cachePolicy{ttl: time.Minute, failureTTL: time.Minute, timeout: 20 * time.Millisecond}
	var calls atomic.Int32
	fetch := func(ctx context.Context) (int, error) {
		calls.Add(1)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 64 {
		_, err := c.get(ctx, policy, fetch)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("cold outage caused %d fetches", calls.Load())
	}
	// Expiring the negative entry must allow recovery.
	c.mu.Lock()
	c.nextRefresh = time.Time{}
	c.mu.Unlock()
	v, err := c.get(ctx, policy, func(context.Context) (int, error) { return 9, nil })
	if v != 9 || err != nil {
		t.Fatalf("recovery=(%d,%v)", v, err)
	}
}

func TestModelCatalogSeparatesAccountsWithoutBlocking(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	c := newModelCatalog(func(ctx context.Context, key string) ([]commandcode.ModelInfo, error) {
		if key == "slow" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []commandcode.ModelInfo{{ID: key}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	slowDone := make(chan []commandcode.ModelInfo, 1)
	go func() { slowDone <- c.list(ctx, "slow") }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("slow fetch not started")
	}
	for _, key := range []string{"fast", "another", "fast"} {
		models := c.list(ctx, key)
		if len(models) != 1 || models[0].ID != key {
			t.Fatalf("key=%s got=%v", key, models)
		}
	}
	release <- struct{}{}
	select {
	case models := <-slowDone:
		if len(models) != 1 || models[0].ID != "slow" {
			t.Fatalf("slow got=%v", models)
		}
	case <-ctx.Done():
		t.Fatal("slow fetch not completed")
	}
}

func TestModelCatalogCapacity(t *testing.T) {
	c := newModelCatalog(nil)
	for i := 0; i < modelCatalogCapacity*2; i++ {
		entry := c.entry(fmt.Sprint(i))
		if entry == nil {
			t.Fatal("idle entries should be evicted")
		}
		c.release(entry)
	}
	if len(c.entries) != modelCatalogCapacity {
		t.Fatalf("entries=%d", len(c.entries))
	}
	// Pin every slot, as if callers are between lookup and starting a fetch.
	for _, entry := range c.entries {
		entry.users++
	}
	if c.entry("over capacity") != nil {
		t.Fatal("evicted an entry still in use")
	}
}

func TestReadyzRefreshDoesNotServeExpiredSuccess(t *testing.T) {
	var calls atomic.Int32
	s := &Server{deps: Deps{Probe: func(context.Context) error {
		if calls.Add(1) == 1 {
			return nil
		}
		return errors.New("offline")
	}}}
	request := func() int {
		rec := httptest.NewRecorder()
		s.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	if code := request(); code != 200 {
		t.Fatalf("initial status=%d", code)
	}
	s.readyz.mu.Lock()
	s.readyz.nextRefresh = time.Time{}
	s.readyz.mu.Unlock()
	for range 32 {
		if code := request(); code != 503 {
			t.Fatalf("offline status=%d", code)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("probe calls=%d", calls.Load())
	}
}
