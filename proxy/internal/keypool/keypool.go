// Package keypool implements the upstream key pool: fill-first selection
// with a per-key circuit breaker. Fill-first saturates key[0] until it is
// limited or broken, then spills to the next key — maximizing per-key
// session continuity and therefore prefix cache hits.
package keypool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

// GenerateClient is the slice of commandcode.Client the pool needs
// (satisfied by *commandcode.Client; mocked in tests).
type GenerateClient interface {
	Generate(ctx context.Context, creds commandcode.Credentials, req *commandcode.GenerateRequest) (io.ReadCloser, error)
}

// BreakerPolicy tunes the circuit breaker.
type BreakerPolicy struct {
	CreditsTTL   time.Duration    // insufficient credits / dead key (default 1h)
	RateLimitTTL time.Duration    // 429 without server guidance (default 1m)
	Now          func() time.Time // test hook
}

func (p *BreakerPolicy) withDefaults() BreakerPolicy {
	out := *p
	if out.CreditsTTL == 0 {
		out.CreditsTTL = time.Hour
	}
	if out.RateLimitTTL == 0 {
		out.RateLimitTTL = time.Minute
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	return out
}

type keyState struct {
	key       string
	openUntil time.Time // breaker open until this instant
	probing   bool      // one half-open probe is already in flight
}

// UnavailableError means every pooled key is either circuit-open or already
// running its single half-open probe. RetryAfter is the earliest useful time
// for a caller to try the pool again.
type UnavailableError struct {
	KeyCount   int
	RetryAfter time.Duration
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("keypool: all %d keys are temporarily unavailable", e.KeyCount)
}

// Pool is a fill-first key selector with circuit breaking.
// It satisfies server.Upstream.
type Pool struct {
	client   GenerateClient
	sessions *session.Store
	policy   BreakerPolicy

	mu   sync.Mutex
	keys []*keyState
}

// New builds a pool over keys (fill-first order).
func New(client GenerateClient, sessions *session.Store, keys []string, policy BreakerPolicy) *Pool {
	p := &Pool{
		client:   client,
		sessions: sessions,
		policy:   policy.withDefaults(),
	}
	for _, k := range keys {
		p.keys = append(p.keys, &keyState{key: k})
	}
	return p
}

// Generate picks a key (fill-first, skipping open breakers) and starts an
// upstream stream. Passthrough hints bypass the pool entirely (no breaker).
// meta carries conversation + trace identity so session IDs stay stable
// within a conversation and retries share one trace ID.
func (p *Pool) Generate(ctx context.Context, hint string, meta commandcode.CallMeta, req *commandcode.GenerateRequest) (io.ReadCloser, error) {
	if hint != "" {
		// Passthrough mode: downstream supplied its own key; no pooling.
		return p.client.Generate(ctx, p.creds(hint, meta), req)
	}

	attempted := make(map[*keyState]bool, len(p.keys))
	var lastErr error
	for {
		ks, unavailable := p.acquireCandidate(attempted)
		if ks == nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, unavailable
		}
		attempted[ks] = true

		body, err := p.client.Generate(ctx, p.creds(ks.key, meta), req)
		if err == nil {
			p.reportSuccess(ks)
			return body, nil
		}
		lastErr = err
		if !p.reportFailure(ks, err) {
			// Non-key-specific failure (network, cancel): no point rotating.
			return nil, lastErr
		}
		slog.Warn("keypool: key failed, spilling to next",
			"keyPrefix", prefix(ks.key), "error", err)
	}
}

// acquireCandidate returns the first usable, not-yet-attempted key. Once an
// open breaker expires, exactly one request is admitted as its half-open
// probe; concurrent requests skip that key until the probe completes.
func (p *Pool) acquireCandidate(attempted map[*keyState]bool) (*keyState, *UnavailableError) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.policy.Now()
	for _, ks := range p.keys {
		if attempted[ks] || ks.probing || now.Before(ks.openUntil) {
			continue
		}
		if !ks.openUntil.IsZero() {
			ks.probing = true
		}
		return ks, nil
	}

	var retryAfter time.Duration
	for _, ks := range p.keys {
		if remaining := ks.openUntil.Sub(now); remaining > 0 && (retryAfter == 0 || remaining < retryAfter) {
			retryAfter = remaining
		}
	}
	if retryAfter == 0 {
		retryAfter = time.Second
	}
	return nil, &UnavailableError{KeyCount: len(p.keys), RetryAfter: retryAfter}
}

// ReportTerminal lets the stream layer report an in-band terminal marker
// (billing/plan) against the key that served the stream. Satisfies the
// optional server reporter hook.
func (p *Pool) ReportTerminal(hint, marker string) {
	if hint != "" {
		return // passthrough keys are not pooled
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Fill-first means the first healthy key served it; find the first
	// non-open key and break it. (Single-key deployments: trivially correct.)
	for _, ks := range p.keys {
		if !p.policy.Now().Before(ks.openUntil) {
			ks.openUntil = p.policy.Now().Add(p.policy.CreditsTTL)
			slog.Warn("keypool: terminal marker, key circuit-broken",
				"keyPrefix", prefix(ks.key), "marker", marker,
				"openFor", p.policy.CreditsTTL)
			return
		}
	}
}

// reportFailure opens the breaker according to the failure class and
// reports whether rotating to the next key makes sense.
func (p *Pool) reportFailure(ks *keyState, err error) (rotate bool) {
	var ae *commandcode.APIError
	switch {
	case errors.Is(err, context.Canceled):
		p.releaseProbe(ks)
		return false
	case errors.As(err, &ae) && ae.IsSpendCapExceeded():
		// Caps may be organization-wide or model-specific. Preserve the
		// key and surface the policy limit without rotating accounts.
		p.close(ks)
		return false
	case errors.As(err, &ae) && ae.IsInsufficientCredits():
		p.open(ks, p.policy.CreditsTTL, "insufficient_credits")
	case errors.As(err, &ae) && ae.IsRateLimited():
		p.open(ks, p.policy.RateLimitTTL, "rate_limited")
	case errors.As(err, &ae) && ae.IsModelNotInPlan():
		// Tier mismatch, not a key problem: keep the key healthy, but a
		// higher-tier pooled key may succeed, so rotating is worthwhile.
		p.close(ks)
		return true
	case errors.As(err, &ae) && ae.IsModelNotRecognized():
		// Unknown model ID: no pooled key will serve it either.
		p.close(ks)
		return false
	case errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403):
		p.open(ks, p.policy.CreditsTTL, "auth_rejected")
	case errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500:
		p.close(ks)
		return false // request-shape problem; rotating won't help
	default:
		// 5xx and transport failures describe the upstream service/path, not
		// a bad account. Surface the original failure without poisoning this
		// key or multiplying one global outage across every pooled key.
		p.close(ks)
		return false
	}
	return true
}

func (p *Pool) reportSuccess(ks *keyState) {
	p.close(ks)
}

func (p *Pool) close(ks *keyState) {
	p.mu.Lock()
	ks.openUntil = time.Time{}
	ks.probing = false
	p.mu.Unlock()
}

func (p *Pool) releaseProbe(ks *keyState) {
	p.mu.Lock()
	ks.probing = false
	p.mu.Unlock()
}

func (p *Pool) open(ks *keyState, ttl time.Duration, reason string) {
	p.mu.Lock()
	ks.openUntil = p.policy.Now().Add(ttl)
	ks.probing = false
	p.mu.Unlock()
	slog.Warn("keypool: circuit opened",
		"keyPrefix", prefix(ks.key), "reason", reason, "openFor", ttl)
}

// creds builds per-request upstream credentials for a key. Session identity
// derives from (key, conversation root): stable within a conversation, fresh
// across conversations, and re-rolled when a spill changes accounts. The
// trace ID passes through so retry attempts share one trace.
func (p *Pool) creds(key string, meta commandcode.CallMeta) commandcode.Credentials {
	return commandcode.Credentials{
		APIKey:      key,
		SessionID:   p.sessions.SessionID(key, meta.Root),
		ProjectSlug: p.sessions.ProjectSlug(key, meta.Root),
		TraceID:     meta.TraceID,
	}
}

// prefix returns a log-safe key prefix.
func prefix(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}

// Snapshot exposes breaker state for metrics/debugging.
func (p *Pool) Snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.keys))
	for _, ks := range p.keys {
		out = append(out, map[string]any{
			"keyPrefix": prefix(ks.key),
			"broken":    p.policy.Now().Before(ks.openUntil),
			"probing":   ks.probing,
		})
	}
	return out
}
