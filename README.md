# command-code-reverse

Reverse engineering of the [Command Code](https://commandcode.ai) CLI
protocol (`command-code@1.32.2`), plus a production-grade Go proxy that
exposes its upstream API as an OpenAI-compatible interface.

[中文文档](./README.zh-CN.md)

## What's inside

| Path | Description |
|---|---|
| `analysis/v1.32.2/` | Protocol analysis (Chinese): endpoints, auth, wire format, streaming events, plan enforcement, fingerprint algorithm, cache behavior — verified live |
| `proxy/` | **commandcode-proxy** — Go service bridging `POST /alpha/generate` (NDJSON streaming) to `/v1/chat/completions` (OpenAI SSE) |
| `.github/workflows/` | CI (vet + race tests + lint + image build) and tag-triggered releases |

## Why

Command Code sells multi-model subscriptions behind a proprietary CLI.
Its `/alpha/generate` endpoint speaks NDJSON, requires a dozen disguise
headers, and returns billing/plan failures as in-band stream markers —
incompatible with the OpenAI ecosystem out of the box. This proxy makes
any OpenAI SDK work against it, with proper error semantics
(`402 insufficient credits`, `403 model not in plan`), key pooling,
and observability.

## Quick start

```bash
cp .env.example .env   # set COMMAND_CODE_API_KEY and PROXY_API_KEY
cd proxy && go run ./cmd/commandcode-proxy
```

```bash
curl -N http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","stream":true,
       "messages":[{"role":"user","content":"hi"}]}'
```

Docker / k8s / full config reference: see [`proxy/README.md`](./proxy/README.md).

## Design highlights

- **Zero third-party dependencies** — Go standard library only.
- **Fill-first key pool** with per-key circuit breakers, maximizing
  prefix-cache locality (verified: >98% cache hit on repeated prefixes).
- **Replayable device fingerprint** — deterministic HMAC derivation from a
  seed survives k8s pod reschedules; no PVC required.
- **Terminal marker mapping** — in-band billing/plan failures become
  explicit 402/403 responses instead of mysterious empty 429s.
- **Information hygiene** — internal upstream events (`start-step`,
  `provider-metadata`) are stripped; cost is surfaced via logs,
  Prometheus metrics, or an opt-in response header.
- **Graceful operations** — `/healthz`, cached-probe `/readyz`,
  `/metrics`, `/version`, SIGTERM drain for in-flight streams.

## Releasing

```bash
# update CHANGELOG.md, then:
git commit -m "chore(release): vX.Y.Z"
git tag vX.Y.Z && git push && git push origin vX.Y.Z
```

The release workflow publishes cross-platform binaries to GitHub
Releases and multi-arch images to GHCR.

## License

See [LICENSE](./LICENSE). This project is an interoperability study;
billing enforcement lives on the server side and is never bypassed.
