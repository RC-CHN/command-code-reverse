// commandcode-proxy exposes Command Code's /alpha/generate as an
// OpenAI-compatible API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/config"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/fingerprint"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/logging"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/metrics"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/server"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/version"
)

// buildVersion is stamped by -ldflags "-X main.buildVersion=..." at release
// time (see .github/workflows/release.yml and the Dockerfile).
var buildVersion = "dev"

func main() {
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println("commandcode-proxy", buildVersion)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}
	logging.Init(cfg.LogFormat, cfg.LogLevel)

	versionTracker := version.New(cfg.VersionPin, cfg.Version)

	client := commandcode.NewClient(cfg.APIBase, versionTracker.String, nil)

	// Background lifecycle for version refresh + fingerprint reporting.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go versionTracker.Start(bgCtx)

	if cfg.FingerprintEnabled {
		fp, err := fingerprint.Resolve(cfg.FingerprintSeed, cfg.FingerprintStateFile)
		if err != nil {
			slog.Warn("fingerprint resolve failed, continuing without it", "error", err)
		} else {
			slog.Info("fingerprint resolved", "thumbmark", fp.Thumbmark[:12])
			go fingerprint.NewReporter(client, cfg.APIKeys[0], fp).Start(bgCtx)
		}
	}

	// Fill-first key pool with circuit breaking. Passthrough hints bypass
	// pooling inside keypool.Generate.
	sessions := session.NewStore()
	upstream := keypool.New(client, sessions, cfg.APIKeys, keypool.BreakerPolicy{})

	// Model catalog fetcher: managed uses the pool key, passthrough uses the
	// downstream key from the request.
	pickKey := func(downstreamKey string) string {
		if cfg.AuthMode == config.AuthPassthrough && downstreamKey != "" {
			return downstreamKey
		}
		return cfg.APIKeys[0]
	}
	fetchModels := func(ctx context.Context, downstreamKey string) ([]commandcode.ModelInfo, error) {
		return client.ProviderModels(ctx, pickKey(downstreamKey))
	}

	// Metrics registry (disabled via METRICS_ENABLED=false).
	var recorder server.MetricsRecorder
	var renderMetrics func() string
	if cfg.MetricsEnabled {
		reg := metrics.New()
		recorder = reg
		renderMetrics = reg.Render
	}

	deps := server.Deps{
		Upstream:    upstream,
		Version:     buildVersion,
		CCVersion:   versionTracker.String,
		FetchModels: fetchModels,
		FetchCredits: func(ctx context.Context, downstreamKey string) (json.RawMessage, json.RawMessage, error) {
			return client.Billing(ctx, pickKey(downstreamKey))
		},
		Probe: func(ctx context.Context) error {
			return client.Whoami(ctx, cfg.APIKeys[0])
		},
		Metrics: recorder,
	}

	handler := server.New(cfg, deps, renderMetrics)
	httpServer := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           versionHeader(handler),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("commandcode-proxy listening",
			"addr", cfg.Addr(),
			"version", buildVersion,
			"authMode", cfg.AuthMode,
			"apiKeys", len(cfg.APIKeys),
			"apiBase", cfg.APIBase,
			"ccVersion", versionTracker.String(),
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		slog.Error("server failed", "error", err)
		os.Exit(1)
	case sig := <-sigCh:
		slog.Info("shutdown signal received, draining", "signal", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownDrain)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	slog.Info("commandcode-proxy stopped")
}

// versionHeader stamps every response with the proxy version.
func versionHeader(next http.Handler) http.Handler {
	server := "commandcode-proxy/" + buildVersion
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", server)
		next.ServeHTTP(w, r)
	})
}
