# AetherRelay

一个本地运行的 AI 网关：一个地址、一套标准接口，就能接入多家 AI 服务。现有 OpenAI 或 Anthropic 客户端改一下 base URL 即可使用；网关负责挑选服务商、在故障时自动切换，并记录每一次调用的用量。单进程、无外部依赖，开箱即用，附带本地管理页面。

## 功能亮点

- **统一入口，无需改代码**：只提供 OpenAI 与 Anthropic 两种标准接口，兼容绝大多数 AI 客户端与工具；按请求中的模型名精确匹配对应的服务商。
- **多服务商自动切换**：同一个模型可以配置多家服务商并设定优先级，网关按序挑选；上游暂时不可用且响应尚未返回时，自动尝试下一个服务商。
- **用量自动记账**：每次调用自动记录用量与归属（哪个客户端、哪个模型、哪个服务商），管理页面可按时间、Key、模型筛选并导出 CSV。
- **网页管理台**：在浏览器中管理服务商与客户端密钥、查看用量统计、体检各服务商可用性，并展示系统信息（版本、运行时长、开放端点与认证方式）。
- **ChatGPT 网页账号接入**：导入 ChatGPT 网页账号即可使用文本对话、图片生成与联网搜索，无需官方 API Key。
- **Codex OAuth 账号池**：接入 Codex CLI 登录的账号，提供 Responses 接口；模型自动发现、登录态自动刷新、额度情况一目了然。
- **管理台工具集**：临时对话（支持发图与逐轮联网搜索）、图片任务与图片库、在线搜索，供管理员日常使用。
- **安全默认**：默认只监听本机；所有请求必须携带客户端密钥；密钥与账号凭据不写入日志、数据库或网页。

## 快速开始

以下快速体验需要 Go 1.24+（源码运行）；也可以直接用发布二进制或容器镜像，见[安装与部署](docs/deployment.md)。先从示例创建配置，再启动：

```bash
cp config.example.yaml config.yaml
export AETHERRELAY_CREDENTIAL_KEY="$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\r\n')"
make run
```

ChatGPT Web 与 Codex OAuth 账号池始终启用。启动后通过管理台导入账号或添加直连 Provider；Provider 定义和三类可恢复凭据均加密保存在 DuckDB，不写入 `config.yaml`。

默认地址为 `http://127.0.0.1:8080`。启动后可访问：

- [本地管理台](http://127.0.0.1:8080/admin/)：管理服务商与客户端密钥、查看用量统计与系统信息；「账号池」区分 ChatGPT Web / Codex OAuth，「功能集」包含图片任务、图片库、在线搜索与临时对话（搜索历史按管理员隔离并保存在服务器，临时对话支持逐轮联网搜索）。默认仅本机可访问，可启用账号密码登录后远程访问，见配置参考
- `GET /healthz`
- `GET /metrics`、`GET /stats`（默认仅本机可访问）

客户端直接使用模型名，把请求地址指向网关即可：

```text
OpenAI API base:    http://127.0.0.1:8080/v1
Anthropic API base: http://127.0.0.1:8080
```

所有调用都需携带客户端密钥：OpenAI 客户端用 `Authorization: Bearer <key>`，Anthropic 客户端用 `X-API-Key: <key>`。缺少密钥、密钥错误或已停用时请求会被拒绝（401），且不会产生用量记录。

管理台工具集由服务端内建的 `builtin-local` scope 归属（不是可对外携带的 secret，不能轮换或删除）；图片任务/图片库未指定 `api_key_id` 时默认使用它。ChatGPT Web 生图只返回栅格图：明确 `WIDTHxHEIGHT` 会在本地裁切/缩放到精确像素，`auto` 保留上游尺寸；SVG 真矢量输出不支持。

## 容器快速开始

发布镜像位于 `ghcr.io/muidea/aetherrelay`，提供 Linux amd64 与 arm64 清单。**一条命令完成部署**（引导生成配置、初始化 Admin 凭据并启动容器）：

```bash
git clone https://github.com/muidea/AetherRelay.git && cd AetherRelay && ./scripts/deploy-docker.sh
```

不想 clone 仓库时，也可以直接运行自包含部署脚本（产物全部落在 `./deploy`，不依赖仓库文件）：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/muidea/AetherRelay/main/scripts/deploy-docker.sh)
```

脚本会实时提示输入两次 Admin 密码（输入不回显，密码不进入参数、环境变量或日志），完成后登录管理台添加 Provider 或导入账号池凭据。无交互场景用 `--admin-password-hash '...'` 注入预生成哈希，或 `--skip-admin` 跳过；完整参数见[安装与部署](docs/deployment.md#一键部署脚本推荐)。

也可以手动复制配置、按容器网络调整 `listen_addr` 与 `state.dir`，再启动：

```bash
mkdir -p deploy/config deploy/data
cp config.example.yaml deploy/config/config.yaml
# 编辑 deploy/config/config.yaml：listen_addr=0.0.0.0:8080，state.dir=/var/lib/aetherrelay
# compose.yaml 会将 deploy/data 映射为容器内 /var/lib/aetherrelay。
# 同时配置实际启用 Provider 的环境变量与客户端 Key。
docker compose up -d
```

完整的配置目录权限、Admin 登录、持久化与升级步骤见[安装与部署](docs/deployment.md#容器部署)。

## 常用命令

```bash
make run                         # 使用 config.yaml 启动
make check                       # 格式、vet、全量测试
make build                       # 构建当前平台二进制
docker build -t aetherrelay:dev .   # 构建本地容器镜像
make release-package VERSION=v1.2.3
AetherRelay admin password-hash     # 交互式生成 Admin Argon2id 密码哈希
AetherRelay admin set-credentials --username ops-admin --config config.yaml # 创建或重置 Admin 登录凭据
```

完整多平台发布由推送 `vX.Y.Z` tag 的 GitHub Actions 完成；详情见[运维与发布说明](docs/operations.md#构建与发布)。

## 文档

| 主题 | 文档 |
| --- | --- |
| 安装与部署（源码/二进制/容器、升级回滚） | [安装与部署](docs/deployment.md) |
| 当前功能说明（端点、路由、Admin、账号池等） | [功能说明](docs/features.md) |
| 外部应用集成（模型能力发现与端点选择） | [集成指南](docs/integration.md) |
| 配置、客户端 Key、Provider 管理 | [配置参考](docs/configuration.md) |
| 运行、监控、归档、探针、备份与发布 | [运维与发布](docs/operations.md) |
| 目录职责与 magicCommon 生命周期 | [代码结构](docs/structure.md) |
| 最终设计（按功能结构） | [设计索引](docs/design/index.md) · [核心代理与路由](docs/design/proxy-core.md) · [安全与认证](docs/design/security.md) · [ChatGPT Web 能力](docs/design/chatgpt-web.md) · [Codex OAuth 账号池](docs/design/codex-oauth.md) |

带日期的计划、审计和现场记录是历史材料，不是运行时合同；当前行为以本 README、配置参考、运维说明、代码结构以及自动化测试为准。已完成的中间过程与失效文档归档在 [`docs/archive/`](docs/archive/)。

`config.example.yaml` 是可复制的启动配置；运行期 Provider 通过管理台创建并加密保存在 DuckDB。
