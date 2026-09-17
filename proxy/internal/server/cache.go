package server

import (
	"context"
	"sync"
	"time"
)

// cachedFetch coalesces concurrent refreshes without holding a lock over I/O.
// Each caller can stop waiting independently; the shared fetch has its own
// timeout so a disconnected caller cannot cancel work needed by other callers.
// Values are immutable after publication.
type cachedFetch[T any] struct {
	mu          sync.Mutex
	value       T
	err         error
	hasValue    bool
	nextRefresh time.Time
	flight      *cacheFlight[T]
}

type cacheFlight[T any] struct {
	done  chan struct{}
	value T
	err   error
}

type cachePolicy struct {
	ttl        time.Duration
	failureTTL time.Duration
	timeout    time.Duration
	serveStale bool
}

func (c *cachedFetch[T]) get(ctx context.Context, policy cachePolicy, fetch func(context.Context) (T, error)) (T, error) {
	if err := ctx.Err(); err != nil {
		var zero T
		return zero, err
	}
	c.mu.Lock()
	if time.Now().Before(c.nextRefresh) {
		value, err := c.value, c.err
		c.mu.Unlock()
		return value, err
	}
	f := c.flight
	if f == nil {
		f = &cacheFlight[T]{done: make(chan struct{})}
		c.flight = f
		go c.refresh(context.WithoutCancel(ctx), policy, fetch, f)
	}
	if policy.serveStale && c.hasValue {
		value := c.value
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}

func (c *cachedFetch[T]) refresh(ctx context.Context, policy cachePolicy, fetch func(context.Context) (T, error), f *cacheFlight[T]) {
	ctx, cancel := context.WithTimeout(ctx, policy.timeout)
	defer cancel()
	value, err := fetch(ctx)
	ttl := policy.ttl
	if err != nil {
		ttl = policy.failureTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil || !policy.serveStale {
		c.value = value
		c.hasValue = err == nil
	}
	c.err = err
	c.nextRefresh = time.Now().Add(ttl)
	f.value, f.err = c.value, c.err
	c.flight = nil
	close(f.done)
}

func (c *cachedFetch[T]) refreshing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flight != nil
}
