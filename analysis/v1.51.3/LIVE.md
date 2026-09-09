# 1.51.3 真实上游兼容与请求一致性验证

日期：2026-09-09。经用户明确授权，读取项目 `.env` 的两个上游 key。
没有修改 `.env`；缺少的下游代理 key 在测试进程中临时生成。
实际运行 Go 代理，用仅监听 127.0.0.1 的中继记录应用层请求，再转发至真实 API。
未执行官方 CLI，未比较 TLS/HTTP 实现指纹，因此“通过”表示真实 API 接受及应用层字段一致，不表示无法区分代理与官方 CLI。

原始实验摘要保存在 [live-results.json](./live-results.json)。仅保存合成测试内容、状态、token/cost、身份相等比较及部分身份哈希；不保存 Authorization、原始 key、账户资料或完整上游内部事件。

## 结果

| 情况 | 真实结果 |
|---|---|
| 两个 key 的 whoami | 均为 200 |
| 两个 key 的模型目录 | 均为 200、67 条 |
| 普通/重复/SSE 请求 | 两个 key 都可用；同一请求的上游 body 一致 |
| 原会话继续追加消息 | body 随消息增加，thread/session/project 保持稳定 |
| 不同首条用户消息 | thread/session/project 改变，符合现有派生策略 |
| system 与 developer | 均接受 |
| system 缓存分段数组 | 200；重复命中 1408 / 1417 输入 token，约 99.36% |
| 同样内容的字符串 system | 同样可命中缓存，不能把命中归因于显式 cache_control 本身 |
| `prompt_cache: "off"` | 200，但仍有 1408 缓存 token；不是“保证关闭模型侧缓存” |
| 普通及流式工具调用 | 200，返回 echo 工具调用 |
| tool result 省略 name | 修复 none 映射后 200，返回 OK |
| 内嵌 PNG data URL | 200，视觉模型对合成红色方块回答 `Red` |
| 外部图片 URL | 本地 400，零上游请求 |
| 无下游鉴权 | 本地 401，零上游请求 |
| 无效模型 ID | 上游拒绝，代理映射为 404 |
| GPT-6 Astra | 实测账户返回 403 `plan_error`，消息说明套餐不支持 |
| Qwen 3.8 Max 0902 | 200 |
| passthrough 模式 | 普通及重复请求均为 200，身份稳定 |
| 原 `.env` 配置下重启 | 无 seed，thread/session/project 都变化 |
| 固定测试 seed 后重启 | thread/session/project 和请求 body 均保持一致；trace 更新 |

不同请求的 traceparent 均符合格式且发生变化。CLI version、环境、User-Agent、上游鉴权 key 均符合构造规则，下游代理 key 没有转发到上游。

“同一会话”在本项目中指相同的 leading system/developer 内容和第一条非 system 消息。两个独立客户端若使用完全相同的开场，会得到相同的 root，不能将“不同真实用户会话”与“不同 root”混为一谈。相同请求不保证模型输出逐字一致，测试中出现 OK/OK.、reasoning 数量变化。

## 实测发现并修复

### 1. project slug 与工作目录不一致

修复前：`x-project-slug` 从模拟 Windows 路径生成，但 body 的工作目录为 `/tmp/proxy`，environment 为 linux。虽然 API 接受，这些字段不能描述同一个项目。

修复后：同一个 account+conversation 派生的 Linux 路径同时用于 `config.workingDir` 和 slug。每次尝试复制请求后修改 config，不污染调用者共享的数据。真实复测两个 key 均满足：

```text
x-project-slug == ProjectSlug(config.workingDir)
config.environment == linux
```

key 切换时保持 thread/trace、更新 account-scoped session/project 的行为，以及共享请求不被修改，另有本地回归测试；没有人为耗尽真实账号额度来诱发切换。

### 2. `tool_choice: none` 的旧映射被上游拒绝

修复前发送 `{type:"none"}`，真实服务端校验只接受 auto/any/tool，因此工具结果续聊被返回为 502，上游提示 `params.tool_choice.type` 不合法。失败原因并非 tool result 缺 name。

修复后不发送该 tool_choice 对象，发送 `tools: []`，表示不再提供可调用工具。保留历史消息及 tool-result name 的推导。真实复测完成工具调用 → 结果回传 → 文本回答，全部 200；普通请求也显式发送空 tools 数组，与 CLI 的请求构造一致。

## 需要正确理解的差异

- **无 system 的默认行为**：实测输入 7641 token，其中 7552 为缓存；发送空白字符串 system 的对照为 87 输入 token。说明省略 system 会进入上游 CLI 默认提示路径。此后按用户要求，代理已对缺省/空 system 自动补单个空格；原始实验保留为改动前对照。非空提示和缓存分段仍由调用方决定。
- **缓存策略提示**：`promptCache: "off"` 不保证 provider cacheReadTokens 为零。仅凭本次数据不能断定所有上游模型的具体缓存实现。
- **测试输出上限**：最初使用 96/128 输出 token 控制费用。个别样例全部用于 reasoning，HTTP 200 且 finish reason 为 length，尚未产生最终正文。第二个 key 的相关样例提高至 256 后返回 OK；图片提高至 512 后以 246 token 返回 Red。最终缓存复测仍有一条 96-token 截断，缓存统计本身正常。报告将这些记录标为 `test_output_budget_exhausted`，没有算成完整回答成功。
- **重启身份**：配置固定 `FINGERPRINT_SEED` 才能跨重启保持派生身份。测试只在子进程使用合成固定 seed，未写入用户配置；`FINGERPRINT_ENABLED` 始终为 false，没有额外上传设备指纹。
- **未覆盖**：组织消费上限、真实额度耗尽、真实大规模限流、长期会话及负载、所有模型/套餐组合。相关错误分支以源码和 mock/race 测试为证据。

## 后续：空 system 自动占位验证

按用户要求，缺省或空 system/developer 现在自动发送单个空格，避免上游默认 CLI 提示。
使用修改后的 Go 代理再发 4 个真实请求，全部 200，并在本地捕获中确认占位值为 `" "`。

| 下游输入 | 实际上游 system | 输入 token |
|---|---|---:|
| 明确的普通 system | 原文保留 | 15 |
| 没有 system | 单个空格 | 87 |
| 显式单个空格 | 单个空格 | 87 |
| 空字符串 system | 单个空格 | 8 |

三种空值路径均未出现此前约 7.6k token 的默认提示开销。不同请求即便 system 相同，
上游 token 统计仍可能不同；这里核实的是占位字段及默认提示路径，没有承诺 token 数恒定。
脱敏记录见 [system-placeholder-results.json](./system-placeholder-results.json)。
缺省、空字符串、null、空数组、空文本块、空缓存块、空 developer 的转换均有回归测试；
非空字符串和缓存分段的既有测试继续通过，完整 vet/race 检查通过。

## 复现

这些命令会使用 `.env` 中的真实凭据并产生少量推理用量。默认每个普通请求最多 96 输出 token，工具调用最多 128。

```bash
cd proxy
go build -o /tmp/commandcode-live-proxy ./cmd/commandcode-proxy
cd ..

python3 analysis/v1.51.3/scripts/live-check.py \
  --binary /tmp/commandcode-live-proxy --output /tmp/live-key1.json

python3 analysis/v1.51.3/scripts/live-check.py \
  --binary /tmp/commandcode-live-proxy --output /tmp/live-key2.json \
  --key-index 2 --suite basic

python3 analysis/v1.51.3/scripts/live-check.py \
  --binary /tmp/commandcode-live-proxy --output /tmp/live-restart.json \
  --suite basic --only plain,restart_same_request --restart-check --fixed-test-seed
```

去掉最后的 `--fixed-test-seed` 即按当前 `.env` 配置比较重启。`--only` 可选择个别案例，`--auth-mode passthrough` 可验证透传，`--max-tokens 512 --only vision_data_url` 可单独复测图片。脚本保存各案例状态，需检查结果 JSON；退出码不代表所有案例都通过。

修复后本地 `go vet ./...` 与 `go test -race -count=1 ./...` 全部通过。
