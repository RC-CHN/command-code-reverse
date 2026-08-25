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

全部通过环境变量配置（支持 `.env` 文件，真环境变量优先）。完整注释版见
`../.env.example`。

### 上游凭据

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `COMMAND_CODE_API_KEY` | ✅ | — | 上游 API key（[studio 页面](https://commandcode.ai/studio) 获取）。逗号分隔多个组成 key 池，**fill-first** 策略：打满第一个 key 直到限流/熔断才溢出到下一个，最大化缓存命中 |

### 上游连接

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `COMMAND_CODE_API_BASE` | | `https://api.commandcode.ai` | API base；staging 环境用 `https://staging-api.commandcode.ai` |
| `COMMAND_CODE_VERSION` | | 空 | 上报的 CLI 版本号初始值；空 = 从 npm registry 每 24h 自动刷新 |
| `COMMAND_CODE_VERSION_PIN` | | 空 | 固定版本号，设置后禁用自动刷新（服务端强制最低版本时的兜底） |

### 下游鉴权

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `AUTH_MODE` | | `managed` | `managed`：下游用 `PROXY_API_KEY` 鉴权，CC key 留在服务端（k8s 推荐）；`passthrough`：下游直接传自己的 CC key（兼容旧代理） |
| `PROXY_API_KEY` | managed 必填 | — | 下游客户端的鉴权 key。**永远不会被发往上游** |

### 指纹（可选伪装，协议不强制）

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `FINGERPRINT_ENABLED` | | `false` | 开启后启动时上报一次设备指纹 + 8h±2h 心搏 |
| `FINGERPRINT_SEED` | | 空 | 确定性派生种子（放 k8s Secret）。所有组件由 `HMAC-SHA256(seed, component)` 派生，pod 重调度指纹不变，无需 PVC。**优先级最高** |
| `FINGERPRINT_STATE_FILE` | | `./data/fingerprint.json` | 采集一次→持久化→永久重放模式（挂 PVC）。仅 seed 为空时生效 |

### 服务器

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `HOST` / `PORT` | | `0.0.0.0` / `3050` | 监听地址 |
| `SHUTDOWN_DRAIN_SECONDS` | | `30` | SIGTERM 后在途流式请求的排空窗口 |

### 防护与超时

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `MAX_BODY_BYTES` | | `10485760` (10MB) | 请求体上限 |
| `MAX_TOKENS_CLAMP` | | `200000` | max_tokens 钳制上限 |
| `STREAM_IDLE_TIMEOUT_SECONDS` | | `30` | 流式事件空闲超时（超时取消上游请求；连续 3 次提示压缩上下文） |
| `NONSTREAM_IDLE_TIMEOUT_SECONDS` | | `90` | 非流式空闲超时 |

### 可观测性

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `LOG_FORMAT` | | `json` | `json` / `text` |
| `LOG_LEVEL` | | `info` | `debug` / `info` / `warn` / `error` |
| `METRICS_ENABLED` | | `true` | Prometheus `/metrics` 端点开关 |
| `COST_HEADER_ENABLED` | | `false` | 非流式响应附带 `x-commandcode-cost` 成本头 |

## 设计文档

`references/design/README.md`（不进 git）。
