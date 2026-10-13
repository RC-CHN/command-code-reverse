package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stats"
)

//go:embed dashboard/index.html dashboard/app.js dashboard/style.css
var dashboardFiles embed.FS

func (s *Server) mountDashboard(mux *http.ServeMux) {
	assets, _ := fs.Sub(dashboardFiles, "dashboard")
	files := http.StripPrefix("/dashboard/", http.FileServer(http.FS(assets)))
	mux.Handle("GET /dashboard/", dashboardHeaders(files))
	api := http.NewServeMux()
	api.HandleFunc("GET /dashboard/api/stats", s.handleDashboardStats)
	api.HandleFunc("GET /dashboard/api/keys/{id}/usage", s.handleDashboardUsage)
	mux.Handle("GET /dashboard/api/", dashboardHeaders(s.dashboardAuth(api)))
}

func dashboardHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := bearerToken(r)
		a, b := sha256.Sum256([]byte(provided)), sha256.Sum256([]byte(s.cfg.DashboardAPIKey))
		if provided == "" || s.cfg.DashboardAPIKey == "" || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid dashboard API key", 0)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	endpoint := r.URL.Query().Get("endpoint")
	if endpoint == "" {
		endpoint = "llm"
	}
	if endpoint != "llm" && endpoint != "jev" {
		writeError(w, 400, "invalid_request_error", "endpoint must be llm or jev", 0)
		return
	}
	routing := keypool.RoutingSnapshot{Keys: []keypool.RouteStatus{}}
	if s.cfg.AuthMode == config.AuthManaged {
		fetch := s.deps.Routing
		if endpoint == "jev" {
			fetch = s.deps.JevRouting
		}
		if fetch != nil {
			routing = fetch()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		stats.Snapshot
		Routing  keypool.RoutingSnapshot `json:"routing"`
		AuthMode config.AuthMode         `json:"authMode"`
		Version  string                  `json:"version"`
	}{s.dashboard.Snapshot(endpoint), routing, s.cfg.AuthMode, s.deps.Version})
}

func (s *Server) handleDashboardUsage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthMode != config.AuthManaged {
		writeError(w, 400, "invalid_request_error", "Passthrough credentials are not retained for billing queries", 0)
		return
	}
	index := -1
	for i := range s.cfg.APIKeys {
		if r.PathValue("id") == stats.KeyID(i) {
			index = i
			break
		}
	}
	if index < 0 {
		writeError(w, 404, "not_found", "Unknown key slot", 0)
		return
	}
	if s.deps.FetchUsage == nil {
		writeError(w, 501, "not_implemented", "Usage queries are not configured", 0)
		return
	}
	value, err := s.accountUsage[index].get(r.Context(), cachePolicy{
		ttl: time.Minute, failureTTL: time.Minute, timeout: 15 * time.Second,
	}, func(ctx context.Context) (*commandcode.AccountUsage, error) {
		select {
		case s.usageSlots <- struct{}{}:
			defer func() { <-s.usageSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return s.deps.FetchUsage(ctx, s.cfg.APIKeys[index])
	})
	if err != nil {
		// Errors may contain account data or credentials; never send them to UI.
		writeError(w, 502, "upstream_error", "Upstream usage query unavailable; retry after one minute", 60)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// observeInference wraps auth as well as handlers so rejected requests are
// counted, while billing, probes and dashboard polling never affect usage.
func (s *Server) observeInference(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint := ""
		if r.Method == http.MethodPost {
			switch r.URL.Path {
			case "/v1/chat/completions":
				endpoint = "llm"
			case "/v1/systemone":
				endpoint = "jev"
			}
		}
		if s.dashboard == nil || endpoint == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, observed := s.dashboard.Begin(r.Context(), endpoint)
		writer := &observedWriter{ResponseWriter: w, status: 200}
		defer func() { observed.Finish(writer.status, r.Context().Err() != nil, writer.failed) }()
		next.ServeHTTP(writer, r.WithContext(ctx))
	})
}

type observedWriter struct {
	http.ResponseWriter
	status        int
	wrote, failed bool
}

func (w *observedWriter) WriteHeader(status int) {
	if !w.wrote && status >= 200 {
		w.status, w.wrote = status, true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.failed = true
	}
	return n, err
}

func (w *observedWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.failed = true
	}
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
