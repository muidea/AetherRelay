# ai-proxy

轻量级本地 LLM API 网关。它提供 OpenAI 和 Anthropic 标准入站端点，严格按请求中的 exact `model` 解析有序 Provider 候选链；仅在响应尚未提交且失败可安全重试时回退到低优先级候选。用量明细持久化至进程内嵌 DuckDB，并提供本地 Web 管理页。

## 快速开始

要求 Go 1.24+。先从示例创建配置，填入 Provider 和模型目录，再启动服务：

```bash
cp config.example.yaml config.yaml
export OPENAI_API_KEY=sk-... # 供 config.yaml 中的 ${OPENAI_API_KEY} 展开
make run
```

默认地址为 `http://127.0.0.1:8080`。启动后可访问：

- [Provider、客户端 Key、使用统计、账号池与功能集管理](http://127.0.0.1:8080/admin/)（账号池区分 ChatGPT Web / Codex OAuth；功能集包含图片任务、图片库、在线搜索与临时对话。在线搜索历史按管理员隔离并服务端保存；临时对话可按轮启用受限联网搜索。默认仅 loopback；可启用账号密码登录后远程访问，见配置参考）
- `GET /healthz`
- `GET /metrics`、`GET /stats`（默认仅 loopback）

客户端使用裸模型名与标准地址：

```text
OpenAI API base:    http://127.0.0.1:8080/v1
Anthropic API base: http://127.0.0.1:8080
```

所有数据端点都要求客户端 API Key：OpenAI 客户端使用 `Authorization: Bearer <key>`，Anthropic 客户端使用 `X-API-Key: <key>`。缺失、未知或禁用的 Key 返回 401，且不产生用量记录。

## 容器快速开始

发布镜像位于 `ghcr.io/muidea/ai-proxy`，提供 Linux amd64 与 arm64 清单。复制配置、按容器网络调整 `listen_addr` 与 `state.dir`，再启动：

```bash
mkdir -p deploy/config
cp config.example.yaml deploy/config/config.yaml
# 编辑 deploy/config/config.yaml：listen_addr=0.0.0.0:8080，state.dir=/var/lib/ai-proxy
# 同时配置实际启用 Provider 的环境变量与客户端 Key。
docker compose up -d
```

完整的配置目录权限、Admin 登录、持久化与升级步骤见[容器部署](docs/operations.md#容器部署)。

## 常用命令

```bash
make run                         # 使用 config.yaml 启动
make check                       # 格式、vet、全量测试
make build                       # 构建当前平台二进制
docker build -t ai-proxy:dev .   # 构建本地容器镜像
make release-package VERSION=v1.2.3
ai-proxy admin password-hash     # 交互式生成 Admin Argon2id 密码哈希
ai-proxy admin set-credentials --username ops-admin --config config.yaml # 创建或重置 Admin 登录凭据
```

完整多平台发布由推送 `vX.Y.Z` tag 的 GitHub Actions 完成；详情见[运维与发布说明](docs/operations.md#构建与发布)。

## 文档

| 主题 | 文档 |
| --- | --- |
| 配置、客户端 Key、Provider 管理 | [配置参考](docs/configuration.md) |
| 运行、监控、归档、探针、备份与发布 | [运维与发布](docs/operations.md) |
| 目录职责与 magicCommon 生命周期 | [代码结构](docs/structure.md) |
| 已实现功能的设计背景 | [客户端 Key](docs/client-api-key-management-design-2026-07-20.md)、[Admin 登录](docs/admin-login-security-design-2026-07-23.md)、[ChatGPT Web 管理](docs/chatgpt-web-admin-closure-design-2026-07-26.md)、[ChatGPT Web 联网搜索](docs/chatgpt-web-search-design-2026-08-03.md)、[Codex OAuth 号池](docs/codex-oauth-account-pool-design-2026-07-30.md)、[SSE](docs/unified-sse-streaming-design-2026-07-23.md) |
| 路由与协议设计参考 | [Provider Capability Contract](docs/provider-capability-contract-design-2026-07-15.md) |

带日期的计划、审计和现场记录是历史材料，不是运行时合同；当前行为以本 README、配置参考、运维说明、代码结构以及自动化测试为准。

`config.example.yaml` 是可复制的完整配置起点；所有 Provider 必须显式写入配置文件，不能由环境变量创建。
