package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

// Deps bundles the injectable dependencies of the HTTP surface.
type Deps struct {
	Upstream Upstream

	// Version is the proxy build version (stamped via ldflags).
	Version string
	// CCVersion reports the CLI version currently sent upstream; nil → omitted.
	CCVersion func() string

	// FetchModels refreshes the model catalog; nil → static fallback only.
	FetchModels func(ctx context.Context, downstreamKey string) ([]commandcode.ModelInfo, error)
	// FetchCredits passthroughs billing data; nil → /v1/credits returns 501.
	FetchCredits func(ctx context.Context, downstreamKey string) (credits, subscriptions json.RawMessage, err error)
	// Probe checks upstream health for /readyz; nil → readiness mirrors liveness.
	Probe func(ctx context.Context) error

	// Sessions derives conversation-scoped identity; nil → random secret.
	Sessions *session.Store

	// Metrics records request/token/cost accounting; nil disables it.
	Metrics MetricsRecorder
}

// MetricsRecorder is the instrumentation slice used by handlers.
type MetricsRecorder interface {
	IncRequest(model string, streamed bool, result string)
	AddTokens(kind string, n int)
	ObserveLatency(model string, streamed bool, ms int64)
	ObserveCost(model string, usd float64)
}

// ── /v1/credits ─────────────────────────────────────────────────────

// handleCredits passthroughs upstream billing data as one JSON document.
func (s *Server) handleCredits(w http.ResponseWriter, r *http.Request) {
	if s.deps.FetchCredits == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"credits passthrough is not configured", 0)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	credits, subs, err := s.deps.FetchCredits(ctx, downstreamKey(r.Context()))
	if err != nil {
		slog.Warn("credits fetch failed", "error", err)
		writeUpstreamError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]json.RawMessage{
		"credits":       credits,
		"subscriptions": subs,
	})
}

// ── /version ────────────────────────────────────────────────────────

// handleVersion reports build and upstream-protocol versions.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	out := map[string]string{"version": s.deps.Version}
	if out["version"] == "" {
		out["version"] = "dev"
	}
	if s.deps.CCVersion != nil {
		out["ccVersion"] = s.deps.CCVersion()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ── /metrics ────────────────────────────────────────────────────────

// handleMetrics renders the Prometheus text exposition format.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	if s.renderMetrics == nil {
		writeError(w, http.StatusNotFound, "not_found", "metrics disabled", 0)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(s.renderMetrics()))
}

// ── /readyz with cached upstream probe ──────────────────────────────

// readyzTTL is how long a probe result is cached.
const readyzTTL = 30 * time.Second

type readyzCache struct {
	mu      sync.Mutex
	ok      bool
	checked time.Time
}

// handleReadyz reports readiness using a cached upstream probe.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.deps.Probe == nil {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}

	s.readyz.mu.Lock()
	fresh := time.Since(s.readyz.checked) < readyzTTL
	if !fresh {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		err := s.deps.Probe(ctx)
		cancel()
		s.readyz.ok = err == nil
		s.readyz.checked = time.Now()
		if err != nil {
			slog.Warn("readiness probe failed", "error", err)
		}
	}
	ok := s.readyz.ok
	s.readyz.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("NOT READY"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
