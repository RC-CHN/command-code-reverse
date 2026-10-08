# Command Code v1.79.1 协议核对

检查日期：2026-10-08。基线为本地 `command-code@1.62.1`，检查时 npm `latest` 为 **1.79.1**，发布时间为北京时间 2026-10-08 06:09:28。

结论：已核对的 `/alpha/generate` 聊天请求、明文思考回传、Jev 和 ZDR 路径未发现破坏性变化。新增 **`cache-write-tokens` 用量事件**、**ACP/RPC 的可选 `x-cli-surface` 请求头**，模型目录也有更新。代理的离线版本和模型元数据需要跟进，缓存写入统计可单独补齐。

本次为静态分析，未调用真实模型。下文“代理现状”对应 `8552aa2`（v0.2.1）；建议项尚未实现。

## 来源与核对方法

- 固定版本元数据：[command-code@1.79.1](https://registry.npmjs.org/command-code/1.79.1)。发布包来自 npm 官方 registry，SHA-512 integrity 与 SHA-1 均通过校验。
- 解包目录：`references/command-code-npm/extracted-1.79.1/package/`，基线包保留在 `extracted-1.62.1/package/`。未安装或执行 CLI。
- [provenance.json](./provenance.json)：检查时间、发布信息、包校验值及源码哈希。
- [comparison.json](./comparison.json)：完整函数的源码行号与哈希、已核对的常量改名、模型和字面量路由差异。

源码以 Prettier **3.6.2** 格式化，完整函数和模型对象通过 Acorn AST 提取，再人工核对关键路径。哈希不同不等于协议改变；打包变量改名与行为变化分别核对。下述行号均指新版 `dist/cli.beautified.mjs`。下载包、完整 AST 提取结果和临时分析脚本留在被 Git 忽略的 `references/` 或临时目录。

## 1. 聊天请求与思考回传

`createModelClient` 仍向 `POST /alpha/generate` 发送 JSON，读取 NDJSON 响应。请求继续使用 `config`、`memory`、`taste`、`skills`、`permissionMode`、`threadId`、`mode`、`promptCache` 和 `params`；`params` 的模型、消息、工具、system、输出上限、流式开关、temperature 和 reasoning effort 结构未变。

网关路径的思考处理保持原状：

- `consumeStream` 聚合 reasoning 文本，生成 `thinking` 内容时仍写入空 `signature`。
- `toWireMessages` 将历史 thinking 转为 `{type: "reasoning", text: ...}`，未新增签名或加密内容字段。
- BYOK 的 `reasoningProviderOptions` 仍单独处理 Anthropic `signature` / `redactedData` 和 OpenAI `reasoningEncryptedContent`，不能据此推断网关也接受这些字段。

代理 v0.2.1 对下游 assistant `reasoning_content` 和 reasoning 分块的转换仍适用。遵循“保持原版网关策略、尊重下游”：转发下游实际提供的明文思考，不补造缺失内容，不将 BYOK 元数据混入网关，也不新增 Gemini `thoughtSignature` 或加密思考的网关回传。

证据：`toWireMessages` 39425 行、`consumeStream` 39588 行、`createModelClient` 39772 行、`reasoningProviderOptions` 39877 行、`readReasoningMetadata` 40040 行。

## 2. 新增缓存写入用量事件

新增事件形状为：

```json
{"type":"cache-write-tokens","cacheWriteTokens":123}
```

原版规则：

1. `cacheWriteTokens` 必须是有限的非负数，保留最后一个有效事件值。
2. `finish.totalUsage.inputTokenDetails.cacheWriteTokens` 仍是主要来源，缺失时按 0 处理。
3. 读完整个流后，只有汇总的 `cacheWriteTokens === 0` 才用事件值补齐；已有正数不覆盖，也不相加。

代理当前既未保留嵌套的缓存写入计数，也未处理新事件，因此会漏掉写缓存统计；正文与 reasoning 事件的转发不受影响。缓存写入量与 `prompt_tokens_details.cached_tokens` 的缓存读取量含义不同，不能合并。

如补齐统计，应同时覆盖嵌套计数和独立事件。若将来向下游暴露独立用量字段，还需处理事件在 `finish` 后到达的情况：当前 SSE 在收到 `finish` 时就写出 usage，直接增加一个事件分支不足以保证流式、非流式统计一致。

证据：`readCapturedCacheWriteTokens` 39492 行、`consumeStream` 39588 行；代理 `proxy/internal/stream/stream.go` 的 `Usage` / `UsageFromUpstream`，以及 `proxy/internal/server/chat.go` 的 `serveStream`。

## 3. 请求头与客户端身份

`buildCommandAuthHeaders`、`buildCommandApiHeaders` 新增可选 `x-cli-surface`。对应状态默认未赋值，只有 ACP 和 RPC 入口分别设置为 `acp`、`rpc`；普通 CLI 不发送该请求头。因此普通代理请求无需默认添加。

设备指纹的盐 `command-code:device-fingerprint:v1`、`collectorVersion: 1` 未变。`cli_session_exists` 仍携带 `sessionId`、`cliVersion`、`mode`、`os`，mode 依据 TTY 为 interactive 或 non-interactive，未发现新增必填字段。

代理版本头已有启动刷新及每 24 小时刷新机制；离线 `fallbackVersion` 仍为 1.62.1，建议更新到此次核对版本，不改变显式版本固定配置。

证据：默认状态声明 1870 行；`setCliSurface` 30746 行；请求头构造 30752、30956 行；ACP/RPC 调用 86882、129684 行；`buildMachineFingerprint` 46847 行；指纹常量 79252 行；`trackCliSessionExistsEvent` 130051 行。

## 4. Jev、ZDR、错误和账单

Jev 仍使用 `POST /provider/v1/systemone`，模型为 `typesafe/jev`，别名、`state/questions` 请求和 `answers/usage` 响应结构未变。CLI 的限制仍是每次 **1–20 个问题**，Choice schema 没有声明单题选项数上限。

代理的 Choice 默认 20 项、显式开关解锁 255 项，是此前确定的代理策略；与 CLI 的问题数量限制应分别理解。本次没有重新实测 255 项。

ZDR 仍发送 `x-cmd-zdr: 1`。`CMD_ZDR_NO_PROVIDERS`、`USAGE_EXCEEDED`、套餐和余额错误在已核对的分支中没有新增协议变化，继续沿用现有处理。

1.69.0 changelog 提到 Go 套餐改为按模型 credits；本次检查的账单端点和错误处理没有发现必须新增的适配。Cloud 增加 `sandboxMinutes`、`sandboxAccess` 展示字段。代理 `Client.Billing` 与 `handleCredits` 使用 `json.RawMessage` 保留完整账单响应，无需为新增字段建立固定结构，也不应根据客户端套餐表预先猜测账号能否访问某个模型。

证据：Jev 定义 81043–81131 行、`runSystemOne` 54172 行；`parseSpendCapError` 31074 行、`formatRunError` 31322 行、`projectUsageView` 5328 行、`evaluateModelAccess` 5616 行。

## 5. 模型目录变化

完整目录对象为 **83 → 95 项**，新增 12 项，没有删除项。其中新增 10 个非隐藏条目：

| 模型 ID | 上下文 | 原版 provider |
|---|---:|---|
| `claude-haiku-5-5` | 1,000,000 | anthropic |
| `claude-opus-5-5` | 1,000,000 | anthropic |
| `claude-sonnet-5-5` | 1,000,000 | anthropic |
| `gpt-6-luna` | 1,050,000 | openai |
| `gpt-6-sol` | 1,050,000 | openai |
| `gpt-6.1-sol` | 1,050,000 | openai |
| `deepseek/deepseek-v4.1-flash-fast` | 1,000,000 | vercel-ai-gateway |
| `inclusionai/ling-3.1-flash:free` | 262,144 | vercel-ai-gateway |
| `mistral/mistral-large-4` | 524,288 | vercel-ai-gateway |
| `stealth/glyph-cluster:free` | 256,000 | vercel-ai-gateway |

另有 `stealth/space-bunny-alpha`、`stealth/pixel-canary`，但新版已标记 `hidden: true`，changelog 也记录退役，不应加入代理公开兜底目录。目录中的 provider 描述原版配置，不代表当前账号的模型访问权限。

**计数修正：**旧分析只匹配字面量 ID，遗漏了 1.62.1 中通过常量 `ZL` 引用的 `MiniMaxAI/MiniMax-M3-Free`。该条目旧版已经存在且隐藏，因此 1.62.1 的完整数量应为 83，不是 82；新版改为字面量写法也不算新增。两版显式隐藏条目分别为 5、7 项，另各有一个按时间动态隐藏的 Ling 条目。

现有型号还有两类元数据变化：

- `stepfun/Step-3.5-Flash` 上下文从 **1,000,000 修正为 262,144**；代理兜底目前仍为旧值。
- DeepSeek V4 Pro、V4 Flash、V4 Flash Vision、V4.1 Flash 新增 effort `off`，新的 V4.1 Flash Fast 也支持。代理已原样转发 `reasoning_effort`，无需额外映射或替下游默认关闭思考。

证据：模型目录始于 83396 行；新增项和变动项的逐项位置见 [comparison.json](./comparison.json)。

## 6. 建议跟进范围

1. 更新离线版本兜底；按当前目录覆盖范围补充模型并修正 Step 3.5 Flash 上下文，保持动态目录优先和用户显式模型 ID 原样转发。
2. 单独补齐缓存写入统计，遵循原版补齐顺序，明确与缓存读取量的区别及流结束后的 usage 时序。
3. 保持现有 CoT、Jev、ZDR 策略。Cloud、ACP、远程控制、计划/执行分模型和中文界面属于 CLI 功能，本次聊天协议跟进无需扩展这些接口。

此次结论来自发布包静态路径核对；模型实际访问权限、套餐行为和新事件到达顺序尚未进行本版本的真实请求验证。
