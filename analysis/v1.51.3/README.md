# Command Code v1.51.3 协议差异分析

检查日期：2026-09-09。比较基线为本地保留的 `command-code@1.40.1`；完整历史协议说明见 [v1.32.2](../v1.32.2/README.md)。

本报告最初依据 **npm 官方发布包的静态源码** 与本地 mock 回归测试。随后经用户授权，使用 `.env` 中两个 key 完成真实上游验证，见 [实测结果](./LIVE.md) 和 [脱敏数据](./live-results.json)。未执行上游 CLI，也未修改 `.env`。以下源码分析与实测结论分别标明。

## 来源与可复现性

- [npm latest 元数据](https://registry.npmjs.org/command-code/latest)：本次返回 `1.51.3`。该地址会变化，固定版本及包校验值见 [provenance.json](./provenance.json)。
- [固定版本发布包](https://registry.npmjs.org/command-code/-/command-code-1.51.3.tgz)：下载到 `references/command-code-npm/command-code-1.51.3.tgz`，已验证 npm `dist.integrity` 的 SHA-512。
- 解包目录：`references/command-code-npm/extracted-1.51.3/package/`。保留旧版本，没有覆盖基线。
- [官方 GitHub 仓库](https://github.com/CommandCodeAI/command-code)：HEAD 仍为 `5c8f1b4`，相对本地没有更新。
- [旧 Node 代理参考仓库](https://github.com/MAXeaglet/commandcode-proxy/commit/487f219f9586b2a4ba7f7435eed7ee19dabc53ec)：从 `bb5120e` 快进到 `487f219`。其实现用于交叉参考，不能代替官方协议证据。

使用 Prettier 3.6.2 格式化源码后生成比较记录：

```bash
prettier --ignore-path /dev/null \
  references/command-code-npm/extracted-1.51.3/package/dist/cli.mjs \
  > references/command-code-npm/extracted-1.51.3/package/dist/cli.beautified.mjs

python3 analysis/v1.51.3/scripts/compare.py \
  references/command-code-npm/extracted-1.40.1/dist/cli.beautified.mjs \
  references/command-code-npm/extracted-1.51.3/package/dist/cli.beautified.mjs
```

结果已保存为 [comparison.json](./comparison.json)，包含函数位置、哈希、模型增删和字面量路由差异。脚本只读文本，不执行 bundle。哈希不同可能仅是压缩变量改名，仍须人工核对；路由扫描不覆盖动态拼接路径。

## 主要变化

| 项目 | 1.40.1 | 1.51.3 | 代理处理 |
|---|---|---|---|
| `params.system` | 字符串 | 字符串或 text block 数组 | 显式缓存标记时保留数组，否则沿用字符串 |
| 顶层 `promptCache` | 主生成请求未发送 | 可选，辅助请求可发 `"off"` | 透传策略提示；实测不保证关闭 DeepSeek 模型侧缓存 |
| 组织消费上限 | 无专用展示分类 | 按 `USAGE_EXCEEDED` 分类为终止错误 | 返回 403 `spend_limit_error`，保留 code/message，不熔断、不切换账户 |
| Anthropic 缓存写入 | 通用 cache write token 统计 | 额外读取一小时缓存写入 token | 记录字段位置；不向 OpenAI usage 发明对应标准字段 |
| CLI 静态模型表 | 扫描到 62 条 | 扫描到 68 条 | 兜底表加入新增的 6 条 |

### System 分段与缓存

证据：新版 `createSystemPromptBuilder`（35471 行）、`toWireSystem`（36755 行）、`createModelClient`（37044 行）。行号均指本地美化后的新版 `cli.beautified.mjs`。

系统提示构建器将基础提示、项目上下文分别标记 `cache: true`，IDE 上下文作为后续段。`toWireSystem` 输出 `type: "text"`、`text`，仅在对应段有 cache 标记时添加 `cache_control: {type: "ephemeral"}`；为保留拼接文本，CLI 在非末尾 section 的文本后添加换行。

请求仍发送到 `POST /alpha/generate`：

```json
{
  "promptCache": "off",
  "params": {
    "system": [
      {"type":"text","text":"stable instructions\n","cache_control":{"type":"ephemeral"}},
      {"type":"text","text":"dynamic context"}
    ]
  }
}
```

这是字段片段，完整请求仍须包含 config、messages、model 等。`promptCache` 默认省略；`"off"` 可见于 compact 等辅助调用，不代表正常聊天默认关闭缓存。缓存关闭时也可以携带分段 system，其实际缓存处理由服务端决定。

代理仅在下游 system/developer 的数组文本块显式包含 `cache_control` 时生成数组，并保留原有文本拼接语义。没有缓存标记的数组仍展平成字符串。没有自行添加缓存边界、没有将 `prompt_cache_key` 猜测映射为 `promptCache`，也没有为普通请求自动关闭缓存。

旧 Node 代理在 9 月 2 日记录“system 必须为字符串”，反映的是更早的实现。新版官方 CLI 已实际构建数组；仅凭旧代理说明不能否定这一变化。新版 npm changelog 的 1.50.0 条目写明 Anthropic caching improvements，但本次只对比两个端点版本，没有单独下载 1.50.0 证明每个字段的首次引入版本。

### 组织消费上限

证据：`parseSpendCapError`（28508 行）、`classifySubmitError`、常量 `USAGE_EXCEEDED`（70820 行）。客户端从顶层或 error 对象读取 code，保留上游 message，将其归类为 `spend-limit`，用户界面显示消费上限已达到。`whoami.orgLimits` 用于展示组织级和模型级限额，含 `scope`、`model`、`modelLabel`、`limit`、`spent`、`exceeded`、`resetInterval`、`resetAt` 等字段。

源码没有在该分类器中限定 HTTP 状态，因此 **代理的 403 是明确的本地映射策略，不是本次测得的上游固定状态**。同一凭据对其他模型可能仍然有效，不能将限额误判为凭据失效。代理识别 code 后保留 key，并终止当前请求，不跨账户重试消费上限错误。

现有 `/v1/credits` 仍只组合 billing credits/subscriptions；没有声称它包含 whoami 的 orgLimits，也未增加新端点。

### 流事件与既有兼容缺口

`readStreamErrorEvent` 在两版中逐字相同：`error` 可以是字符串，也可以是含 `message/statusCode/isRetryable` 的对象。旧 Go 解码器只接受对象，会跳过字符串 error；即便对象被解析，除三种计费标记外也会被 handler 忽略。此次修复同时覆盖流式和非流式请求：

- 尚未输出时返回 HTTP JSON 错误；SSE 已开始时发 error 对象和 `[DONE]`。
- 显式错误会立即终止处理，后续 finish 不会把失败覆盖成成功。
- 支持错误消息中嵌入的 JSON error envelope，保留组织消费上限 code。
- 既有 premium/model-plan/insufficient-credits 标记映射仍生效。

嵌套缓存读取字段 `totalUsage.inputTokenDetails.cacheReadTokens` 在 1.40.1 已存在，属于补齐已有协议支持，并非 1.51.3 才新增。代理兼容旧 `cachedInputTokens`；嵌套字段存在时优先，包括显式零值。也兼容嵌套 `outputTokenDetails.reasoningTokens` 与旧 `reasoningTokens`，不重复累加 flat/nested 计数。

真正新增的 `readCacheWriteTokens1h`（36763 行）读取：

```text
providerMetadata.anthropic.usage.cache_creation.ephemeral_1h_input_tokens
```

该字段在 `provider-metadata` 事件中读取，并随 CLI 内部 usage 累加。当前代理仍剥离 provider-metadata，只保留已有成本处理，没有把缓存写入计为缓存命中。

## 模型目录差异

| 新增 canonical ID | 名称 | 上下文长度 |
|---|---|---:|
| `Qwen/Qwen3.8-Max-0902` | Qwen 3.8 Max 0902 | 1,000,000 |
| `meituan/LongCat-2.0:free` | LongCat 2.0 | 1,048,576 |
| `google/gemini-3.8-flash` | Gemini 3.8 Flash | 1,000,000 |
| `meta/muse-spark-1.3` | Muse Spark 1.3 | 1,048,576 |
| `meta/muse-spark-1.3-contributor` | Muse Spark 1.3 Contributor | 1,048,576 |
| `gpt-6-astra` | GPT-6 Astra | 1,050,000 |

这里是发布包中的 Command Code 模型元数据，不是对模型厂商当前产品规格的独立核实，也不保证账户套餐可以调用。上下文、名称按该版本本地表记录；运行时 `/provider/v1/models` 的成功结果仍优先于静态兜底。

两版扫描到的共同条目没有 ID/名称/上下文长度变化，完整对象中的能力、路由、定价不在脚本比较范围。人工确认 Muse Spark 1.3 支持 `low/medium/high/xhigh/max`，Contributor 支持到 `xhigh`；Grok 4.6 的视觉能力在 changelog 中修正。1.51.0 加入的 DeepSeek V4.1 Flash Beta 于 1.51.2 移除，不能按中间版本 changelog 将它加回模型目录。

## 保持不变的核心协议

- `POST /alpha/generate` + JSON 请求 + NDJSON 响应不变，未发现新增字面量 API 路由。
- `toWireMessages`、`toWireTools`、`toWirePermissionMode` 在两版中逐字一致。threadId 仍按 UUID 校验。
- Bearer 鉴权、CLI 版本、环境、session、project slug、taste、traceparent 的构造逻辑未见语义变化；可选 ZDR/OAuth/provider 头仍可见。
- `buildMachineFingerprint` 仅有导入别名/常量变量改名；namespace 仍是 `command-code:device-fingerprint:v1`，thumbmark 派生规则未变。
- 三个计费/套餐终止标记未变；配额仍由服务端执行。

## 本次落地与验证

- 离线 CLI 版本兜底更新为 1.51.3，版本 pin 和 24h npm 刷新逻辑保留。
- 加入上述 6 个兜底模型，修复 README 中已过时的 5xx 熔断说明。
- 加入 system 缓存分段和 `prompt_cache: "off"` 扩展；随后按用户要求为缺省/空 system 自动补单个空格，已做真实上游复测（见 LIVE.md）。
- 修复字符串/对象流错误、嵌套 token 统计及 `USAGE_EXCEEDED` 分类。
- 测试覆盖缓存请求 JSON、字符串兼容、字段校验、错误发生在首帧前/后、后续 finish、flat/nested usage 优先级、消费上限不切换/不熔断 key。
- 验证命令：在 `proxy/` 执行 `go vet ./...` 与 `go test -race -count=1 ./...`。

后续实测已确认 system 数组被接受、重复前缀可命中缓存，以及两账号的基本请求可用；同时发现并修复 tool_choice none 和 project slug/workingDir 不一致，详见 [LIVE.md](./LIVE.md)。组织消费上限没有主动触发，仍是源码及 mock 验证，不能由普通调用成功推断。
