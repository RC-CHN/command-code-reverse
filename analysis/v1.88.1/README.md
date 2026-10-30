# Command Code v1.88.1 协议核对

检查日期：2026-10-29。基线为 `command-code@1.87.3`，npm 最新稳定版为 **1.88.1**，发布时间为 2026-10-27 23:06:35 UTC（北京时间 10 月 28 日）。发布包已下载到 `references/command-code-npm/extracted-1.88.1/package/`，校验后仅做静态分析，没有安装或执行 CLI。

结论：**聊天、明文 CoT、Jev、鉴权和 ZDR 主协议保持兼容**。本次主要变化是客户端多图处理和大小错误恢复、后台任务模型选择，以及模型目录。代理已跟进离线版本和模型目录，并修正聊天上游 413 的错误映射；继续尊重下游，不自动删图、清除思考、摘要历史或替换模型。

## 来源与核对方法

- [npm 固定版本元数据](https://registry.npmjs.org/command-code/1.88.1)和其中的官方 tarball，已核验 SHA-512 integrity 与 SHA-1。
- [provenance.json](./provenance.json)：发布时间、检查时间、来源、包与源码 SHA-256。
- [comparison.json](./comparison.json)：69 个完整函数的存在性、行号和哈希，18 组已核验值相同的常量，Jev schema 和两种模型目录的差异。
- 使用本地 Prettier 3.6.2 格式化并通过其 Babel parser 静态提取函数和对象。下文行号均为新版 `dist/cli.beautified.mjs` 的行号。函数哈希变化可能只是打包变量改名；对核心调用、headers、指纹和相关错误分类，进一步核验了改名所指向的常量值。

发布包、完整函数提取、目录快照在被忽略的 `references/`，一次性脚本在 `/tmp`。静态核对后的代理适配与本地验证见末节；本轮没有调用付费推理 API 或更新集群。

## 1. 聊天、流与思考回传

`createModelClient`（45371 行）仍调用 `POST /alpha/generate`，发送原有的 `config/memory/taste/skills/permissionMode/threadId/mode/promptCache/params`，读取 NDJSON。路由、默认输出预算 64000、pause continuation 次数 5 均不变。

`toWireMessages`（45012 行）、`toWireTools`（44993 行）、`toWireSystem`（45060 行）的完整函数与旧版文本相同。`consumeStream`（45186 行）只更换初始 usage 常量的打包名字，其值相同；`consumedStopReason`、缓存写入统计、流内错误读取和流错误重试判定也未出现协议变化。

网关历史 thinking 仍转成 `{type: "reasoning", text: ...}`。`reasoningProviderOptions`（45480 行）、`readReasoningMetadata`（45646 行）和 `responsesProviderOptions`（44524 行）保持原样；没有发现新加入网关消息的 Gemini thought signature 或 OpenAI encrypted reasoning 字段。BYOK 与网关仍是不同处理路径。

新版新增 `stripThinkingFrom`（29657 行）：**当 CLI 自己裁掉历史图片、改变上下文前缀时**，对于 `bindsThinkingToPrefix` 模型，从首个被改动的消息开始清除旧 thinking，避免回放与原前缀绑定的状态（调用点 30393 行及 `withoutStaleThinking`，36110 行）。这不是网关新增的回传能力，也不是无条件删除客户端思考的规则。代理不做这种历史裁剪，继续转发下游实际提交、且原版网关支持的明文思考。

## 2. 多图上下文与请求大小恢复

这是本次最明显的行为变化，发生在 CLI 的会话管理和工具结果处理层，wire 格式未变。

- `imgCompact`（29255 行）统计 user 消息及 tool result 的图片 base64 字符串长度。正常发送采用 **20 MiB 触发阈值、10 MiB 保留窗口**（87758–87759 行），累计超过阈值时依序裁掉最早图片。裁剪用文字占位，并向用户显示 notice；完整图片仍留在 CLI 会话历史中。
- `compressWithLadder` / `compressImageBlock`（40807 / 40839 行）为工具返回的图片增加压缩处理。压缩后仍超阈值时尝试质量阶梯 `[80, 60, 40, 20]`；最终超出 **5,000,000 个 base64 字符**的单图会换成省略说明。缩放后可附坐标换算提示。
- `isRequestTooLargeError`（29521 行）识别 HTTP 413，以及 `request_too_large` 等请求大小错误文本。`isWorkerResourceError`（29525 行）在无状态码或 5xx 时识别 `worker_exceeded_resources`、`error 1102` 等资源耗尽文本。
- `withContextRecovery`（36175 行）新增大小恢复分支：明确请求过大，或 worker 错误且本地序列化调用参数达到 **8 MiB**时，先把图片压到 **4 MiB base64 字符预算**，必要时再摘要会话并重试。较小请求的 worker 错误则等待 **5 秒、10 秒**各重试一次。恢复成功会记住收紧后的会话图片预算。
- 这些错误从普通模型重试中排除，交给大小恢复流程处理；已有请求截断后的摘要恢复仍保留。

以上是 CLI 本地阈值，**不是本次验证出的上游服务端限额**。其中图片预算按 base64 字符数统计，8 MiB 判断按本地调用参数 JSON 的 UTF-8 字节数统计；都不能直接当成代理入口 `MAX_BODY_BYTES` 的新默认值。

代理当前的 64 MiB 请求入口上限仍有独立意义。依照尊重下游的策略，本轮不照搬图像有损压缩、删除旧图、删除 thinking、自动摘要或重放：这些操作会改变下游交给代理的上下文。

核对时发现 v0.3.3 的 `proxy/internal/server/errors.go` 中 `classifyUpstreamError` 没有上游 413 分支，会落成默认的 **502 `upstream_error`**。本次已保留明确的 413 / 请求过大语义，交由下游决定如何缩减请求；错误码 `upstream_request_too_large` 区分上游限制与本地入口超限。worker 资源耗尽的已识别 5xx 继续保留服务器状态，不新增 key 失效或余额耗尽判定。该问题已通过本地 HTTP 模拟上游复现并修复，未故意向生产上游发送超大请求。

## 3. 模型目录：新增 2 项，富目录隐藏 4 项

旧式表与内嵌 client catalog 均为 **104 → 106 项**，没有移除模型 ID。

| 新增 ID | 上下文 | reasoning efforts | client catalog 标注 |
|---|---:|---|---|
| `gpt-6.1-terra` | 1,050,000 | low / medium / high / xhigh / max | premium、最低 individual-pro、ZDR |
| `moonshotai/Kimi-K3.1-Pro` | 1,048,576 | low / high / max | opensource、最低 individual-go、ZDR |

两项均支持 text/image。上述套餐、ZDR 是发布包目录元数据，不能替代真实账号权限或在线路由验证。

富目录新增 `hidden: true` 的条目有：

- `minimax/minimax-m3.1-flash:free`
- `ornith/ornith-1.5-122b:free`
- `xiaomi/mimo-v2.5`
- `xiaomi/mimo-v2.5-pro`

**旧式对象表只同步了两个 MiMo 标记**，MiniMax / Ornith 免费版仍没有 hidden，因此仅比较旧式表会漏掉这两项。静态展示应参考新版 `/provider/v1/models?view=client` 所用快照。隐藏不等于已证实接口拒绝，不能静默把免费 ID 改成付费 ID，也不应拒绝下游显式指定的旧 ID。

富目录 schemaVersion 仍为 1，refreshAfterSeconds 仍为 900，roles 完全相同。既有条目除展示排序和上述四个 hidden 字段外，没有其他字段变化；包含既有定价、最低套餐、ZDR、行为配置。没有新增或移除字面量 `/alpha/`、`/provider/` 端点。

代理 v0.3.3 的 fallback 包含上述四项，也没有两个新模型。本次已增加新项并从离线推荐列表移除 hidden 项；在线目录继续优先，显式模型 ID 原样转发。离线版本兜底从 1.87.3 更新到 1.88.1；启动刷新、24 小时刷新和显式固定版本逻辑保持原样。

## 4. 后台任务免费模型选择

`resolveFeatureModel`（7415 行）新增通用免费模型选择：当前会话使用免费模型、未开启 ZDR、开关启用时，将符合条件的后台功能默认选到该会话模型。新增 `backgroundTasksUseFreeModel` 配置，未设置时启用；vision 和 tasteLearning 不走此分支。显式 feature override 仍需经过原有 BYOK、模型可用性和 ZDR 判断；这不是无条件覆盖所有用户配置。

这是 CLI 的标题、摘要、工具描述等后台工作调度。代理没有对应的 CLI 后台任务，应继续使用下游请求的 model，不自动改成免费模型，也不更改 ZDR 请求策略。

## 5. Jev、鉴权、ZDR、用量与指纹

- Jev schema（92326–92411 行）消除打包变量改名后与旧版相同：`/provider/v1/systemone`、`typesafe/jev` 别名、`state/questions`、`answers/usage` 保持原样，问题数仍为 **1–100**。Choice 仍无 CLI 选项数上限声明，代理默认 20 / 显式解锁 255 的独立策略无需变化。
- `buildCommandAuthHeaders`、`buildCommandApiHeaders` 只有已验证的常量绑定改名；header 名称、User-Agent `cli`、`x-cmd-zdr: 1` 保持原样。
- `buildMachineFingerprint`（53007 行）的算法、盐 `command-code:device-fingerprint:v1`、collectorVersion 1 不变；`trackCliSessionExistsEvent`（145334 行）完整文本相同。
- `USAGE_EXCEEDED`、续费处理中提示、拒答别名和套餐访问规则的已核对常量保持相同。`projectUsageView`、缓存读写统计函数未变，未发现面板余额/用量格式必须适配的新变化。
- 发布清单中的 AI SDK 依赖版本范围不变。更新器、配置 UI 和 taste 覆盖修复属于 CLI 功能，不要求代理同步实现。

## 已实施范围与验证

1. 离线版本更新到 1.88.1；目录增加两个新模型、去掉四个 hidden 项。逐项核对剩余 58 项的 ID、名称、上下文和可见性与内嵌 client catalog 一致。
2. 按 CLI 已识别的状态与文本签名检测请求过大，并识别结构化 code/type；聊天返回 413 `invalid_request_error` / `upstream_request_too_large`，使用固定诊断文字，不回显原始上游错误体。已开始的 SSE 返回同码错误和一个 `[DONE]`，不发送成功 finish。
3. 大小错误不提示自动重试、不轮换账号或熔断；共享池分类也覆盖 Jev。原有 CoT、Jev 响应格式、ZDR、其他错误的 key 策略及 v0.3.3 空闲超时保持原样。
4. 本地回归覆盖 HTTP 413、结构化大小错误被包在 403/429 中、纯文本/结构化/嵌套 JSON 流错误、流式与非流式、SSE 开始前后、统计失败记账及 key 可用性。负例区分单图提示、上下文超限、worker 资源错误、限流与鉴权错误；请求转发检查保留显式隐藏模型、预算、effort、图片与明文思考。

验证结果：Go 1.24 `go test -race -count=1 ./...`、`go vet ./...` 和 golangci-lint 2.1.6 均通过（0 issues）。

上游依据仍来自静态发布包；新模型实际访问、hidden 条目的在线可用性、服务端请求体阈值均未作真实调用验证。
