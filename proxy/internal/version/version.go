// Package version tracks the x-command-code-version value, refreshing it
// from the npm registry every 24h unless pinned.
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// registryURL is the npm metadata endpoint for the CLI package.
const registryURL = "https://registry.npmjs.org/command-code/latest"

// refreshInterval is how often the registry is polled.
const refreshInterval = 24 * time.Hour

// fallbackVersion is used when the registry is unreachable and nothing
// else is configured.
const fallbackVersion = "1.32.2"

// Tracker holds the current version value.
type Tracker struct {
	mu      sync.RWMutex
	v       string
	pin     string
	hc      *http.Client
	fetchFn func(ctx context.Context) (string, error) // test hook
}

// New builds a tracker. pin forces a fixed version (auto-refresh disabled);
// initial is the starting value before the first refresh.
func New(pin, initial string) *Tracker {
	v := pin
	if v == "" {
		v = initial
	}
	if v == "" {
		v = fallbackVersion
	}
	return &Tracker{
		v:   v,
		pin: pin,
		hc:  &http.Client{Timeout: 10 * time.Second},
	}
}

// String returns the current version for header injection.
func (t *Tracker) String() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.v
}

// Start refreshes the version every 24h until ctx is cancelled.
// No-op when pinned. Call as a goroutine.
func (t *Tracker) Start(ctx context.Context) {
	if t.pin != "" {
		slog.Info("version pinned, auto-refresh disabled", "version", t.pin)
		return
	}
	t.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(refreshInterval):
			t.refresh(ctx)
		}
	}
}

// refresh polls the registry once; failures keep the current value.
func (t *Tracker) refresh(ctx context.Context) {
	fetch := t.fetchFn
	if fetch == nil {
		fetch = t.fetchLatest
	}
	v, err := fetch(ctx)
	if err != nil {
		slog.Warn("version refresh failed, keeping current", "version", t.String(), "error", err)
		return
	}
	t.mu.Lock()
	t.v = v
	t.mu.Unlock()
	slog.Info("version refreshed from npm", "version", v)
}

// fetchLatest queries the npm registry for command-code@latest.
func (t *Tracker) fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry responded %d", resp.StatusCode)
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Version == "" {
		return "", fmt.Errorf("npm registry returned empty version")
	}
	return out.Version, nil
}
