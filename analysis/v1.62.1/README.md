# Command Code v1.62.1 协议核对

检查日期：2026-09-22。基线为本地 `command-code@1.54.2`，本次 npm `latest` 为 **1.62.1**，发布时间为北京时间 2026-09-22 09:12:11。

结论：新增 **Jev / System One 决策 API** 和 **BYOK Responses 协议支持**；现有 `/alpha/generate` 聊天请求及 NDJSON 消费逻辑未发现破坏性变化。经用户授权，用 `.env` 中第一个上游 key 实测 Jev，返回 **HTTP 200**。已跟进离线版本兜底和聊天模型目录，Jev 代理端点仍待接入。

## 来源与复现

- 固定版本元数据：[command-code@1.62.1](https://registry.npmjs.org/command-code/1.62.1)。下载时从 npm 官方 registry 确认 `dist-tags.latest`。
- 包、元数据：`references/command-code-npm/command-code-1.62.1.tgz`、`command-code-1.62.1.metadata.json`；SHA-512 integrity 和 SHA-1 均通过校验。
- 解包：`references/command-code-npm/extracted-1.62.1/package/`；旧包保留。未运行或安装上游 CLI。
- [provenance.json](./provenance.json)：发布信息、校验值、格式化工具版本。
- [comparison.json](./comparison.json)：函数哈希、已核对的变量改名、模型目录和字面量路由差异。
- [jev-live-results.json](./jev-live-results.json)：单次 Jev 请求的合成输入及脱敏结果，不含凭证和请求头。

在仓库根目录复现静态对比（Prettier 使用与基线相同的 3.6.2）：

```bash
prettier --ignore-path /dev/null \
  references/command-code-npm/extracted-1.62.1/package/dist/cli.mjs \
  > references/command-code-npm/extracted-1.62.1/package/dist/cli.beautified.mjs
python3 analysis/v1.62.1/scripts/compare.py \
  > analysis/v1.62.1/comparison.json
```

下述源码行号均指新版 `cli.beautified.mjs`。对比脚本不执行 CLI；其中变量映射针对这两个版本人工核对，不是通用 JavaScript 语义等价证明。

## 1. Jev：新增独立决策接口，已实测

1.62.1 新增 `typesafe/jev`，以及 `jev`、`jev-latest`、`typesafe-ai/jev` 别名。CLI 只允许 headless 模式，进入独立分支，不走普通聊天循环。

- 方法和路径：`POST /provider/v1/systemone`。
- 鉴权：沿用 Command Code 账号和 `resolveCommandAuthHeaders`，不需要在该分支配置独立 TypeSafe key。
- 请求：顶层 `model`、`state`、`questions`，没有聊天请求的 `messages`、`params` 或 `stream`。
- `questions` 为具名问题对象，CLI 校验 1–20 个问题。支持 `noul`、`choice`、`score`；`choice` 使用具名 criteria，`score` 使用至少两个元素的 criteria 数组。
- 响应：一次性 JSON，包含 `answers`，可带 `model` 和 `usage.input_tokens/output_tokens`，不是 NDJSON/SSE。
- `noul` 返回数值；`choice` 返回选项，可带 confidence/probabilities；`score` 返回分数，可带 confidence/legend/probabilities。

证据：模型与 schema 定义在 76515–76600 行；解析与发送在 51348–51397 行；鉴权 transport 在 112490 行；headless 分支在 112813 行。

实测请求为构造数据：

```json
{
  "model": "typesafe/jev",
  "state": {"value": 2},
  "questions": {
    "greater_than_one": {
      "type": "noul",
      "instructions": "Is the value greater than 1?"
    }
  }
}
```

使用 `.env` 中 `COMMAND_CODE_API_KEY` 的第一个 key，直连官方 HTTPS 接口，未重试、未轮换 key。实测耗时 **4.053 秒**，**HTTP 200 / application/json**，返回：

```json
{
  "model": "typesafe/jev",
  "answers": {"greater_than_one": {"type": "noul", "noul": 0.99}},
  "usage": {"input_tokens": 279, "output_tokens": 22}
}
```

包内 `dist/bundled/command-code-knowledge/reference/headless.md:222` 写明现有登录可用、限时免费，但没有列出 Jev 的最低套餐、各档配额或免费截止时间。本次成功证明该 key 当前可以调用；未查询账单，响应也没有费用字段，不能据此确认实际扣费或所有套餐的长期权益。

可按需手动复现；此命令会发起真实请求，可能消耗上游配额：

```bash
python3 analysis/v1.62.1/scripts/probe-jev.py --live --output /tmp/jev-result.json
```

**对代理的影响**：当前仅将 `/v1/chat/completions` 转为 `/alpha/generate`，无法直接服务此接口。支持 Jev 需要专门的请求/响应处理；只把 `typesafe/jev` 加到聊天模型列表并不成立。

## 2. Responses 与 BYOK 的变化

1.55.0 changelog 宣布 Provider API 和 BYOK 支持 Responses。本地包能直接确认 BYOK 端的实现：

- `providers.json` 接受 `api: "openai-responses"`；SDK 分支调用 `.responses(model)`，向 `{baseURL}/responses` 发送请求。
- `api`、`baseURL` 可在单个模型上覆盖 provider 默认值，同一 provider 可混合 Chat Completions、Responses 和 Anthropic Messages。
- Responses provider options 包含 `store: false` 和 `include: ["reasoning.encrypted_content"]`；具备推理能力时还设置 `forceReasoning` 与 `reasoningSummary`。
- 新增 BYOK 请求默认 `User-Agent: command-code/<version>`，保留用户显式覆盖。
- 对 `opencode.ai` 增加 `x-opencode-session`，值来自 threadId；只在未显式配置同名头时补上。这不是 Command Code 网关新增的必需头。

证据：`asApiKind` 36689 行、`parseModel` 36750 行、`routeFor`/`responsesProviderOptions` 37173–37191 行、`sessionRoutingHeaders` 37219 行、`buildAiSdkTransport` 37280 行、相关常量 74247 行。包内 `reference/byok.md` 也记录了新 wire 和模型级覆盖。

**对代理的影响**：现有聊天链路无需切到 Responses。本仓库当前没有 `/v1/responses` 路由；若希望作为新版 CLI 的 Responses BYOK 后端，需要另行实现。Provider API 的服务端 Responses 行为本次未实测，不能仅由客户端包推定全部字段和流事件兼容。

## 3. 原有聊天协议核对

原先跟踪的 14 个核心函数中：**8 个逐字一致、5 个仅有已验证的打包变量改名、1 个只增加遥测会话参数**。另外复核了 11 个 transport、行读取、错误和工具转换辅助函数；详见 `comparison.json`。

- `/alpha/generate` 路径、JSON 请求、`config/memory/taste/skills/permissionMode/threadId/mode/promptCache/params` 结构保持一致。
- `params.messages/tools/system/max_tokens/stream/temperature/reasoning_effort` 的构造未变，仍支持 system 缓存块与 `cache_control`。
- 鉴权头名字、Bearer token、`User-Agent: cli`、指纹盐与 collector version 均未变。
- NDJSON 的 text/reasoning/tool/finish/error 处理、缓存 token 解析及终止错误标记未变；没有要求迁移到 SSE。
- `createModelClient` 向 `startChatSpan` 多传 `conversationId`；后者按会话选择 trace 上下文，并补充遥测 `session.id` 属性。线上仍使用相同格式的 `traceparent`，请求体没有增加此字段。
- CLI 将权限界面改称 accept edits/yolo，但 `toWirePermissionMode` 仍保持原映射，没有要求代理发送新的权限字符串。

字面量 `/alpha/`、`/provider/` 路由扫描仅新增 `/provider/v1/systemone`。该扫描不覆盖所有动态拼接路径；静态包分析也不能排除服务端独立变更。

## 4. 模型目录更新

完整模型表为 **74 → 82** 项，其中隐藏条目 **3 → 4** 项，故非隐藏项为 **71 → 78**。旧版比较脚本显示的 **70 → 78** 只统计显式写了 `contextWindow` 的条目，不是完整目录；本次补充了完整计数，避免沿用这一口径错误。Jev 在独立决策模型表，不计入上述聊天目录。

| 新增 canonical ID | 上下文 | 输入 |
|---|---:|---|
| `Qwen/Qwen3.8-Omni-Flash` | 1,000,000 | 文本、图片 |
| `z-ai/glm-5.3-flashx` | 1,000,000 | 文本、图片 |
| `meituan/LongCat-2.0` | 1,048,576 | 文本 |
| `xai/grok-4.7` | 500,000 | 文本、图片 |
| `stepfun/Step-5-Preview` | 1,000,000 | 文本、图片 |
| `xiaomi/mimo-v2.6-pro` | 1,048,576 | 文本、图片 |
| `xiaomi/mimo-v2.6-pro-ultraspeed` | 1,048,576 | 文本、图片 |
| `xiaomi/mimo-v2.6-flash` | 1,048,576 | 文本、图片 |

`meituan/LongCat-2.0:free` 仍保留在源表，但名称增加 `(Free)`、设置 `hidden: true`，并进入不可选择集合；1.58.0 changelog 标记其免费版退役。新 ID `meituan/LongCat-2.0` 是付费条目，不应静默把旧免费请求改写过去。

证据：模型表 81801、81924–81985、82088–82123、82450 行；隐藏过滤 84103、85725 行；不可选择集合 63359 行附近。这里描述客户端目录，并未逐一验证新模型访问权限。

## 已跟进与后续

1. 已将离线 `fallbackVersion` 更新到 1.62.1，补充上述 8 个模型的静态目录，并移除退役 LongCat 免费条目。动态模型目录仍优先；显式模型 ID 原样转发，包括旧免费 ID，不会静默切换到付费模型。
2. 保留当前 `/alpha/generate` 请求与流解析实现；此次未发现必须立即修复的聊天协议不兼容。
3. Jev 与 `/v1/responses` 分别作为独立功能扩展评估。Jev 已完成一次真实访问验证；Responses 尚未做真实调用。

其余变化主要是 `/loop` 调度、剪贴板图片、图片上下文压缩、终端权限交互及遥测。Node 要求仍为 `>=22`，package.json 除版本号外无差异，CLI 引导入口 `dist/index.mjs` 逐字一致。

### 代理更新验证

- 静态兜底目录由 40 项变为 47 项：新增 8 项逐一匹配本地 npm 分析中的 ID、名称和上下文长度，仅移除 LongCat 免费条目，其余条目不变。
- Go 1.24.0 下 `go vet ./...`、`go test -race -count=1 ./...` 全部通过。
- 与 CI 同版本的 golangci-lint v2.1.6 通过，结果为 `0 issues`。
