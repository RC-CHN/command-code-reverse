# commandcode-proxy

Go 版 Command Code → OpenAI 兼容代理。把 `POST /alpha/generate`（NDJSON streaming）
包装成标准 OpenAI 接口，可直接对接任何 OpenAI SDK / 生态工具。

协议依据：`analysis/v1.32.2/README.md`（command-code@1.32.2 静态逆向 + 实测）。
零第三方依赖（标准库 only）。

## 功能

- `POST /v1/chat/completions` — 流式（SSE）+ 非流式，工具调用、思考链（reasoning_content）、缓存命中透传
- `GET /v1/models` — 动态拉取 `/provider/v1/models`（5min 缓存 + 静态兜底）
- `GET /v1/credits` — 透传 billing/credits + subscriptions（5h/weekly 窗口可见）
- `GET /healthz` / `GET /readyz` — liveness / readiness（readiness 带 30s 缓存的上游 whoami 探针）
- `GET /metrics` — Prometheus 文本格式（请求数/token/延迟/成本微美元）
- 多 key 池：fill-first + 熔断（欠费 1h / 限流 1m / 5xx 指数退避）
- 指纹上报（可选）：seed 确定性派生 / 状态文件重放 / 现场采集 三模式
- CLI 版本号：npm registry 24h 自动刷新，可 pin
- 流内错误标记（premium_credits_exhausted / model_not_in_plan / insufficient credits）→ 明确 402/403
- 内部事件（start-step / provider-metadata）剥离，成本进日志+指标，可选 `x-commandcode-cost` 响应头

## 快速开始

```bash
cp ../.env.example ../.env   # 填入 COMMAND_CODE_API_KEY 和 PROXY_API_KEY
go run ./cmd/commandcode-proxy
```

```bash
curl -N http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","stream":true,
       "messages":[{"role":"user","content":"hi"}]}'
```

## Docker

```bash
docker build -t commandcode-proxy .
docker run --env-file ../.env -p 3050:3050 commandcode-proxy
# 或 docker compose up（compose 读取 ../.env）
```

## k8s 要点

- 指纹放 Secret：`FINGERPRINT_SEED=<random>` → pod 重调度指纹不变，无需 PVC
- 探针：liveness `/healthz`，readiness `/readyz`
- 优雅退出：SIGTERM 后排空在途流（`SHUTDOWN_DRAIN_SECONDS`）
- 单副本起步；多副本缓存亲和靠 Service `sessionAffinity`

## 配置

见 `../.env.example`，全部环境变量都有注释。关键项：

| 变量 | 说明 |
|---|---|
| `COMMAND_CODE_API_KEY` | 上游 key，逗号分隔成池（fill-first） |
| `AUTH_MODE` | `managed`（下游用 PROXY_API_KEY）/ `passthrough`（透传下游 CC key） |
| `PROXY_API_KEY` | managed 模式的下游鉴权 key |
| `FINGERPRINT_ENABLED/SEED/STATE_FILE` | 指纹开关与重放模式 |
| `COMMAND_CODE_VERSION_PIN` | 固定 CLI 版本（禁用自动刷新） |

## 设计文档

`references/design/README.md`（不进 git）。
