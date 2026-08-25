// Package server wires the HTTP surface of commandcode-proxy:
// OpenAI-compatible routes, auth, health probes, metrics, and models.
package server

import (
	"net/http"
	"sync/atomic"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	cfg    *config.Config
	deps   Deps
	models *modelCatalog

	// renderMetrics produces the /metrics payload; nil when disabled.
	renderMetrics func() string
	// readyz caches the upstream probe result.
	readyz readyzCache

	// consecutiveTimeouts backs the "reduce context" hint logic.
	consecutiveTimeouts atomic.Int32
}

// New builds the root handler with all routes mounted.
func New(cfg *config.Config, deps Deps, renderMetrics func() string) http.Handler {
	s := &Server{cfg: cfg, deps: deps, renderMetrics: renderMetrics}
	s.models = newModelCatalog(deps.FetchModels)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /version", s.handleVersion)

	// Authenticated API surface.
	api := http.NewServeMux()
	api.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	api.HandleFunc("GET /v1/models", s.handleModels)
	api.HandleFunc("GET /v1/credits", s.handleCredits)
	mux.Handle("/v1/", s.authMiddleware(api))

	return mux
}

// handleHealthz reports liveness (process is up).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
