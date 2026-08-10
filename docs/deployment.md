# 安装与部署

本文说明 `AetherRelay` 的完整安装部署流程：环境要求、获取发布产物、配置准备、首次启动与验证、本机部署、容器部署、升级回滚与常见问题。配置项的完整含义见[配置参考](configuration.md)，运行期观测、备份与发布见[运维与发布](operations.md)，全部功能清单见[功能说明](features.md)。

## 前置要求

`AetherRelay` 是单进程、单二进制程序，不依赖外部数据库、消息队列或常驻中间件：

| 项目 | 要求 |
| --- | --- |
| 操作系统 | Linux（amd64 / arm64）、macOS（arm64）、Windows（amd64）。容器镜像提供 Linux amd64 与 arm64 清单 |
| Go（仅源码构建） | 1.24+ |
| 运行时依赖 | 无外部服务。唯一结构化状态是进程内嵌 DuckDB 文件 |
| 网络 | 默认监听 `127.0.0.1:8080`；上游 Provider 需可直连（或经账号代理） |

发布平台矩阵：Release workflow 在 Linux amd64/arm64、macOS arm64 原生 runner 上打包 `.tar.gz` 与 SHA-256 文件。**不要从 amd64 交叉编译 arm64 目标**：DuckDB Go bindings 需要相应的原生目标 runner。Windows 不发布原生二进制（代码依赖 Unix termios 且需要 MinGW CGO 工具链），Windows 用户请使用 WSL2 或容器部署。

## 获取发布产物

### 方式一：GitHub Releases 二进制包（推荐）

从 GitHub Releases 页面下载对应平台的 `.tar.gz` 包及其 `.tar.gz.sha256` 校验文件，解压后即可运行：

```bash
VERSION=vX.Y.Z
tar xzf AetherRelay_${VERSION#v}_linux_amd64.tar.gz
./AetherRelay -h   # 校验文件可用后运行
```

发布包的 `main.version` 由 Release workflow 注入；`make release-package VERSION=vX.Y.Z` 可在本机原生平台打出相同结构的包。

### 方式二：容器镜像

镜像发布到 GitHub Container Registry：`ghcr.io/muidea/aetherrelay`。`main` 成功构建后更新 `latest` 与 `main`，发布 `vX.Y.Z` tag 后推送对应的 `X.Y.Z`、`X.Y` 与 Git SHA 标签；生产部署应固定到完整版本或 SHA，不要仅依赖 `latest`。每个标签同时提供 Linux `amd64` 与 `arm64` 镜像。

### 方式三：源码构建

```bash
git clone <repo-url> AetherRelay && cd AetherRelay
make build              # 产出 ./AetherRelay；可用 BINARY=bin/AetherRelay 指定路径
./AetherRelay -h
```

构建走 vendor 依赖，`Makefile` 默认 `-buildvcs=false`，非完整 git worktree 下也不会构建失败。开发调试常用 `make run`（读 `config.yaml` 或 `AETHERRELAY_CONFIG` 启动）。

## 配置准备

先从示例创建配置，再编辑：

```bash
cp config.example.yaml config.yaml
export AETHERRELAY_CREDENTIAL_KEY="$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\r\n')"
${EDITOR:-vi} config.yaml
```

配置要点（完整说明见[配置参考](configuration.md)）：

- Provider 通过管理台创建；完整定义与 API Key 使用 `AETHERRELAY_CREDENTIAL_KEY` 加密后保存到 DuckDB，不写回 `config.yaml`。
- `AETHERRELAY_CREDENTIAL_KEY` 必须是 Base64 编码的 32 字节随机值。一键部署自动生成并保存在私有 `.env`；手动部署必须自行生成并注入，丢失后无法解密 Provider 和账号池凭据。
- 每个 enabled Provider 仍必须显式声明 `protocol`、`base_url`、`endpoints` 与 `models`，但这些字段由管理页提交到运行期 Provider 存储。
- `model_metadata` 只登记可选模型元数据，模型 ID exact 且严格区分大小写；它不发布模型或创建路由。Provider 的精确 `models` 与账号池发现结果决定实际模型，通配 pattern 只参与候选匹配。
- `state.dir` 是单实例唯一的持久化工作区（DuckDB 用量、账号池、图片元数据与交互归档都在其中），多实例不得共享。
- 客户端 API Key 不在配置文件中声明，由 Admin 创建并保存到 DuckDB；创建时必须绑定指定 Provider 或选择全部 Provider。数据库没有 Key 时服务仍可启动，但所有数据端点返回 401，Admin 仍可用于创建第一个 Key。

```yaml
server:
  listen_addr: 127.0.0.1:8080

state:
  dir: var
  database: aetherrelay.duckdb

model_metadata:
  gpt-5.5:
    context_window_tokens: 272000
    max_output_tokens: 128000
    reasoning_supported: true
    reasoning_default_effort: medium
    reasoning_efforts: [none, low, medium, high, xhigh]
    native_responses_tools: true
    native_responses_images: false

```

Provider 不在启动配置中声明；服务启动后通过管理台创建，并加密保存到 DuckDB。

## 首次启动与验证

```bash
make run # 或直接运行发布二进制：./AetherRelay
```

启动后验证：

```bash
curl -s http://127.0.0.1:8080/healthz                 # 健康检查
curl -s http://127.0.0.1:8080/v1/models               # 有效模型目录（需客户端 Key 认证）
```

用任意 OpenAI/Anthropic 客户端指向标准地址，携带客户端 Key：

```text
OpenAI API base:    http://127.0.0.1:8080/v1
Anthropic API base: http://127.0.0.1:8080
Authorization:      Bearer <client_api_key>   # OpenAI 风格
X-API-Key:          <client_api_key>          # Anthropic 风格
```

默认仅 loopback 可访问 `/admin`、`/metrics`、`/stats`；需要远程访问时见下文"Admin 登录"与[运维与发布](operations.md#启动与客户端接入)。

## 本机部署

### 以服务方式运行（systemd 示例）

发布二进制或构建产物放入受控目录，用任意进程管理器托管。以下是一个最小 systemd unit 示例（**示例**，按实际路径调整）：

```ini
# /etc/systemd/system/aetherrelay.service
[Unit]
Description=AetherRelay local LLM gateway
After=network-online.target

[Service]
ExecStart=/opt/aetherrelay/AetherRelay
EnvironmentFile=/etc/aetherrelay/aetherrelay.env    # 存放凭据主密钥与 Admin 哈希等私有变量
WorkingDirectory=/opt/aetherrelay
User=aetherrelay
Group=aetherrelay
Restart=on-failure
ProtectSystem=strict
ReadWritePaths=/opt/aetherrelay

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now aetherrelay
journalctl -u aetherrelay -f
```

`AETHERRELAY_CONFIG` 可指定非默认配置路径（默认 `config.yaml`，位于工作目录）。`state.dir` 与配置路径按配置文件所在目录解析相对路径，建议配置与数据目录都由专用运行用户私有持有。

### Admin 登录（可选）

默认 Admin 仅 loopback 可访问。需要远程运维时启用账号密码登录，先交互式生成 Argon2id 哈希（密码不进入 argv / 环境变量 / 日志）：

```bash
AetherRelay admin password-hash
# 或直接创建/重置登录凭据（自动写入 server.admin_auth_enabled、账号与哈希）：
AetherRelay admin set-credentials --username ops-admin --config config.yaml
```

然后在 `server` 中配置 `admin_auth_enabled: true`、`admin_username` 与 `admin_password_hash`（或对应环境变量），并经 HTTPS 反向代理暴露 `<admin_base_path>`（默认 `/admin`）。完整要点见[配置参考](configuration.md#安全登录模式)。

## 容器部署

### 一键部署脚本（推荐）

仓库提供 [`scripts/deploy-docker.sh`](../scripts/deploy-docker.sh) 自动化完整流程：生成配置（含容器适配）、初始化 Admin 凭据、生成自包含的 `docker-compose.yml` 与 `.env`、拉取镜像、启动容器并等待就绪。产物全部落在部署目录（默认 `./deploy`，已被 `.gitignore` 忽略），运行数据直接持久化到其中的 `data/` 子目录。

```bash
# 交互式：按实时提示输入两次 Admin 密码（输入不回显）
./scripts/deploy-docker.sh

# 无交互（CI / 无人值守）：预先用 `AetherRelay admin password-hash` 生成哈希
./scripts/deploy-docker.sh --admin-password-hash '$argon2id$v=19$...'
```

常用参数：

| 参数 | 说明 |
| --- | --- |
| `--dir <path>` | 部署目录，默认 `./deploy` |
| `--image <image>` | 镜像引用，默认 `ghcr.io/muidea/aetherrelay:latest`（也可用 `AETHERRELAY_IMAGE`） |
| `--admin-username <name>` | Admin 用户名，默认 `ops-admin`（仅限 `[A-Za-z0-9._-]`） |
| `--admin-password-hash <phc>` | 直接注入 Argon2id PHC 哈希（跳过交互生成） |
| `--skip-admin` | 不启用 Admin 登录（仅本机访问） |
| `--listen <addr>` | 宿主机端口绑定，默认 `127.0.0.1:8080`；改 `0.0.0.0:8080` 会放开到所有网卡并打印安全警告 |

要点：

- **Admin 凭据初始化**：密码提示直接显示在当前 TTY，输入不回显；`password-hash` 在容器内运行，密码不进入参数、环境变量或日志。其 stdout 通过临时挂载文件回传哈希，随即写入 `.env` 的 `AETHERRELAY_ADMIN_PASSWORD_HASH` 并删除临时文件；`config.yaml` 中仅以 `${AETHERRELAY_ADMIN_PASSWORD_HASH}` 引用。
- **凭据加密**：脚本首次运行自动生成 `AETHERRELAY_CREDENTIAL_KEY`；Provider 与两类账号池凭据均以该密钥加密写入 DuckDB。重复运行保留原密钥，禁止随意重置。
- **Provider 配置**：默认配置不内置 Provider，因此脚本不询问任何固定厂商 Key。部署完成后从管理台添加任意 Provider，或在账号池页面导入 ChatGPT Web / Codex OAuth 凭据。
- 容器内 `listen_addr` 恒为 `0.0.0.0:8080`，暴露面由宿主机端口绑定（`--listen`）控制；默认仅 `127.0.0.1:8080`。
- `.env` 生成后为 `chmod 600`，包含凭据主密钥与 Admin 哈希，务必保持私有、不入版本库。
- 重复运行时保留已有 `config.yaml` 不覆盖；`.env` 中已配置的值保留为默认。
- 脚本创建 `config/` 与 `data/`，并尽力将两者设为 UID/GID `10001:10001`；权限不足时会提示手动修正。配置目录不可写时，内建 Provider 路由策略、客户端 Key 与管理偏好只读；Provider 目录仍由 DuckDB 安全存储负责。数据目录不可写时服务无法保存运行数据。

### Docker Compose（手动）

仓库中的 [`compose.yaml`](../compose.yaml) 是推荐起点。先建立一个可原子替换的**目录**挂载，而不是只挂载单个 `config.yaml` 文件：管理页保存实例默认语言等 YAML 设置时会通过临时文件和 `rename` 更新配置；Provider 与客户端 Key 直接写入 DuckDB。

```bash
mkdir -p deploy/config deploy/data
cp config.example.yaml deploy/config/config.yaml
credential_key="$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\r\n')"
printf 'AETHERRELAY_CREDENTIAL_KEY=%s\n' "$credential_key" >> .env
chmod 600 .env

# 容器内需要监听全部网卡，且状态必须落到宿主机数据目录映射点。
${EDITOR:-vi} deploy/config/config.yaml
```

至少将配置调整为下列形态，并删除或禁用未使用的示例 Provider；所有仍启用的远程 Provider 都必须有可用凭据：

```yaml
server:
  listen_addr: 0.0.0.0:8080
  # Docker 转发连接在容器内不是 loopback。若要使用 /admin，必须开启登录保护。
  admin_auth_enabled: true
  admin_username: ops-admin
  admin_password_hash: ${AETHERRELAY_ADMIN_PASSWORD_HASH}
  # 经 HTTPS 反向代理对外提供 Admin 时设为 true。
  admin_session_cookie_secure: true

state:
  dir: /var/lib/aetherrelay
  database: aetherrelay.duckdb
```

生成 Admin 密码哈希时不会启动网关，也不会读取配置：

```bash
docker run --rm ghcr.io/muidea/aetherrelay:latest AetherRelay admin password-hash
```

在未纳入版本控制的 `.env` 或容器编排的 secret 中保存凭据主密钥与 Admin 哈希。例如：

```dotenv
AETHERRELAY_CREDENTIAL_KEY=<base64-encoded-32-byte-key>
AETHERRELAY_ADMIN_PASSWORD_HASH=$argon2id$...
```

若需要从管理页修改配置，配置目录必须由 UID `10001`（容器内 `aetherrelay` 用户）可写；为了让配置本身和同目录临时文件保持私有，可在受控主机上执行：

```bash
sudo chown -R 10001:10001 deploy/config deploy/data
sudo chmod 700 deploy/config deploy/data
sudo chmod 600 deploy/config/config.yaml
docker compose up -d
docker compose logs -f aetherrelay
```

只需要只读启动配置时，可将 Compose 的配置卷改为 `:ro`；管理页会禁用依赖 YAML 写入的设置，DuckDB 中的 Provider 与账号池仍可管理。容器的数据面默认仅发布到宿主机 `127.0.0.1:8080`。需要远程访问时，优先通过 HTTPS 反向代理转发，并保留原始 `Host`；不要直接把端口公开到不受控网络。启用登录后还应保留 `admin_session_cookie_secure: true`。

检查状态：

```bash
docker compose ps
docker compose exec aetherrelay curl --fail http://127.0.0.1:8080/healthz
```

宿主机 `deploy/data/` 保存 DuckDB、图片、缩略图、交互归档与加密凭据；删除或重建容器不会清除该目录。`deploy/.env` 中的 `AETHERRELAY_CREDENTIAL_KEY` 是解密 Provider 与账号池凭据的唯一主密钥，必须与 `deploy/config/`、`deploy/data/` 一起安全备份，但不得复制进数据库或配置文件。

### 直接运行镜像

不用 Compose 时也必须挂载宿主机配置目录与数据目录：

```bash
mkdir -p deploy/config deploy/data
sudo chown -R 10001:10001 deploy/config deploy/data

docker run -d --name aetherrelay \
  --restart unless-stopped \
  --env-file .env \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/deploy/config:/etc/aetherrelay" \
  -v "$PWD/deploy/data:/var/lib/aetherrelay" \
  ghcr.io/muidea/aetherrelay:1.2.3
```

容器内程序最终以 UID/GID `10001`（`aetherrelay`）运行。入口程序只在启动时以 root 初始化映射到 `/var/lib/aetherrelay` 的数据目录所有权，随后立即降权；它不会修改主机挂载的配置目录，因此配置目录仍需在宿主机上提前设置权限。

## 升级与回滚

升级时保留同一宿主机配置目录和数据目录即可：

```bash
docker compose pull
docker compose up -d
docker image inspect ghcr.io/muidea/aetherrelay:latest --format '{{index .RepoDigests 0}}'
```

二进制部署升级流程：

1. 按[备份与维护](operations.md#备份与维护)停止写入并备份 `state.database` 与整个 `state.dir`。
2. 替换二进制（或解压新发布包到安装目录），重启进程/服务。
3. 启动后检查 `healthz`、`/v1/models` 与日志中的配置校验结果。

跨大版本升级或迁移宿主机前，必须先停止写入并完整备份 `deploy/config/` 与 `deploy/data/`。**不要并发运行两个实例指向同一个数据目录**；DuckDB 用量与账号状态不允许多实例共享。

## 常见问题

| 现象 | 处理 |
| --- | --- |
| 启动即失败，提示配置校验错误 | `model_metadata` ID 与容量、状态目录及启动期开关必须合法；Provider 字段在管理页保存时独立校验 |
| 所有数据请求返回 401 | DuckDB 尚未创建 Key，或 Key 缺失/未知/禁用；Key 是必需的应用层认证 |
| 容器内 `/admin` 打不开 | Docker 转发连接在容器内不是 loopback；必须开启 `admin_auth_enabled` 登录保护 |
| 管理页提示配置不可写 | 挂载的配置目录需要 UID `10001` 可写（`chown -R 10001:10001`）；这只影响 YAML 管理项，不影响由 DuckDB 承载的 Provider 目录 |
| Provider 管理提示安全存储不可写 | 注入合法的 `AETHERRELAY_CREDENTIAL_KEY`，并确保 `state.database` 所在数据目录可写 |
| 端口被占用 | 修改 `server.listen_addr`（或 `AETHERRELAY_PORT` 仅替换端口） |
| 容器升级后数据还在吗 | 宿主机 `deploy/data/` 持久化；重建容器不清除。前提是仍映射同一目录且不与其他实例共享 |
| 需要远程访问 /metrics | `metrics_remote_access: true`，并设置 `metrics_allowed_cidrs` 限制采集端来源 |
| 修改账号定时刷新间隔后不生效 | 刷新定时器在启动期创建，修改后必须重启进程 |

更多运行期问题排查见[运维与发布](operations.md)。
