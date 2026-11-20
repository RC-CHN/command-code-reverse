# Command Code v1.91.1 协议核对

检查日期：2026-11-19。基线为本地保留的 `command-code@1.88.1`，本次 npm 最新稳定版为 **1.91.1**，发布时间为 2026-11-18 20:14:14 UTC（北京时间 11 月 19 日 04:14）。发布包下载到 `references/command-code-npm/extracted-1.91.1/package/`，校验后仅做静态分析，没有安装或执行 CLI。

结论：**没有发现当前代理必须立即适配的聊天、CoT、Jev 或流结束协议变化**。实际变化主要是删除未公开的 DeepSeek 内部路由开关、增加模型和启动公告，以及 CLI 的子代理、goal 和登录行为。静态核对后，已按用户确认更新代理的离线版本与模型目录；没有改动主协议、发布或部署。

## 来源和核对方法

- [npm 固定版本元数据](https://registry.npmjs.org/command-code/1.91.1)及其 [官方发布包](https://registry.npmjs.org/command-code/-/command-code-1.91.1.tgz)，SHA-512 integrity 和 SHA-1 均通过校验。下载时另核对 registry 的 `dist-tags.latest` 与发布时间。
- [provenance.json](./provenance.json)：下载来源、检查时间、发布包和原始/格式化源码 SHA-256。
- [comparison.json](./comparison.json)：函数行号与哈希、常量绑定、Jev schema、模型目录及代理跟进建议。
- 使用本地 Prettier 3.6.2 格式化，并用其 Babel parser 提取完整函数、常量、导入和模型对象。下文行号均指新版 `dist/cli.beautified.mjs`。不执行上游模块或 getter。

此前跟踪的 **69 个函数全部仍存在**：36 个完整文本相同，另 28 个在检查对应常量/导入并归一化打包变量名后 AST 相同。剩余 5 个实质变化为三个鉴权/请求头函数和两个 CLI 功能模型选择函数。另检查了登录验证、goal 验证、公告和流错误辅助函数；这是一组有明确范围的静态比较，不代表整个客户端或服务端行为完全相同。

发布包、完整提取结果和目录快照保存在被 Git 忽略的 `references/`；一次性分析脚本放在 `/tmp`。本轮没有调用真实模型或使用集群凭据。

## 1. 聊天请求、流和思考内容

`createModelClient`（45915 行）仍请求 `POST /alpha/generate`，发送原有 `config/memory/taste/skills/permissionMode/threadId/mode/promptCache/params`，使用 NDJSON 流。默认输出预算仍为 **64000**，pause continuation 上限仍为 **5**；`max_tokens`、`reasoning_effort`、温度和 system 的字段未变。

`toWireTools`（45537 行）、`toWireMessages`（45556 行）和 `toWireSystem`（45604 行）与基线完整文本相同。历史 thinking 仍转为 `{type: "reasoning", text: ...}`。`reasoningProviderOptions`（46024 行）、`readReasoningMetadata`（46190 行）和 BYOK 的 `responsesProviderOptions`（45071 行）也保持原样；没有新增 Gemini thought signature、OpenAI encrypted reasoning 等数据进入网关消息的通道。继续遵循现有策略：转发下游提交且原版网关支持的明文思考，不自行构造签名或把 BYOK 格式套进网关请求。

`consumeStream`（45730 行）仅初始 usage 常量的打包名称改变，常量值相同。流错误提取、拒答结束原因、缓存写入统计和 `readLines` 的逻辑未变。**没有发现改为“收到 finish 就结束读取”的上游改动，也没有针对之前连接超时或响应尾部中断的新恢复协议。**代理 v0.3.6 的尾部校验和详细诊断仍有意义。

新出现的 `reasoningTagAt` / `stripThinkTags`（29304 / 29309 行）容易被误认为新的 CoT 转换：实际只被 `requestVerdict`（29363 行）用于清理 goal 验证器返回的正文，再提取完成判定 JSON。它没有接入聊天历史转换或 NDJSON 转发，不应照搬为代理全局删除 `<think>` 内容的规则。

## 2. 请求头删除内部 DeepSeek 开关

`buildCommandAuthHeaders`（36491 行）、`buildCommandApiHeaders`（36694 行）和 `resolveCommandAuthHeaders`（63747 行）删除了 `deepseekInternal` 配置及其来源 `CMD_PROVIDER_DEEPSEEK_INTERNAL`，不再构造 `x-cmd-provider-deepseek-internal: 1`。header 名称常量表也移除了该项。

其余已检查请求头保持原样，包括 Bearer、User-Agent `cli`、CLI 版本/环境、project/session、taste、可选 surface、OAuth 和 `x-cmd-zdr: 1`。**现有 Go 代理本来就没有发送内部 DeepSeek header，因此这里无需修复，也不应补回这个开关。**新目录中的 V4.1 Flash Ultrafast 是独立模型 ID，不能据此重新启用旧 header 或改写下游指定的普通 Flash。

## 3. 模型目录：新增 14 项，其中 12 项可见

内嵌 client catalog 从 **106 → 120 项**，没有删除已有 ID；schemaVersion 仍为 **1**，refreshAfterSeconds 仍为 **900**。推荐展示继续以富目录为依据，而非仅看旧式模型对象表。

新增可见条目如下；上下文和 ZDR 均为发布包目录元数据，不是本次账号权限或实际路由测试结果。

| 模型 ID | 上下文 | 目录 ZDR |
|---|---:|---|
| `claude-fable-5-2` | 1,000,000 | 否 |
| `gpt-6.2-astra` | 1,050,000 | 是 |
| `gpt-6.2-sol` | 1,050,000 | 是 |
| `deepseek/deepseek-v4.1-flash-ultrafast` | 1,048,576 | 是 |
| `Qwen/Qwen4-Max` | 1,000,000 | 是 |
| `tencent/hy4` | 1,024,000 | 是 |
| `tencent/hy4-flash` | 1,024,000 | 是 |
| `google/gemini-4-carbon` | 1,000,000 | 是 |
| `thinkingmachines/inkling-2-small:free` | 1,048,576 | 否 |
| `recursive/flint-v1.0:free` | 262,144 | 否 |
| `inclusionai/ling-3.1-flash-1019` | 1,000,000 | 是 |
| `meta/muse-ember` | 1,048,576 | 是 |

另两项 `stealth/crouton-alpha` 和 `inclusionai/ling-3.1-flash-1019:free` 在内嵌富目录中均为 hidden。Ling 免费版的旧式对象使用日期 getter，开放窗口为 2026-10-29 16:00 UTC 至 11-03 16:00 UTC，核对时已过期（95626–95633、97105 行）。不把这两项新增到离线推荐目录；仍不阻止下游显式使用模型 ID。

已有条目的相关变化：

- `poolside/laguna-s-2.1-free`、`reflection/beam:free` 改为 hidden，名称附加 `(Free)`。发布说明称其免费版本退役，但静态目录本身不能证明服务端一定拒绝所有显式请求。
- `tencent/hy4-preview`、`z-ai/glm-5.3-flashx` 的目录 ZDR 从 false 改为 true。
- Glyph Cluster 的展示 notice 从每日 100 请求改为 1000；这是公告式元数据，不应硬编码成代理限流或余额承诺。
- 两个旧 Fable 的描述及多项展示排序调整。既有条目的定价和上下文未见其他变化。

目录 roles 将默认模型从 `deepseek/deepseek-v4-flash` 改为 `deepseek/deepseek-v4.1-flash`，标题生成和工具描述模型也同步到 V4.1 Flash。代理要求下游显式提供 model，不能照搬 CLI 默认选择去替换请求。

逐项比较更新前的代理 58 项兜底目录：**唯一已列出但新版 hidden 的项是 `reflection/beam:free`**；Laguna 免费版原本就不在代理兜底中。其余 57 项的 ID、名称和上下文与新版富目录相符。本轮已去掉 Beam 并加入上表 12 项，目录共 69 项，保持动态目录优先和显式模型 ID 原样转发。

## 4. Jev、ZDR、用量、指纹和错误

- Jev 定义（93682–93770 行）在核对 Zod 导入和变量绑定后结构一致：仍为 `POST /provider/v1/systemone`、`typesafe/jev` 别名、`state/questions` 请求和 `answers/usage` 响应。每次 **1–100 个问题**，Choice schema 仍未声明选项数上限；代理默认 20、显式解锁 255 的独立策略无需调整。
- ZDR 的请求 header 和 `CMD_ZDR_NO_PROVIDERS` 处理未见新增要求。部分模型目录 ZDR 更新不等于需要改变代理的显式配置策略。
- `projectUsageView`（6849 行）仅官网地址常量改名且值相同。credits、subscriptions、usage summary 和 whoami 的已检查路径、窗口/余额字段未见本次必须适配的变化。
- `buildMachineFingerprint`（53624 行）盐仍为 `command-code:device-fingerprint:v1`、collectorVersion 仍为 1；心搏函数（150074 行）仅 `os` 导入别名改变。没有发现设备或会话上报格式变化。
- 流内终止错误标记、`USAGE_EXCEEDED`、请求体过大/worker 资源错误识别、重试分类和图片恢复阈值保持一致。新版没有提供可直接解决集群偶发超时的改动。

登录流程新增已保存 key 的验证：`checkExistingCommandAuth`（9457 行）调用 whoami 验证器，明确无效或该接口返回 403 时重新登录；网络及其他服务错误只标记为未验证。这是 **whoami/登录流程的上下文判断**，不能扩展成聊天接口所有 403 都熔断 key，否则会误伤套餐、ZDR 和用量策略错误。

## 5. CLI 功能变化及范围

- 新增 `GET /provider/v1/notices`（114388 行），启动时获取公告，支持 ETag/304、本地缓存及展示去重。它是 template literal 中的路由，仅比较普通字符串会漏掉；本轮两种形式都已检查。没有新增生成或账单路由，代理无须模仿公告读取。
- 子代理支持后台运行、完成后唤醒主代理、默认模型配置和 200 回合默认上限。`defaultFeatureModel` / `resolveFeatureModel`（7446 / 7458 行）新增 subagents 分支及 BYOK/ZDR 选择约束，属于 CLI 调度，不改变代理的 key 选择策略。
- `/goal` 默认跨轮续跑，最多 5 次自动 continuation，连续 3 轮无进展暂停；验证失败不再误报完成。它不是 `/alpha/generate` 的 pause continuation，两者不能混为一个上游重试参数。
- 远程控制、MCP、Markdown 输出处理、主题和 GTA 启动动画等属于 CLI 体验。发布清单的依赖范围比较中仅 `glob` 从 `^13.0.5` 改为 `^14.0.3`；所列 AI SDK 和 Zod 依赖范围未变，不据此推断用户实际安装的依赖解析版本必然相同。

## 已实施范围和验证边界

已将离线版本兜底 `1.88.1` 更新到 `1.91.1`，增加 12 个可见模型、移除 Beam 免费版的兜底展示，并更新现有目录/版本测试。保持启动刷新、24 小时刷新和显式 version pin；聊天、CoT、Jev、超时、错误码和 key 策略沿用现有实现。

本次完成发布包完整性验证、完整函数/AST 比较、常量与 schema 核对。更新后的 69 个兜底模型已逐项核对 ID、名称、上下文长度和可见性，均与新版内嵌 client catalog 相符。

本地验证：Go 1.24 `go test -race -count=1 ./...`、`go vet ./...` 和 golangci-lint 2.1.6 均通过（0 issues）。

没有验证新模型在线权限、实际免费额度、实际 ZDR 路由或重现历史网络异常；这些仍需相应账号下的独立请求验证。
