package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
)

// modelCatalogTTL is how long a successful upstream fetch is cached.
const modelCatalogTTL = 5 * time.Minute

// fallbackModels is the static catalog used until the first successful
// upstream fetch (or when the upstream is unreachable). Model IDs follow
// the upstream "vendor/name" convention (standard-tier, goat-compatible).
var fallbackModels = []commandcode.ModelInfo{
	{ID: "deepseek/deepseek-v4-pro", Name: "DeepSeek V4 Pro (latest)", ContextLength: 1_000_000},
	{ID: "deepseek/deepseek-v4-flash", Name: "DeepSeek V4 Flash (latest)", ContextLength: 1_000_000},
	{ID: "deepseek/deepseek-v4-flash-vision-exp", Name: "DeepSeek V4 Flash Vision (exp)", ContextLength: 1_000_000},
	{ID: "deepseek/deepseek-v4-flash-fast", Name: "DeepSeek V4 Flash Fast", ContextLength: 1_000_000},
	{ID: "moonshotai/Kimi-K3", Name: "Kimi K3", ContextLength: 1_000_000},
	{ID: "moonshotai/Kimi-K2.7-Code", Name: "Kimi K2.7 Code", ContextLength: 256_000},
	{ID: "moonshotai/Kimi-K2.7-Code-Highspeed", Name: "Kimi K2.7 Code HighSpeed", ContextLength: 262_000},
	{ID: "moonshotai/Kimi-K2.6", Name: "Kimi K2.6", ContextLength: 256_000},
	{ID: "moonshotai/Kimi-K2.5", Name: "Kimi K2.5", ContextLength: 256_000},
	{ID: "z-ai/glm-5.3-flash", Name: "GLM-5.3 Flash", ContextLength: 1_048_576},
	{ID: "zai-org/GLM-5.3", Name: "GLM-5.3", ContextLength: 1_000_000},
	{ID: "zai-org/GLM-5.2", Name: "GLM-5.2", ContextLength: 1_000_000},
	{ID: "zai-org/GLM-5.2-Fast", Name: "GLM-5.2 Fast", ContextLength: 1_000_000},
	{ID: "zai-org/GLM-5.1", Name: "GLM-5.1", ContextLength: 200_000},
	{ID: "zai-org/GLM-5", Name: "GLM-5", ContextLength: 200_000},
	{ID: "MiniMaxAI/MiniMax-M3", Name: "MiniMax M3", ContextLength: 1_000_000},
	{ID: "MiniMaxAI/MiniMax-M2.7", Name: "MiniMax M2.7", ContextLength: 200_000},
	{ID: "MiniMaxAI/MiniMax-M2.5", Name: "MiniMax M2.5", ContextLength: 200_000},
	{ID: "xiaomi/mimo-v2.5-pro", Name: "MiMo V2.5 Pro", ContextLength: 1_000_000},
	{ID: "xiaomi/mimo-v2.5", Name: "MiMo V2.5", ContextLength: 1_000_000},
	{ID: "Qwen/Qwen3.8-Max", Name: "Qwen 3.8 Max", ContextLength: 1_000_000},
	{ID: "Qwen/Qwen3.8-27B", Name: "Qwen 3.8 27B", ContextLength: 262_144},
	{ID: "Qwen/Qwen3.8-Flash", Name: "Qwen 3.8 Flash", ContextLength: 1_000_000},
	{ID: "Qwen/Qwen3.7-Max", Name: "Qwen 3.7 Max", ContextLength: 1_000_000},
	{ID: "Qwen/Qwen3.7-Plus", Name: "Qwen 3.7 Plus", ContextLength: 1_000_000},
	{ID: "Qwen/Qwen3.7-Flash", Name: "Qwen 3.7 Flash", ContextLength: 1_000_000},
	{ID: "Qwen/Qwen3.6-Max-Preview", Name: "Qwen 3.6 Max Preview", ContextLength: 200_000},
	{ID: "Qwen/Qwen3.6-Plus", Name: "Qwen 3.6 Plus", ContextLength: 200_000},
	{ID: "stepfun/Step-3.7-Flash", Name: "Step 3.7 Flash", ContextLength: 256_000},
	{ID: "stepfun/Step-3.5-Flash", Name: "Step 3.5 Flash", ContextLength: 1_000_000},
	{ID: "tencent/hy4-preview", Name: "Tencent Hy4 Preview", ContextLength: 1_048_576},
	{ID: "tencent/hy3-paid", Name: "Tencent Hy3", ContextLength: 262_144},
}

// modelCatalog serves the model list with a TTL cache and static fallback.
type modelCatalog struct {
	fetch func(ctx context.Context, downstreamKey string) ([]commandcode.ModelInfo, error)

	mu      sync.Mutex
	models  []commandcode.ModelInfo
	fetched time.Time
}

// newModelCatalog wraps a fetcher. fetch may be nil (static-only mode).
func newModelCatalog(fetch func(ctx context.Context, downstreamKey string) ([]commandcode.ModelInfo, error)) *modelCatalog {
	return &modelCatalog{fetch: fetch}
}

// list returns the current catalog, refreshing when stale.
// Any fetch failure falls back to the last good list, then the static table.
func (c *modelCatalog) list(ctx context.Context, downstreamKey string) []commandcode.ModelInfo {
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

// get returns one model by ID, or nil when unknown.
func (c *modelCatalog) get(ctx context.Context, downstreamKey, id string) *commandcode.ModelInfo {
	for _, m := range c.list(ctx, downstreamKey) {
		if m.ID == id {
			return &m
		}
	}
	return nil
}

// modelJSON renders one catalog entry in OpenAI model format, enriched
// with the upstream display name and context length.
func modelJSON(m commandcode.ModelInfo) map[string]any {
	created := m.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	ownedBy := m.OwnedBy
	if ownedBy == "" {
		ownedBy = "command-code"
	}
	out := map[string]any{
		"id":       m.ID,
		"object":   "model",
		"created":  created,
		"owned_by": ownedBy,
	}
	if m.Name != "" {
		out["name"] = m.Name
	}
	if m.ContextLength > 0 {
		out["context_length"] = m.ContextLength
	}
	return out
}

// handleModels serves GET /v1/models in OpenAI list format.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.models.list(r.Context(), downstreamKey(r.Context()))
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, modelJSON(m))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// handleModelGet serves GET /v1/models/{id} (OpenAI retrieve-model shape).
func (s *Server) handleModelGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m := s.models.get(r.Context(), downstreamKey(r.Context()), id)
	if m == nil {
		writeError(w, http.StatusNotFound, "invalid_request_error",
			"The model '"+id+"' does not exist", 0)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelJSON(*m))
}
