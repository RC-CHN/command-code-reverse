package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// modelCatalogTTL is how long a successful upstream fetch is cached.
const modelCatalogTTL = 5 * time.Minute

// fallbackModels is the static catalog used until the first successful
// upstream fetch (or when the upstream is unreachable). Model IDs follow
// the upstream "vendor/name" convention (standard-tier, goat-compatible).
var fallbackModels = []string{
	"deepseek/deepseek-v4-pro",
	"deepseek/deepseek-v4-flash",
	"moonshotai/Kimi-K2.6",
	"moonshotai/Kimi-K2.5",
	"zai-org/GLM-5.1",
	"zai-org/GLM-5",
	"MiniMaxAI/MiniMax-M3",
	"MiniMaxAI/MiniMax-M2.7",
	"MiniMaxAI/MiniMax-M2.5",
	"Qwen/Qwen3.6-Max-Preview",
	"Qwen/Qwen3.6-Plus",
	"Qwen/Qwen3.7-Max",
	"stepfun/Step-3.7-Flash",
	"stepfun/Step-3.5-Flash",
	"xiaomi/mimo-v2.5-pro",
	"xiaomi/mimo-v2.5",
}

// modelCatalog serves the model list with a TTL cache and static fallback.
type modelCatalog struct {
	fetch func(ctx context.Context, downstreamKey string) ([]string, error)

	mu      sync.Mutex
	models  []string
	fetched time.Time
}

// newModelCatalog wraps a fetcher. fetch may be nil (static-only mode).
func newModelCatalog(fetch func(ctx context.Context, downstreamKey string) ([]string, error)) *modelCatalog {
	return &modelCatalog{fetch: fetch}
}

// list returns the current catalog, refreshing when stale.
// Any fetch failure falls back to the last good list, then the static table.
func (c *modelCatalog) list(ctx context.Context, downstreamKey string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fetch != nil && time.Since(c.fetched) > modelCatalogTTL {
		if models, err := c.fetch(ctx, downstreamKey); err == nil && len(models) > 0 {
			c.models = models
			c.fetched = time.Now()
			slog.Info("model catalog refreshed", "count", len(models))
		} else if err != nil {
			slog.Warn("model catalog fetch failed, using cached/fallback", "error", err)
		}
	}

	if len(c.models) > 0 {
		return c.models
	}
	return fallbackModels
}

// handleModels serves GET /v1/models in OpenAI list format.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := s.models.list(r.Context(), downstreamKey(r.Context()))
	data := make([]map[string]any, 0, len(models))
	for _, id := range models {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  now,
			"owned_by": "command-code",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
