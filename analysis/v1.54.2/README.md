# Command Code v1.54.2 上游核对

检查日期：2026-09-17。基线为本地保留的 `command-code@1.51.3`。

结论：当前代理未发现必须立即修改的主协议。已将离线 CLI 版本兜底更新到 `1.54.2`，并补充两个新增模型的静态目录。本次完成发布包下载、校验、静态分析和本地兼容验证，未执行上游 CLI 或发起真实推理请求。

## 来源与复现

- [npm latest 元数据](https://registry.npmjs.org/command-code/latest) 本次返回 `1.54.2`；该地址会变化，固定版本为 [command-code@1.54.2](https://registry.npmjs.org/command-code/1.54.2)。
- 发布包：`references/command-code-npm/command-code-1.54.2.tgz`，已与 npm 元数据核对 SHA-512 integrity 和 SHA-1 shasum。
- 元数据：`references/command-code-npm/command-code-1.54.2.metadata.json`。
- 解包：`references/command-code-npm/extracted-1.54.2/package/`，旧版本原样保留。
- [provenance.json](./provenance.json) 保存校验值与工具版本；[comparison.json](./comparison.json) 保存函数哈希、模型目录及字面量路由对比。

```bash
prettier --ignore-path /dev/null \
  references/command-code-npm/extracted-1.54.2/package/dist/cli.mjs \
  > references/command-code-npm/extracted-1.54.2/package/dist/cli.beautified.mjs

python3 analysis/v1.51.3/scripts/compare.py \
  references/command-code-npm/extracted-1.51.3/package/dist/cli.beautified.mjs \
  references/command-code-npm/extracted-1.54.2/package/dist/cli.beautified.mjs
```

本次格式化使用 Prettier 3.6.2，与基线一致。以下行号均指新版 `cli.beautified.mjs`。这些结论描述发布包中的客户端实现，不代表已验证服务端行为、账户套餐或模型可用性。

## 已跟进

### 离线版本兜底

`proxy/internal/version/version.go` 的 `fallbackVersion` 已由 `1.51.3` 更新为 `1.54.2`。现有 npm 自动刷新和显式版本 pin 逻辑保持不变：正常联网时启动即刷新，之后每 24 小时刷新；这里主要影响无法读取 registry 且未配置版本的启动场景。

### 静态模型目录

CLI 静态表由 68 条增至 70 条，原有共同条目的 ID、名称、上下文长度未变，没有删除条目。

| 新增 canonical ID | 名称 | 上下文长度 | CLI 元数据 |
|---|---|---:|---|
| `deepseek/deepseek-v4.1-flash` | DeepSeek V4.1 Flash | 1,000,000 | 文本及图片；推理档位 `low/high/max` |
| `inclusionai/ling-3.0-flash-sante:free` | Ling 3.0 Flash Sante | 262,144 | 文本；最大输出 32,768 |

证据：模型表第 79355、79893 行。Ling 条目的提示还写明共享容量及每账户每日 100 次请求，这是 CLI 展示信息，代理不应据此自行实现服务端配额。

`proxy/internal/server/models.go` 的静态兜底已加入这两项。代理不以静态表限制聊天模型 ID，`convert.ToWire` 会转发请求中的模型。运行时 `/provider/v1/models` 成功结果仍优先于静态兜底，是否可用由上游账户和服务决定。

这个正式的 `deepseek/deepseek-v4.1-flash` 是 1.53.0 引入的条目，不是 1.51.2 已撤下的 Beta ID。

## 不需要直接移植的变化

- **带图片的压缩边界修复（1.54.2）**：`splitForSummary` 在为保留图片移动切分点后，再次回退到完整用户轮次，避免孤立工具结果；`withContextRecovery` 在压缩后重新调用 `prepareForSend`。证据为第 23438、23443、28183 行。代理不执行这种会话压缩，因此不需要引入该切分逻辑。这也不同于本仓库 PR #3 修复的请求大小和 NDJSON 行大小限制。
- **压缩模型默认值（1.53.1）**：CLI 将 compaction 默认模型切换至 DeepSeek V4.1 Flash（第 61771 行）。代理不自行调用压缩模型。
- **BYOK 推理参数（1.53.1）**：`thinkingHook` 将 SDK provider options 的 `reasoning_effort` 改为 `reasoningEffort`（第 36345 行附近）。Command Code 网关的 `createModelClient` 仍发送 `params.reasoning_effort`，Go wire 字段应保持原样。
- **herdr（1.54.0）**：通过 `HERDR_ENV`、socket 路径和 pane ID 控制本地进程状态上报，走本地 socket（第 85184、85229 行附近），未改变网关鉴权或推理请求格式。
- **print 模式落盘和界面修复（1.54.1）**：退出前刷新会话写入队列，以及窄终端模型选择器调整；属于 CLI 自身会话与界面行为。
- **DeepSeek 路由表**：CLI 的 provider/gateway 元数据把旧 V4 Flash、Vision 名称的部分路由指向 V4.1，同时保留 ZDR 分支（第 80075、80545 行附近）。这些表不同于发送到 `/alpha/generate` 的 canonical model ID；`createModelClient` 仍原样发送所选模型。不能仅据此在代理中强制重写旧 ID，实际路由和计价需要另行实测。

## 协议核对结果与边界

以下 14 个关键函数经同版本格式化后逐字一致，SHA-256 及两版位置见 `comparison.json`：

- 鉴权与请求头：`buildCommandAuthHeaders`、`buildCommandApiHeaders`。
- 指纹：`buildMachineFingerprint`。
- 请求构造：`createModelClient`、`createSystemPromptBuilder`、`toWireSystem`、`toWireMessages`、`toWireTools`、`toWirePermissionMode`、`toWireThreadId`。
- 流与错误：`consumeStream`、`readStreamErrorEvent`、`readCacheWriteTokens1h`、`parseSpendCapError`。

`POST /alpha/generate`、JSON 请求、NDJSON 响应、system 缓存块和 `promptCache` 均保留，字面量 `/alpha/`、`/provider/` 路由扫描未发现增删。该扫描不覆盖动态拼接路由，函数一致也不能证明服务端没有变化。

包的运行时依赖、Node 引擎要求和 CLI 入口映射未变；package.json 中观察到的依赖变化是开发测试工具 Vitest 4.1.6 → 4.1.11。新的模型实际可用性、套餐限制与路由尚未做真实上游验证。

## 本地验证

- 新版离线版本与两个模型的 ID、名称、上下文长度已逐项对照本次下载的 npm 发布包记录。
- Go 1.24.0 下运行 `go test -race -count=1 ./internal/version ./internal/server` 通过，覆盖离线兜底、版本 pin/刷新、静态模型目录、动态目录与缓存等既有行为。
- 与 CI 相同版本的 golangci-lint v2.1.6 检查通过，结果为 `0 issues`。
- 已同步更新版本分析、README 和 changelog。
