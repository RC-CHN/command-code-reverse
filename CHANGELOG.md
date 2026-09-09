# Changelog

All notable changes to commandcode-proxy are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
versioning follows [SemVer](https://semver.org/).

## [v0.1.6] - 2026-09-09

### Changed

- Revalidated the gateway protocol against `command-code@1.51.3`; added
  a versioned static analysis, package integrity/provenance, and a reproducible
  bundle comparison script. Updated the offline CLI version fallback.
- Added Qwen 3.8 Max 0902, LongCat 2.0, Gemini 3.8 Flash, Muse Spark 1.3
  and Contributor, and GPT-6 Astra to the fallback catalog. The withdrawn
  DeepSeek V4.1 Flash Beta remains excluded.

### Added

- Explicit system/developer text-block `cache_control: {type: "ephemeral"}`
  preserves system sections on the upstream wire; ordinary prompts keep the
  string form. Optional `prompt_cache: "off"` forwards the CLI cache-policy hint;
  live DeepSeek requests can still report provider-side cache hits.
- `USAGE_EXCEEDED` organization/model spend caps return 403
  `spend_limit_error`, preserving the code and message without rotating or
  circuit-breaking accounts.

### Fixed

- Missing or empty system/developer content now sends a single-space system
  placeholder, preventing upstream default CLI prompt injection. Non-empty
  prompts and explicit cache sections are preserved.
- String and object stream errors are surfaced for both streaming and
  non-streaming requests, including after partial output, instead of becoming
  empty 429s or successful partial completions.
- Nested cache/reasoning token details are recognized alongside legacy flat
  counters; explicit nested zeros take precedence without double-counting.
- Corrected obsolete README guidance about circuit-breaking upstream 5xx.
- Live testing found `tool_choice: "none"` is rejected by the upstream wire
  schema. It now sends an explicit empty tools list and omits tool_choice;
  tool-result follow-ups without a name complete successfully.
- Project slugs and `config.workingDir` now derive from the same stable Linux
  path, replacing the inconsistent Windows-derived slug/Linux body pair.
  Per-attempt request copies preserve caller data when pooled keys change.

### Validation

- Static npm bundle comparison, `go vet`, and full race regression tests.
- Opt-in live tests with both configured accounts: ordinary/SSE requests,
  conversation continuity, tools, cache sections, managed/passthrough auth,
  and process restart identity. Redacted results are under `analysis/v1.51.3/`.
- Confirmed that omitting system can inject the upstream CLI prompt (~7.6k
  input tokens in this sample), and that an unseeded process restart changes
  identity; a fixed test seed preserves it. Neither behavior was hidden by
  changing the user's .env.

## [v0.1.5] - 2026-09-02

### Fixed

- Upstream 5xx and transport failures no longer circuit-break an otherwise
  valid API key or spill the same service-wide outage across every account in
  the pool. Breakers are now reserved for key-specific failures (rate limits,
  exhausted credits, and rejected credentials), with a single half-open probe
  after cooldown to prevent a retry stampede.
- Error responses now preserve an upstream HTTP 5xx status. Transport failures
  remain 502, while a locally unavailable key pool is reported as 503 with a
  `keypool_unavailable` code and an accurate `Retry-After` value.

### Changed

- Refreshed compatibility data against `command-code@1.40.1`: the offline
  CLI-version fallback and static model catalog now include the current
  DeepSeek, Kimi, GLM, Qwen, and Tencent additions with context lengths.
  Retired Ox Alpha and MiniMax free aliases remain excluded.

## [v0.1.4] - 2026-08-27

### Changed

- Session identity is now derived from the conversation instead of wall-clock
  rotation. The upstream can reconstruct conversations by prefix-matching
  message histories, so timer-based rotation produced impossible sequences
  (IDs changing mid-conversation, interleaved conversations sharing one
  eternal per-key session). The conversation root — leading system context
  plus the first non-system message — feeds a stateless HMAC derivation:
  threadId is conversation-scoped (survives key spills), sessionId/slug are
  account-scoped (key+root). Stable from the very first request, fresh
  across conversations, zero stored state.
- Retry attempts inside one request now share a single W3C trace ID with a
  fresh span ID per attempt, mirroring the CLI's per-iteration trace spans.
  Previously every attempt rolled a fully random traceparent — a sequence
  no real CLI can emit.

## [v0.1.3] - 2026-08-26

### Fixed

- Tool-result messages without a `name` field (optional in OpenAI, sent by
  minimal clients like ReuleauxCoder) no longer fail the whole conversation
  with a misleading "expected one of user|assistant" 502. The upstream wire
  schema requires a non-empty `toolName` on every tool-result part, so the
  name is now resolved the way the CLI does: explicit name → lookup in the
  toolCallID→toolName map built from preceding assistant tool_calls →
  "unknown". Verified by replaying a real 97-message agent session against
  the live upstream: 502 before, 200 after.
- A 403 "Model/provider not recognized" (bare model names get the default
  `anthropic:` provider prefix upstream) no longer circuit-breaks a healthy
  key for an hour. Unknown model IDs are a request-shape problem: no pooled
  key will serve them, so the failure neither circuits nor rotates, and
  downstream clients get a truthful 404 `invalid_request_error`.

## [v0.1.2] - 2026-08-25

### Fixed

- A 403 `MODEL_NOT_IN_PLAN` response (model above the account's tier) no
  longer circuit-breaks an otherwise healthy key for an hour. The key stays
  in rotation — plan mismatch is per-model, not per-key — and downstream
  clients now get a truthful `403 plan_error` instead of a misleading
  "Upstream rejected the CC API key".
- Remote image URLs (`image_url` with an `https://` URL) are rejected with a
  clear 400 error instead of being silently dropped upstream, where the
  model would answer "I can't see the image".

### Added

- `GET /v1/models/{id}` retrieve endpoint (OpenAI shape, wildcard route for
  slash-bearing model IDs, OpenAI-shaped 404 for unknown models).
- `GET /v1/models` entries now carry the upstream display `name` and
  `context_length` as additive fields on the OpenAI model object.

### Verified

- Multimodal input end-to-end: `image_url` data URLs convert to the upstream
  wire format and vision models (e.g. `deepseek/deepseek-v4-flash-vision-exp`)
  actually receive the image. Non-vision models silently ignore images —
  clients must pick a vision-capable model.

## [v0.1.1] - 2026-08-25

### Fixed

- Truncated upstream streams (EOF without a finish event) now always deliver
  a terminal SSE chunk with finish_reason and usage — clients no longer hang
  waiting for one.
- `recordFailure` is wired into every failure path (upstream error, terminal
  marker, empty response, timeout); it was dead code in v0.1.0.

### Changed

- Removed dead code: write-only `lastEvent`, unused `Tracker.hc` client,
  never-set error `code` field, and parsed-but-never-consumed wire fields
  (`parallel_tool_calls`, `marketCost`, `totalTokens` — verified live to
  always equal input+output).
- Deduplicated: shared `streamState.absorb` for stream bookkeeping,
  `Client.setAuthHeaders` for the repeated auth header block,
  `toolCallArgs` for argument normalization, and `strconv.Itoa` in place of
  a hand-rolled helper.
- `usage.completion_tokens_details.reasoning_tokens` is now mapped from the
  upstream `reasoningTokens` (OpenAI-standard field).

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
