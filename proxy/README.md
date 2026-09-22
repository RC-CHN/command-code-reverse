# commandcode-proxy

Go 版 Command Code → OpenAI 兼容代理。把 `POST /alpha/generate`（NDJSON streaming）
包装成标准 OpenAI 接口，可直接对接任何 OpenAI SDK / 生态工具。

协议依据：`analysis/v1.32.2/README.md`（command-code@1.32.2 静态逆向 + 实测）。
最新核对：[command-code@1.62.1](../analysis/v1.62.1/README.md)（聊天主协议未变；已更新离线版本兜底、8 个新增聊天模型，并从静态目录移除退役的 LongCat 免费版）。
现已提供原生 Jev 决策端点 `/v1/systemone`；Responses 端点仍待接入。
此前的 [1.51.3 协议分析](../analysis/v1.51.3/README.md) 和 [真实上游验证](../analysis/v1.51.3/LIVE.md) 保留作为历史记录。
零第三方依赖（标准库 only）。

## 功能

- `POST /v1/chat/completions` — 流式（SSE）+ 非流式，工具调用、思考链（reasoning_content）、缓存命中透传
- `POST /v1/systemone` — Jev 原生 JSON 决策接口，支持 noul / choice / score；独立熔断
- `GET /v1/models` — 动态拉取 `/provider/v1/models`（5min 缓存 + 静态兜底）
- `GET /v1/credits` — 透传 billing/credits + subscriptions（5h/weekly 窗口可见）
- `GET /healthz` / `GET /readyz` — liveness / readiness（readiness 带 30s 缓存的上游 whoami 探针）
- `GET /metrics` — Prometheus 文本格式（请求数/token/延迟/成本微美元）
- 多 key 池：fill-first + 熔断（欠费/凭据失效 1h、限流 1m；5xx/网络故障不熔断、不切换 key）
- 指纹上报（可选）：seed 确定性派生 / 状态文件重放 / 现场采集 三模式
- CLI 版本号：npm registry 24h 自动刷新，可 pin
- `CMD_ZDR=1`：上游请求统一携带零数据留存路由要求；不支持时明确报错，不降级重试
- 流内错误标记（premium_credits_exhausted / model_not_in_plan / insufficient credits）→ 明确 402/403
- 组织消费上限 `USAGE_EXCEEDED` → 403 `spend_limit_error`，保留上游消息，不切换账户、不熔断 key
- 字符串和对象形式的流错误均显式返回；兼容 flat/nested 缓存 token 统计
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

## Jev / System One

`POST /v1/systemone` 使用与聊天相同的鉴权，转发到上游 `/provider/v1/systemone`。
接受 `typesafe/jev`、`jev`、`jev-latest`、`typesafe-ai/jev`，统一发送 `typesafe/jev`。
返回原生 JSON（`model/answers/usage` 及扩展字段）；Jev 请求应使用本端点。
聊天端点收到这些模型名会返回 400 `unsupported_endpoint`。

```bash
curl http://localhost:3050/v1/systemone \
  -H "Authorization: Bearer $PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"jev","state":{"value":2},"questions":{
    "greater_than_one":{"type":"noul","instructions":"Is value greater than 1?"}
  }}'
```

沿用 CLI 的 1–20 个具名问题限制。`state`、`instructions` 接受字符串、对象、数组或 null；
原始 JSON 数字和扩展字段保留。`noul.criteria` 可省略，提供时只接受 `true/false`；
`choice.criteria` 为选项对象；`score.criteria` 为 2–10 个有序等级。接口不支持 `stream` 参数。

每个 Choice **默认最多 20 项**，这是代理的保守限制，不是 CLI 已确认的限制。
启动时设置 `JEV_UNLOCK_MAX_OPTIONS=true`，重启后允许最多 **255 项**。
开关关闭且请求有 21–255 项时返回：

```json
{"error":{"type":"invalid_request_error","code":"jev_choice_options_locked","param":"questions.pick.criteria","message":"Choice has 21 options; the proxy limit is 20. Set JEV_UNLOCK_MAX_OPTIONS=true and restart the proxy to allow up to 255"}}
```

无论是否解锁，超过 255 项均返回 422 `jev_choice_options_exceeded`。
这两种错误均在本地拦截，不调用上游、不截断选项、不切换 key。下游参数或请求头不能解锁。

managed 模式按配置顺序选择 key，策略与聊天一致，但 **所有熔断、冷却和半开探测状态独立**。
凭证失效、欠费、429 限流会尝试下一个未尝试的 key；每个 key 在单次请求中最多调用一次。
欠费/凭证失效冷却 1h，限流采用有效 `Retry-After`（秒数或 HTTP 日期），缺失时 1m。
套餐错误（`upgrade_required` / `MODEL_NOT_IN_PLAN`）可换 key，但不熔断。
passthrough 只使用调用者的 key，不轮换到配置池。

| 错误 | 下游返回 | 轮换 / 熔断 |
|---|---|---|
| 本地 JSON 损坏 / 请求体超限 | 400 `invalid_json` / 413 `request_body_too_large` | 不请求上游 |
| 题目结构、选项限制错误 | 422，带 `code` 和 `param` | 不请求上游 |
| 上游 400/404/408/413/415/422 等参数错误 | 保留状态、错误码、消息和字段位置 | 不轮换、不熔断 |
| 上游 401 / 明确鉴权失败的 403 | key 用尽后 502 `upstream_auth_error` | 轮换，独立熔断 |
| 上游 402 / `insufficient_credits` | 402 `billing_error` | 轮换，独立熔断 |
| 套餐不支持 | 403 `plan_error` | 轮换，不熔断 |
| 其他 403 权限错误 | 403 `permission_error` | 不轮换、不熔断 |
| ZDR 无可用路由 / `USAGE_EXCEEDED` | 403 `zdr_error` / `spend_limit_error` | 不轮换、不熔断、不降级 |
| 上游 429 | 429 `rate_limit_error`，保留有效 `Retry-After` | 轮换，独立冷却 |
| 当前没有可选 key | 503 `keypool_unavailable`，带最短等待时间 | 不请求上游 |
| 上游 5xx（含 529 过载） | 保留状态和错误码；503/529 保留有效 `Retry-After` | 不轮换、不熔断 |
| 本地超时 / 网络故障 | 504 `upstream_timeout` / 502 `upstream_connection_error` | 不轮换、不熔断 |
| 上游 JSON 非法、缺少答案 / 响应超限 | 502 `upstream_invalid_response` / `upstream_response_too_large` | 不轮换、不熔断 |

默认总超时 90s，覆盖所有 key 尝试及响应读取；客户端断开会取消上游。
响应完整读取、校验后才返回 200；0 概率或 0 分是合法结果。
`CMD_ZDR` 同样生效。日志仅记录模型、状态、耗时、错误码，不记录 state/questions/answers。
用量进入现有指标；没有费用字段时不推算费用。不会缓存决策结果。

## System 缓存扩展

普通 system/developer 内容仍转成 `params.system` 字符串。需要显式缓存边界时，可在文本块设置：

```json
{"model":"meta/muse-spark-1.3","messages":[
  {"role":"system","content":[
    {"type":"text","text":"Stable instructions","cache_control":{"type":"ephemeral"}},
    {"type":"text","text":"Current context"}
  ]},
  {"role":"user","content":"Hello"}
]}
```

带标记时代理保留 system 分段数组（依据 1.51.3 CLI）；没有标记的数组仍展平成字符串。
可选请求字段 `"prompt_cache":"off"` 映射为上游顶层 `promptCache`，不设置则省略。
这是缓存策略提示：实测 DeepSeek 在 `off` 下仍会返回模型侧缓存命中，不能保证零缓存。
这些属于代理扩展，实际缓存支持取决于上游模型；固定旧版上游时应使用普通字符串。
模型目录包含不同套餐的模型，列出不代表当前账户有调用权限。

`tool_choice: "none"` 通过空工具列表表达；上游不接受 `{type:"none"}`。
未提供 system/developer 或内容为空时，代理自动发送 `params.system: " "`，
避免上游注入默认 CLI 提示（此前实测约 7.6k 输入 token）。
非空 system/developer 内容和显式缓存分段照常保留。
请求头 project slug 和请求体工作目录使用相同的派生 Linux 路径。
跨进程重启需要配置固定 `FINGERPRINT_SEED` 才能保持会话身份；这与
`FINGERPRINT_ENABLED` 是否启用指纹上报相互独立。

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
| `CMD_ZDR` | | `0` | `1`/`true` 开启零数据留存路由要求，`0`/`false` 关闭；非法值启动失败 |

设置 `CMD_ZDR=1` 后重启代理，聊天及其他上游 API 请求统一携带
`x-cmd-zdr: 1`；managed 和 passthrough 两种鉴权模式均生效，下游不能关闭此要求。
上游返回 `CMD_ZDR_NO_PROVIDERS`（或错误类型 `cmd_zdr_no_providers`）时，
代理返回 403 `zdr_error`，保留错误码和消息，不切换 key、不熔断，也不会去掉 ZDR 头重试。
若 SSE 已开始，则通过流内错误事件返回相同错误信息。
模型目录可能仍包含不支持 ZDR 的条目，实际路由由上游判断。
固定发送的 `x-taste-learning: false` 仅关闭 taste learning，不能代替 ZDR。
本开关传达上游路由要求，不改变本地日志或可选指纹上报配置。
已做少量真实请求对照：[ZDR 路由实测](../analysis/v1.62.1/README.md#5-zdr代理已接入路由策略已实测)；
上游实测返回的 422 策略拒绝也统一映射为上述 403。

### 下游鉴权

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `AUTH_MODE` | | `managed` | `managed`：下游用 `PROXY_API_KEY` 鉴权，CC key 留在服务端（k8s 推荐）；`passthrough`：下游直接传自己的 CC key（兼容旧代理） |
| `PROXY_API_KEY` | managed 必填 | — | 下游客户端的鉴权 key。**永远不会被发往上游** |

### 指纹（可选伪装，协议不强制）

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `FINGERPRINT_ENABLED` | | `false` | 开启后启动时上报一次设备指纹，之后每 8–10h 心搏；当前只为池中第一个 key 上报 |
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
| `MAX_BODY_BYTES` | | `67108864` (64 MiB) | 启动时读取的请求体字节上限，包含 base64 图片和 JSON；超限返回 413 |
| `JEV_UNLOCK_MAX_OPTIONS` | | `false` | 每个 Choice 默认 20 项；`true`/`1` 放宽至 255 项；非法布尔值启动失败 |
| `JEV_TIMEOUT_SECONDS` | | `90` | Jev 请求总超时（含轮换和读取）；正整数 |
| `JEV_MAX_RESPONSE_BYTES` | | `8388608` (8 MiB) | Jev 完整 JSON 响应上限；正整数，超限返回 502 |
| `MAX_TOKENS_CLAMP` | | `200000` | max_tokens 钳制上限 |
| `STREAM_IDLE_TIMEOUT_SECONDS` | | `30` | 流式事件空闲超时（超时取消上游请求；连续 3 次提示压缩上下文） |
| `NONSTREAM_IDLE_TIMEOUT_SECONDS` | | `90` | 非流式空闲超时 |

多图请求的大小按编码后的整个 JSON 计算，base64 数据通常比原图片大约三分之一。
可在启动环境中设置 `MAX_BODY_BYTES=134217728`，将上限提高到 128 MiB。
旧 `.env` 中显式设置的 `MAX_BODY_BYTES=10485760` 会继续覆盖新默认值，需修改后重启。
此入口限制与上游 NDJSON 单行读取独立；上游 `start-step` 回显不再受原 4 MiB 单行限制。

### 可观测性

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `LOG_FORMAT` | | `json` | `json` / `text` |
| `LOG_LEVEL` | | `info` | `debug` / `info` / `warn` / `error` |
| `METRICS_ENABLED` | | `true` | Prometheus `/metrics` 端点开关 |
| `COST_HEADER_ENABLED` | | `false` | 非流式响应附带 `x-commandcode-cost` 成本头 |

## 设计文档

`references/design/README.md`（不进 git）。
