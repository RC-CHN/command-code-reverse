# command-code-reverse

对 [Command Code](https://commandcode.ai) CLI 协议（初始分析 `1.32.2`，最新静态核对 `1.51.3`）的
逆向工程分析，外加一个生产级 Go 代理，把它的上游 API 包装成 OpenAI 兼容接口。

[English README](./README.md)

## 仓库内容

| 路径 | 说明 |
|---|---|
| `analysis/v1.32.2/` | 协议分析（端点、鉴权、wire 格式、流事件、套餐校验、指纹算法、缓存行为——均已实测验证） |
| [`analysis/v1.51.3/`](./analysis/v1.51.3/README.md) | 相对 1.40.1 的新版协议差异、发布包校验记录、模型目录对比与本地兼容测试 |
| `proxy/` | **commandcode-proxy**——把 `POST /alpha/generate`（NDJSON 流）桥接成 `/v1/chat/completions`（OpenAI SSE）的 Go 服务 |
| `.github/workflows/` | CI（vet + race 测试 + lint + 镜像构建）与 tag 触发的发版流水线 |

## 为什么做这个

Command Code 的多模型订阅绑死在自家 CLI 上。它的 `/alpha/generate` 说
NDJSON 方言、要带一整套伪装请求头、计费/套餐失败以流内标记的形式返回——
跟 OpenAI 生态完全不兼容。这个代理让任何 OpenAI SDK 直接可用，同时把错误
语义摆正（`402 insufficient credits`、`403 model not in plan`），并提供
多 key 池与可观测性。

## 快速开始

```bash
cp .env.example .env   # 填 COMMAND_CODE_API_KEY 和 PROXY_API_KEY
cd proxy && go run ./cmd/commandcode-proxy
```

```bash
curl -N http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","stream":true,
       "messages":[{"role":"user","content":"hi"}]}'
```

Docker / k8s 部署与完整配置说明见 [`proxy/README.md`](./proxy/README.md)。

## 设计要点

- **零第三方依赖**——只用 Go 标准库。
- **fill-first 多 key 池** + 每 key 熔断器，最大化前缀缓存亲和
  （实测相同前缀缓存命中率 >98%）。
- **可重放设备指纹**——seed 的 HMAC 确定性派生，k8s pod 重调度后指纹
  不变，无需 PVC。
- **终止标记映射**——流内的计费/套餐失败变成明确的 402/403 响应，
  而不是莫名其妙的空 429。
- **信息卫生**——上游内部事件（`start-step`、`provider-metadata`）一律
  剥离；成本走日志 / Prometheus 指标 / 可选响应头暴露。
- **运维友好**——`/healthz`、带缓存探针的 `/readyz`、`/metrics`、
  `/version`，SIGTERM 优雅排空在途流。

## 发版

```bash
# 更新 CHANGELOG.md，然后：
git commit -m "chore(release): vX.Y.Z"
git tag vX.Y.Z && git push && git push origin vX.Y.Z
```

发版流水线会把跨平台二进制发布到 GitHub Releases，多架构镜像推到 GHCR。

## 许可证

见 [LICENSE](./LICENSE)。本项目是互操作性研究；计费校验在服务端执行，
代理不做任何绕过。
