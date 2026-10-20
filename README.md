# AetherRelay

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

AetherRelay 是一个可自行部署的 AI 网关，通过统一地址接入多个 AI 服务商、ChatGPT Web 与 Codex OAuth 账号池。客户端使用 OpenAI 或 Anthropic 兼容接口，网关负责模型路由、权限校验、可恢复故障切换和用量记录。

项目采用 Go 实现，单进程运行，内嵌 DuckDB 与中英文 Web 管理台，无需外部数据库或消息队列。

## 主要功能

| 功能 | 说明 |
| --- | --- |
| 标准接口 | 支持 Chat Completions、Responses、Messages、Completions、Embeddings 和 Images；具体可用端点由模型目录发布 |
| 模型路由 | 按精确模型 ID 匹配 Provider，支持优先级、健康检查、熔断和有界故障切换 |
| 协议转换 | 提供 Chat Completions ↔ Messages 文本转换及按能力声明开放的 Responses ↔ Messages 转换 |
| 客户端 Key | 创建、启停、轮换密钥，并按 Key 设置指定 Provider 或全部 Provider 的访问权限 |
| 账号池 | 统一展示 ChatGPT Web / Codex CLI 账号，分别管理凭据、刷新状态、模型目录、额度和冷却，支持整体迁移 |
| Codex OAuth | 提供 Responses HTTP/SSE/WebSocket、compact 和本地输入 Token 预估；上游优先 WebSocket，在可确认安全时回退 HTTP |
| 模型指纹检测 | 对指定 Codex 账号手动采样，展示参考库候选排名、采样诊断，并下载完整 JSON 报告 |
| 用量与监控 | 按时间、Key、Provider、模型和结果筛选用量，查看缓存使用率、TPS、当前并发及完整调用详情，支持 CSV 导出与 Prometheus 指标 |
| 管理台工具 | 临时对话、逐轮联网搜索、图片任务与图片库；工具可用性取决于账号和模型能力 |

客户端必须使用已创建的客户端 Key。原始客户端 Key 仅在创建或轮换时显示一次，数据库保存摘要及展示 hint；Provider 和账号池的可恢复凭据使用主密钥加密保存。

## 快速开始

### 容器部署

需要 Docker 和 Docker Compose。部署脚本会拉取镜像、生成配置与凭据加密主密钥，并交互式初始化管理员登录：

```bash
git clone https://github.com/muidea/AetherRelay.git
cd AetherRelay
./scripts/deploy-docker.sh
```

也可以直接运行自包含脚本，部署文件写入当前目录的 `deploy/`：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/muidea/AetherRelay/main/scripts/deploy-docker.sh)
```

镜像为 `ghcr.io/muidea/aetherrelay`，提供 Linux amd64 与 arm64。重复运行脚本会保留现有配置和加密主密钥。完整参数、远程访问与固定版本部署见[安装与部署](docs/deployment.md#一键部署脚本推荐)。

### 源码运行

需要 Go 1.26+（仓库工具链为 Go 1.26.7）及支持 CGO 的本机 C/C++ 构建工具链。DuckDB 使用平台绑定，发布构建应在对应平台原生执行。

```bash
git clone https://github.com/muidea/AetherRelay.git
cd AetherRelay
cp config.example.yaml config.yaml
# 仅在首次初始化时生成；保存该值，并在后续启动时注入同一主密钥。
export AETHERRELAY_CREDENTIAL_KEY="$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\r\n')"
make run
```

默认监听 `http://127.0.0.1:8080`，管理台为 [`/admin/`](http://127.0.0.1:8080/admin/)。源码示例默认仅允许本机管理；需要远程管理时，先按照[配置参考](docs/configuration.md)启用管理员登录。

发布二进制、容器手动部署、反向代理及升级回滚见[安装与部署](docs/deployment.md)。

### 首次配置

1. 打开管理台，在「Provider 管理」添加服务商，或在「账号池」导入 ChatGPT Web / Codex OAuth 凭据。
2. 核对服务商启用状态与有效模型目录；账号池可手动同步模型及刷新用量。
3. 在「客户端 Key」创建密钥，选择允许访问的 Provider，并保存本次返回的完整 Key。
4. 将客户端的 Base URL 和 Key 指向网关，按目录中的精确模型 ID 发起请求。

Provider 定义、客户端 Key 和账号池凭据通过管理台维护；`config.yaml` 用于进程配置与静态模型元数据。仅添加 `model_metadata` 不会创建模型路由。没有外部客户端 Key 时服务仍可启动，但数据端点会拒绝未认证调用。

## 客户端接入

| 客户端协议 | Base URL | 认证 |
| --- | --- | --- |
| OpenAI 兼容 | `http://127.0.0.1:8080/v1` | `Authorization: Bearer <client-key>` |
| Anthropic 兼容 | `http://127.0.0.1:8080` | `X-API-Key: <client-key>` |

先读取当前 Key 可访问的模型目录：

```bash
export AETHERRELAY_CLIENT_KEY='替换为管理台创建的客户端密钥'
curl -sS http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $AETHERRELAY_CLIENT_KEY"
```

以下为 Responses 调用示例，将 `gpt-6.1-sol` 替换为目录中支持 `/v1/responses` 的实际模型：

```bash
curl -sS http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer $AETHERRELAY_CLIENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6.1-sol","input":"请简要介绍你能完成的任务。","stream":false}'
```

模型 ID 区分大小写。`GET /v1/models` 返回各模型的 `supported_endpoints` 与已声明能力；路由候选仍受权限、账号额度、冷却和并发限制约束。跨协议转换按已发布能力开放，图片、工具、历史续接等功能不能仅凭模型名称推断，详见[外部应用集成指南](docs/integration.md)。

故障切换受执行进度限制：已向客户端输出业务内容，或上游是否执行无法确定时，不会自动重放生成请求。Codex WebSocket 的安全 HTTP 回退边界见[传输说明](docs/design/codex-ws-first-http-fallback.md)。

## 模型指纹检测

在管理台「账号池」中选择 Codex 账号的「检测模型指纹」，设置模型和推理强度后手动开始。每次任务最多发起三条独立挑战请求，可能消耗额度；打开窗口只查询最近报告，不会自动开始检测。

结果默认展示检测摘要、前三名候选和每条采样状态，其余候选可展开。失败原因保留完整信息，原始用量、采样输出及内部标识可通过 JSON 展开区或下载报告查看。窗口按屏幕尺寸调整布局，长文本换行，标题与操作按钮保持可见。

候选排名与库内相对权重只表示参考库中的行为接近程度，不能确认真实模型身份或能力回退。当前不支持 ChatGPT Web 槽位检测。报告完成后保留 30 分钟，服务重启后丢失；关闭窗口不会取消服务端任务，可使用「取消检测」终止。实现与验收见[ModelTrace 集成说明](docs/modeltrace-integration.md)。

## 运行与维护

- `GET /healthz`：服务健康检查。
- `GET /metrics`、`GET /stats`、`GET /stats/stream`：Prometheus 指标、运行快照与实时推送，默认仅本机可访问。
- 管理台「使用统计」：查看历史调用与全局当前并发；并发值在页面可见时每 2 秒刷新，不受历史筛选影响，读取失败显示 `—`。
- 当前并发统计支持的代理业务请求从进入到结束的完整生命周期，内部重试不重复计数；重启后归零。
- 每个实例独占 `state.dir`。备份配置、数据目录以及 `AETHERRELAY_CREDENTIAL_KEY`；恢复加密凭据需要原主密钥。

管理台临时对话、搜索及图片工具使用服务端内建 `builtin-local` 作用域记录用量，该标识不能作为外部客户端 Key 使用。ChatGPT Web 生图输出栅格图，明确指定尺寸时可在本地裁切或缩放，不提供 SVG 真矢量输出。

完整的超时、归档、监控、备份与故障诊断见[运维与发布](docs/operations.md)。

## 开发与构建

```bash
make run                          # 使用 config.yaml 启动
make build                        # 构建当前平台二进制
make check                        # Go 格式化、vet 与全量 Go 测试
make check-codex                   # Codex 代理维护合同检查
node --test web/admin/*.test.cjs   # 管理台前端回归（需要 Node.js）
docker build -t aetherrelay:dev .  # 构建本地容器镜像
make release-package VERSION=v1.2.3
```

管理员凭据工具：

```bash
./AetherRelay admin password-hash
./AetherRelay admin set-credentials --username ops-admin --config config.yaml
```

推送 `vX.Y.Z` tag 触发 GitHub Actions 发布流程；支持的平台与构建要求见[运维与发布](docs/operations.md#构建与发布)。

## 文档

| 主题 | 文档 |
| --- | --- |
| 安装、部署与升级回滚 | [安装与部署](docs/deployment.md) |
| 端点、路由、管理台与账号池 | [功能说明](docs/features.md) |
| 模型能力发现与客户端接入 | [集成指南](docs/integration.md) |
| 配置、客户端 Key 与 Provider | [配置参考](docs/configuration.md) |
| 监控、归档、备份与发布 | [运维与发布](docs/operations.md) |
| 目录职责与运行单元生命周期 | [代码结构](docs/structure.md) |
| 模型指纹检测与验收 | [ModelTrace 集成说明](docs/modeltrace-integration.md) |
| 设计与维护合同 | [设计索引](docs/design/index.md) · [Codex 代理维护合同](docs/design/codex-proxy-maintenance-contract.md) |

[`config.example.yaml`](config.example.yaml) 是可复制的启动配置。带日期的计划、审计及现场记录属于历史材料；已归档文档位于 [`docs/archive/`](docs/archive/)。

## 许可证

AetherRelay 采用 [MIT License](LICENSE)。Copyright (c) 2026 muidea and AetherRelay contributors。

第三方依赖和引用资产保留各自的许可证与版权声明。ModelTrace 算法及参考资产的来源与许可见[第三方说明](internal/pkg/modeltrace/NOTICE.md)和[原始 MIT 许可证](internal/pkg/modeltrace/assets/vendor/modeltrace/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/LICENSE)；`vendor/` 下的依赖以各自附带的许可证为准。
