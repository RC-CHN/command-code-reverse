# Changelog

All notable changes to commandcode-proxy are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
versioning follows [SemVer](https://semver.org/).

## [v0.1.0] - 2026-08-25

Initial release.

### Added

- **OpenAI-compatible surface**: `POST /v1/chat/completions` (SSE streaming +
  aggregated non-streaming), `GET /v1/models`, `GET /v1/credits`.
- **Wire-accurate upstream client** for Command Code `/alpha/generate`
  (reverse-engineered from command-code@1.32.2): exact CLI header set,
  NDJSON event translation, tool calls/results, images, reasoning content,
  prefix-cache token passthrough.
- **Fill-first key pool** with per-key circuit breakers (insufficient credits
  1h, rate limit 1m, exponential backoff for 5xx) to maximize cache locality.
- **Error semantics**: in-band terminal markers (premium_credits_exhausted /
  model_not_in_plan / insufficient credits) map to explicit 402/403 responses;
  internal events (start-step, provider-metadata) never leak downstream.
- **Per-key sessions** with 12h+1h jitter rotation and fake project slugs.
- **Replayable fingerprint reporting**: deterministic HMAC derivation from a
  seed (k8s-reschedule-safe), persisted state file, or live collection.
- **CLI version tracking**: npm registry refresh every 24h, pin override.
- **Observability**: Prometheus `/metrics` (requests, tokens, latency, cost in
  micro-dollars), structured slog logging, `/healthz` + cached-probe `/readyz`.
- **Deployment**: multi-stage distroless image (nonroot), docker-compose,
  graceful SIGTERM drain, GitHub Actions CI (vet + race tests + lint +
  multi-arch build) and tag-triggered releases (binaries + GHCR images).
- **Version stamping**: `--version` flag, `Server` response header,
  `GET /version` endpoint; ldflags injection via CI/Dockerfile.

### Notes

- Zero third-party Go dependencies (standard library only).
- Anthropic protocol is intentionally not supported.
- Billing enforcement lives upstream; the proxy never bypasses quotas.
