# Command Code v1.87.3 协议核对

检查日期：2026-10-23。基线为本地 `command-code@1.79.1`，本次从 npm 官方 registry 下载 **1.87.3**，解包到 `references/command-code-npm/extracted-1.87.3/package/` 后静态分析，未安装或执行 CLI。

结论：聊天请求和明文 CoT 回传结构保持兼容；需要跟进的是拒答结束原因、Jev 问题数上限及模型目录。新版还新增面向 CLI 的富模型目录接口，可作为以后面板能力展示的数据源。

## 来源与方法

- [固定版本元数据](https://registry.npmjs.org/command-code/1.87.3)及其官方 tarball，校验 npm SHA-512 integrity 和 SHA-1。
- [provenance.json](./provenance.json)：来源、检查时间、包与源码校验值。
- [comparison.json](./comparison.json)：完整函数的行号、哈希、模型差异和关键常量。函数由 Prettier 3.6.2 的 Babel parser 静态提取；哈希变化包含打包变量改名，不能直接视为行为变化。

以下行号均对应新版 `dist/cli.beautified.mjs`。下载包、完整函数提取文件和临时脚本保留在被忽略的 `references/` 或 `/tmp`；分析目录不收录它们。

## 1. 聊天与 CoT

`createModelClient`（44888 行）继续请求 `POST /alpha/generate`，发送原有 `config/memory/taste/skills/permissionMode/threadId/mode/promptCache/params`，接收 NDJSON。路由、默认输出预算 64000、暂停续传次数 5 的常量值未变。

`toWireMessages`（44529 行）、`toWireTools`（44510 行）、`toWireSystem`（44577 行）与旧版完整函数文本相同。网关历史 thinking 仍转成 `{type: "reasoning", text: ...}`；BYOK 的 `reasoningProviderOptions`（44997 行）、`readReasoningMetadata`（45163 行）和 `responsesProviderOptions`（44041 行）也没有变化，签名和加密思考仍属于另一条处理路径。

因此保持既定策略：尊重下游实际传入的明文思考，按原版网关格式回传；没有新增 Gemini thought signature 或 OpenAI 加密思考的网关通道。

新版会在发送 CLI 自身的 effort 配置前调用 `isThinkingEffort`。代理继续原样转发下游提供的 `reasoning_effort`，避免把 CLI 配置修复变成代理静默删除调用方参数的行为。

## 2. 正常截断与拒答

`consumeStream`（44703 行）仍把显式 `length` 视为输出预算耗尽，不要求同时出现最终正文。这支持修正代理 v0.3.1 对“只有 reasoning、预算已经耗尽”的误判：流式和非流式均正常结束为 `finish_reason: "length"`，保留实际 reasoning 与 usage，不擅自增加预算或补造回答。若模型不公开思考，显式 length 下的空输出也按正常预算截断处理。

新增 `consumedStopReason`（44656 行）同时检查 `finishReason` 和 `rawFinishReason`。任一字段为 `refusal`、`content_filter` 或 `content-filter`（去首尾空白、不区分大小写）即按拒答结束；这一步优先于普通结束原因。别名集合在 89300 行，判断函数在 44463 行。

代理本轮将这些拒答映射为 OpenAI `content_filter`。正常拒答不要求正文或正数 output tokens。协议校验继续拒绝显式 abort、缺少最终 finish、未知结束原因、未完成工具调用及 finish 后的传输失败。

此外原版 TTFT 改为首个 token 事件触发，避免把流建立事件当作首 token。代理目前记录的是完整请求耗时，本轮不新增 TTFT 指标。

## 3. Jev 上限

`POST /provider/v1/systemone`、`typesafe/jev` 及其别名、`state/questions` 请求和 `answers/usage` 响应保持一致。请求 schema 的问题数常量从旧版 `qM = 20`（81086 行）变为新版 **`YA = 100`**（91592 行）；schema 在 91593 行起引用该常量。

代理本轮相应允许每次 **1–100 个具名问题**，101 个及以上在本地返回 422，不进入 key 池。单题 Choice 的 schema 仍没有选项数上限声明；代理现有默认 20 项、`JEV_UNLOCK_MAX_OPTIONS=true` 解锁 255 项的策略保持独立。

这里只静态确认问题数上限并使用模拟上游验证转发和拦截，没有发出 100 个问题的付费真实请求。

## 4. 模型目录

旧目录对象 **95 → 104 项**，新增 11 项、移除 2 项。另一个原有免费 Ling 条目改为隐藏。新增的 11 项均可见：

| 模型 ID | 上下文 |
|---|---:|
| `minimax/minimax-m3.1-flash` | 1,000,000 |
| `minimax/minimax-m3.1-flash:free` | 1,000,000 |
| `reflection/beam:free` | 1,048,576 |
| `google/gemini-4-argon` | 1,000,000 |
| `meta/muse-wonder-1.0` | 1,048,576 |
| `meta/muse-spark-open` | 1,048,576 |
| `Qwen/Qwen3.8-Max-Prime` | 1,000,000 |
| `nvidia/nemotron-3.5-super-vl` | 262,144 |
| `ornith/ornith-1.5-122b` | 262,144 |
| `ornith/ornith-1.5-122b:free` | 262,144 |
| `inclusionai/ling-3.1-flash` | 262,144 |

退役的 `inclusionai/ling-3.0-flash-sante:free` 和 `inclusionai/ling-3.0-flash-free` 从原版目录移除；`inclusionai/ling-3.1-flash:free` 改为隐藏。付费 `inclusionai/ling-3.1-flash` 是不同 ID，不能把旧免费 ID 静默改写到这里。DeepSeek V4.1 Flash 和 Fast 的展示名新增 `(latest)`。

代理本轮同步新增模型、清理已列出的退役免费条目并更新名称。离线目录仍覆盖现有模型范围，动态上游目录优先，显式模型 ID 原样转发。已逐项比对当前 60 个兜底条目的 ID、名称、上下文与新版内嵌目录，并核对没有隐藏条目。离线 CLI 版本兜底更新为 1.87.3，显式固定和自动刷新继续生效。

新版新增 `/provider/v1/models?view=client`（76681 行），附带 `schemaVersion`、`catalogVersion`、`refreshAfterSeconds`、`roles`，每个模型含别名、输入模态、effort、ZDR、定价、最低套餐和行为配置等元数据。内嵌快照（73388 行起）schemaVersion 为 1，共 104 项，与旧式目录的 ID 集合相同；建议刷新间隔 900 秒。

现有代理仍消费普通 `/provider/v1/models`，本轮不切换 schema 或引入磁盘缓存。富目录可另做能力展示，但目录中的最低套餐和定价不能替代账号真实余额、可用性和上游实际计费结果。

## 5. 请求头、ZDR、用量及其余变化

- `buildCommandAuthHeaders`（35850 行）、`buildCommandApiHeaders`（36054 行）的已核对差异为常量绑定改名；ZDR 仍发送 `x-cmd-zdr: 1`，CLI surface 仍是可选字段。
- 设备指纹盐仍为 `command-code:device-fingerprint:v1`，collectorVersion 仍为 1（89799–89800 行）；`trackCliSessionExistsEvent`（144450 行）携带的会话字段未变。
- 缓存写入统计函数 `readCapturedCacheWriteTokens`、`readCacheWriteTokens1h` 文本相同，继续使用原有总量与事件补齐规则。
- 用量查询仍有 `/alpha/whoami?limits=1`、credits、subscriptions 和 usage summary；端点常量位于 77038–77041 行。未发现要求修改当前面板账单适配的变化。
- `formatRunError`（36453 行）新增续费处理中和模型退役提示。`RENEWAL_PENDING` 是 CLI 对特定消息附加的标签，本轮不把它推断成新的网关错误码或改变 key 熔断策略。
- 新增 `/alpha/artifacts` 是 CLI 发布内容功能，不影响聊天代理。权限模式别名、Mods、TUI、ACP 和 BYOK 包更新不在此次代理兼容改动范围内。

以上协议结论来自发布包静态分析。本轮代码使用模拟上游覆盖流式、非流式、统计记账与边界；新模型访问权限、富目录在线响应和实际 Jev 100 问题服务端行为未逐项实测。
