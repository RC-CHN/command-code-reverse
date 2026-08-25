# Command Code v1.32.2 逆向分析

> 分析对象：npm 包 `command-code@1.32.2`（官方 GitHub 仓库仅有 README，闭源，npm 是唯一代码来源）。
> 发布的 CLI 是压缩打包后的单文件 bundle（约 2.5MB），美化后可读。

## 一、整体架构

Command Code 是一个终端 coding agent（Ink/React TUI），核心模式是**客户端壳 + 服务端推理代理**：

- CLI 本身不直接调 OpenAI/Anthropic，而是把对话整体 POST 到自家后端 `https://api.commandcode.ai`，由后端统一转发给各家上游（openrouter、vercel-ai-gateway、anthropic 等）。
- 所有计费、plan 校验、模型权限都在服务端完成；客户端只做展示层的预检。
- 三类请求路径（重要区分）：

| 模式 | 请求去向 |
| --- | --- |
| Command 内置/gateway 模型 | `/alpha/generate` |
| Copilot/Codex/Anthropic OAuth 通道 | 通常仍走 Command 后端，附带 `x-oauth-token` + `x-oauth-provider` 头借道转发 |
| 自定义 API provider / BYOK | AI SDK 直接请求用户配置的 provider baseURL，完全绕过 `/alpha/generate` |

- 主推理协议是 **NDJSON streaming**（按行分隔的 JSON），不是 SSE；sandbox 功能另有 WebSocket `/alpha/sandbox/stream`。

## 二、认证流程

### 登录（浏览器 OAuth）

1. CLI 在 **127.0.0.1 的 5959 端口起尝试，最多连续试 10 个端口（5959–5968）** 起本地回调服务器（注意：8085 是 MCP OAuth 等另一套流程用的）。
2. 浏览器打开 `https://commandcode.ai/studio/auth/cli?callback=http://localhost:<port>/callback&state=<state>`。
3. state 是 32 字节随机值；回调只校验 state，**不会再由 CLI 调 `/alpha/whoami` 验证**；监听仅 127.0.0.1，超时 120 秒。
4. 拿到的 key 写入本地 auth 文件。

### 手动 API key

- 粘贴 key → CLI 调 `GET /alpha/whoami`（Bearer）验证，401 即无效；通过后写入 auth 文件。
- Studio 网页上生成的 API key 可以走这条路，与 OAuth 拿到的 key 完全等效。

### 存储与优先级

- 取 key 优先级：环境变量 `COMMAND_CODE_API_KEY` > auth 文件。
- 按环境分别存储：`auth.json`（prod）/ `auth.staging.json` / `auth.local.json`，均位于 `~/.commandcode/`。
- 文件是 **0600 明文 JSON**，目录 0700。
- `cmd auth status` 基本只检查 key 是否存在，不在线验证；`logout` 只删本地字段，**不会在服务端吊销 key**。

### 请求头

每个 API 请求携带：

- `Authorization: Bearer <apiKey>`
- `x-command-code-version`：CLI 版本
- `x-cli-environment`：prod/staging
- `x-project-slug`：项目标识
- `x-taste-learning`：是否开启 taste 学习
- `x-session-id`：会话 ID
- `User-Agent: cli`
- 可选：`x-cmd-zdr: 1`（零数据保留）、`x-oauth-token` + `x-oauth-provider`（OAuth 借道）、`x-cmd-provider-deepseek-internal: 1`（疑似内部测试通道，值得探）

## 三、设备指纹

- **上报时机**：每个 CLI 进程启动时最多尝试一次（不是"首次安装才上报"）；仅当已有 API key 且 telemetry 开启时才发送，POST `/alpha/fingerprint/record`。发送失败静默忽略。
- **thumbmark 算法**（以源码为准）：

  ```
  namespace = "command-code:device-fingerprint:v1"
  components_for_thumbmark = [machineId,
                              排序去重后的 MAC 列表 join(","),
                              hostname   (仅当 machineId 缺失),
                              cpuModel   (仅当 machineId 缺失)]
  thumbmark = sha256(namespace + "\0machine\0" + components.join("|"))
  ```

  即 thumbmark 主要由 **machineId + 排序后的 MAC 集合**生成；machineId 缺失时才补 hostname 和 CPU model。
- **components 字段**：`machineIdHash`、`macHashes`、`osUserHash`、`hostnameHash`、`gitEmailHash` 等字符串组件各自用 `sha256(namespace + "\0" + 值.toLowerCase())` 独立哈希；用户名、Git 邮箱、OS、内存等**只在 components 中上报，不参与 thumbmark**。
- **隐私注意**：namespace 是公开的常量，所以 Git 邮箱、用户名、hostname 的哈希**不等于匿名化**，可被字典枚举反推。
- MAC 集合变化会改变 thumbmark；容器/虚拟机中 machine-id、MAC 可能漂移或复用。
- `CMD_ZDR` **不**禁用指纹（只是请求多带一个 `x-cmd-zdr: 1`）；真正关闭方式是 `DO_NOT_TRACK=1` 或配置文件 `"telemetry": false`。
- 指纹**不是认证凭据**，客户端也不用它决定 API 是否可调用。"大量不同机器可能触发风控"目前只是推测，npm 客户端代码无法证明——代理实现里指纹一致性属于可选伪装，不是协议必需。

## 四、核心 API 端点

| 路由 | 用途 |
| --- | --- |
| `POST /alpha/generate` | **主聊天/补全流（NDJSON streaming）**，所有 agent 对话都走这里 |
| `POST /alpha/agent/generate` | 生成自定义 agent 定义 |
| `GET /alpha/whoami` | 鉴权检查，返回用户 + 组织信息 |
| `GET /alpha/billing/credits` | 余额：`{purchasedCredits, freeCredits, planId}` |
| `GET /alpha/billing/subscriptions` | 订阅：`{planId, status, currentPeriodStart...}` |
| `GET /alpha/usage/summary` | 用量统计（`?orgId=&since=`） |
| `POST /alpha/fingerprint/record` | 机器指纹上报 |
| `POST /alpha/learn`、`/alpha/taste/:slug` | taste 学习 |
| `/alpha/share/*`、`/alpha/namespaces`、`/alpha/lifecycle-events` | 分享/命名空间/埋点 |
| `/alpha/sandbox/start` + WS `/alpha/sandbox/stream` | 云端沙箱 |

## 五、主聊天请求格式（POST /alpha/generate）

```json
{
  "config": {
    "workingDir": "...",
    "date": "YYYY-MM-DD",
    "environment": "...",
    "structure": ["...目录结构..."],
    "isGitRepo": true,
    "currentBranch": "...",
    "mainBranch": "...",
    "gitStatus": "...",
    "recentCommits": ["...最近三条..."]
  },
  "memory": null,
  "taste": null,
  "skills": null,
  "permissionMode": "auto-accept|plan|standard",
  "threadId": "...（仅当合法 UUID 才发送）",
  "mode": "...",
  "params": {
    "model": "deepseek/deepseek-v4-flash",
    "messages": ["... wire 格式消息 ..."],
    "tools": ["... 工具 schema ..."],
    "system": "...",
    "max_tokens": 64000,
    "stream": true,
    "temperature": 0.0,
    "reasoning_effort": "low|medium|high|xhigh|max"
  }
}
```

要点：

- **permissionMode 是线上 wire 值**，本地模式发送前映射（`toWirePermissionMode`，源码实锤）：

  | 本地模式 | wire 值 |
  | --- | --- |
  | bypass | auto-accept |
  | auto-accept | auto-accept |
  | plan | plan |
  | default | standard |
  | dont-ask | standard |

- `threadId` 只有通过 UUID 校验才发送，否则省略。
- `temperature`、`reasoning_effort` 为可选字段。
- 会发送完整 messages、system prompt、工具 schema；config 里还带工作目录、目录结构、Git 状态、当前/主分支、最近三条 commit。

### 响应：NDJSON streaming

上游返回**按行分隔的 JSON（NDJSON），不是 SSE**——没有 `data:` framing：

```
{"type":"text-delta",...}\n
{"type":"reasoning-delta",...}\n
{"type":"tool-call",...}\n
{"type":"finish",...}\n
```

流事件至少包括：`text-delta`、`reasoning-start/delta/end`、`tool-call`、`tool-result`、`finish`、`error`、`abort`。
代理可以对外转换成 SSE，但上游协议本身必须按行解析。

错误不一定走 HTTP 状态码，也可能**嵌在流内**，客户端识别的终止标记有：
`premium_credits_exhausted`、`model_not_in_plan`、`insufficient credits`。

### 重试行为（客户端）

- 可重试状态：网络错误、408、429、5xx。
- 上层最多进行 10 次模型调用尝试；已产生输出后的中途断流最多重启 3 次，旧的可见内容会被丢弃。
- `pause_turn` 会自动发起最多 5 次额外 continuation。

## 六、订阅/plan 校验逻辑（重点）

### 服务端——真正的闸门

- 额度耗尽：`400` + 响应体包含 `"insufficient credits"`
- 模型越权 / premium 额度耗尽：流内终止标记 `model_not_in_plan` / `premium_credits_exhausted`
- 频率窗口：触发后提示 "You've reached your {window} usage limit"，附带 reset 时间

### 客户端——只是 UX 预检，且有放行口子

`evaluateModelAccess` 的判定顺序：

```js
if (purchasedCredits > 0 || freeCredits > 0) return 放行;   // 有任何余额 → 全部模型可用
if (!planId) return 放行;                                   // 没拉到 plan → 放行
// 否则才按 plan 表检查模型类别
```

billing 信息拉取失败时 `fetchFailed → 放行`（fail-open）。
结论：**改客户端只能绕过提示，绕不过计费**；但"余额"和"订阅"是两套并行体系——只要有任意余额（包括免费额度），plan 的模型限制就形同虚设。

### Plan 表

月额度（美元）：

| planId | 月额度 | 显示名 |
| --- | --- | --- |
| individual-go | 10 | Go |
| individual-provider | 15 | Provider |
| individual-pro | 30 | Pro |
| teams-pro | 40 | Teams Pro |
| individual-goat | 70 | GOAT |
| individual-pro-v1 | 80 | Pro（旧版） |
| individual-max | 150 | Max |
| individual-ultra | 300 | Ultra |

模型权限（模型分 standard / premium 两类）：

- **Go / GOAT**：仅 standard；Go 额外拉黑 muse-spark、grok-4.6、gemini-3.7-flash、gpt-5.6-sol（GOAT 无 blockedModels）
- **Pro / Pro-v1**：standard + premium，但拉黑 claude-fable-5、claude-opus-5/4-8/4-7/4-6/4-5、sakana/fugu-ultra
- **Provider / Max / Ultra / Teams Pro**：全部模型
- 算"有效订阅"的状态：`active`、`trialing`、`past_due`（欠费状态仍算有效）

### 免费模型

模型目录里有 `badge: "free"` 的免费模型（如 MiniMax M3 Free），不耗 premium 额度。
另有促销开关 `isLingFlashFreeEnded`：Ling-flash 限免在 2026-08-03T13:00Z 已截止——官方会搞限时免费活动，值得关注新活动。

## 七、Taste 学习

官方 FAQ 自述：`taste-1` 是 "meta neuro-symbolic AI model with continuous reinforcement learning"。
**已确认的调用链**：

- 后台 learner 分析会话消息；
- 通过 feature model 发起模型调用；
- 最终更新本地 `.commandcode/taste/.../taste.md`。

`/alpha/learn` 路由常量确实存在，但当前 bundle 中没有清晰的直接调用点——"用户接受/拒绝/编辑直接上传 `/alpha/learn`"目前**证据不足，属根据路由名推测**。

开关关系（纠正常见误解）：

- `DO_NOT_TRACK` 只控制 telemetry/fingerprint，**不影响** tasteLearning。
- `CMD_ZDR` 附加 ZDR header、调整可用上游及 feature model，但**不会**统一关闭 taste learning。
- taste 学习有自己的开关（`/taste`、`cmd taste enable/disable`），反映在 `x-taste-learning` 请求头。

## 八、其他发现

1. **`past_due` 仍视为有效订阅**：欠费未付的账号在客户端逻辑里依然享受完整 plan 权限，服务端是否同样宽松值得验证。
2. **BYOK 完全绕过订阅**：自定义 provider 流量直连用户配置的 baseURL。`individual-provider`（$15）档大概率只买 taste 等增值服务，不买模型权限。
3. **遥测**：Axiom + OpenTelemetry 双路埋点，`DO_NOT_TRACK=1` 或 `"telemetry": false` 可关（只影响遥测和指纹，不影响 taste）。
4. **多环境**：staging 走 `staging-api.commandcode.ai`，本地开发走 `localhost:9090+offset`。
5. **`x-cmd-provider-deepseek-internal: 1`**：疑似内部/测试通道 header，服务端行为值得探。

## 九、实测验证（goat 账号，2026-08-25）

以下均为真实 API 调用确认，修正/补充了纯静态分析的推测：

### 认证与计费接口

- `GET /alpha/whoami`：200，返回 `{success, user:{id,name,email,userName}, org}`。
- `GET /alpha/billing/subscriptions`：200，`planId: "individual-goat"`、`status: "active"`、
  Stripe 订阅 ID/price ID、`currentPeriodStart/End`。
- `GET /alpha/billing/credits`：200，真实结构为

  ```json
  {
    "credits": {"belowThreshold": false, "creditThreshold": 0,
                "monthlyCredits": 70, "purchasedCredits": 0, "freeCredits": 0},
    "windowLimits": {"limited": true, "exceeded": null,
      "fiveHour": {"used": 0, "cap": 14, "exceeded": false, "resetAt": 0},
      "weekly":   {"used": 0, "cap": 35, "exceeded": false, "resetAt": 0}}
  }
  ```

  **新发现**：goat 有双窗口限流——5 小时窗口 $14、每周窗口 $35（客户端源码里的
  "You've reached your {window} usage limit" 对应此）。

### /alpha/generate 实测

- **`mode` 字段枚举**（静态分析缺失，靠 400 报错提示挖出）：
  `agent | learning | custom-agent | custom-agent-create | title-gen | tool-desc | compact | vision`。
  主对话用 `"agent"`。
- **错误响应格式**：`{"success":false,"error":{"code":"BAD_REQUEST","status":400,"message":"...","docs":"https://commandcode.ai/docs/reference/errors/..."}}`
- **NDJSON 流确认**，真实事件序列：
  `start` → `start-step` → `reasoning-start` → `reasoning-delta`×N → `text-start` →
  `reasoning-end` → `text-delta`×N → `text-end` → `finish-step` → `finish` → `provider-metadata`
- **重大发现——上游泄露**：`start-step` 事件的 `request.body` 完整暴露了转发给上游的请求，
  确认后端走的是 **Vercel AI Gateway v3**（`/v3/ai/language-model`），
  `providerOptions.gateway: {caching: "auto", sort: "ttft", only: ["deepseek"]}`。
  `finish-step.providerMetadata.gateway` 还带完整路由信息：解析的 provider、
  credentialType（system/BYOK 优先级）、每次尝试耗时、**精确成本**（cost/inferenceCost 等）、
  generationId。代理可以原样透传或剥离这些元数据。
- usage 结构：`{inputTokens, inputTokenDetails:{noCacheTokens,cacheReadTokens},
  outputTokens, outputTokenDetails:{textTokens,reasoningTokens}, totalTokens, cachedInputTokens}`。
- 一条 "say hi"（max_tokens=32）实测花费 $0.0000748，走月度 $70 额度。

### KV 缓存实验（DeepSeek 自动上下文缓存）

方法：同一对话（908 tokens 前缀）间隔 2 秒重发，共两批四次请求（脚本 `scripts/test-cache-hit.mjs`，与本文件同目录）。

| 批次 | promptCacheHit | promptCacheMiss | 成本 |
| --- | --- | --- | --- |
| 第 1 批 round1 | 0 | 908 | $0.00042064 |
| 第 1 批 round2 | 0 | 908 | $0.00042064 |
| 第 2 批 round1 | **896** | 12 | **$0.000038944** |
| 第 2 批 round2 | **896** | 12 | **$0.000038944** |

结论：

- **缓存确实存在且全自动**（DeepSeek 上下文缓存，客户端/代理零感知、零标记）。
- **有预热延迟**：前两次相同请求都未命中，第三、四次（约几十秒后）才命中——缓存写入是异步的，
  不能指望"第二轮必中"。
- 命中率 896/908 ≈ 98.7%（64-token 粒度，尾部 12 tokens 永远 miss）。
- **命中后输入成本降约 10.8 倍**（$0.00042 → $0.000039）。
- 对代理的含义不变：保持前缀稳定（system、tools、消息顺序不乱动），命中交给上游。

## 十、对代理项目的结论

- 由于计费在服务端，代理**无法绕过配额**，能做的是：精确复刻 wire 格式（请求体/头/NDJSON 流解析），把 `/alpha/generate` 包装成 OpenAI 兼容接口，给任意客户端用。
- 关键实现点：
  1. NDJSON 按行解析（**不是 SSE**），识别流内错误标记并转成标准错误响应；
  2. permissionMode 按映射表发 wire 值（`standard`/`auto-accept`/`plan`）；
  3. `threadId` 仅发合法 UUID；`skills`/`memory`/`taste` 为 `null`；
  4. `insufficient credits` 的 400 处理；
  5. 指纹一致性属于可选伪装（非协议必需），要做就按真实算法生成（machineId + 排序 MAC 集合，固定 namespace）。
- 值得探的灰色点：`x-cmd-provider-deepseek-internal` 内部通道、`past_due` 状态的服务端实际行为、免费模型是否真的无限制。
