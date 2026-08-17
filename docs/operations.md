# 运行、观测与发布

## 启动与客户端接入

```bash
make run
AETHERRELAY_CONFIG=/etc/aetherrelay/config.yaml make run
```

默认服务地址为 `127.0.0.1:8080`。客户端使用标准入站地址：

```text
OpenAI API base:    http://127.0.0.1:8080/v1
Anthropic API base: http://127.0.0.1:8080
```

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/metrics
curl http://127.0.0.1:8080/stats
```

`/metrics` 与 `/stats` 默认仅允许 loopback。若启用远程访问，应同时设置 `metrics_allowed_cidrs` 限制采集端来源。

## 容器部署

容器部署的完整步骤（Docker Compose 与直接运行镜像、目录权限、Admin 登录、数据持久化、升级与回滚）见[安装与部署](deployment.md#容器部署)。此处仅列出运维要点：

- 镜像发布到 GitHub Container Registry：`ghcr.io/muidea/aetherrelay`。`main` 成功构建后更新 `latest` 与 `main`，发布 `vX.Y.Z` tag 后会推送对应的 `X.Y.Z`、`X.Y` 与 Git SHA 标签；生产部署应固定到完整版本或 SHA，不要仅依赖 `latest`。每个标签同时提供 Linux `amd64` 与 `arm64` 镜像。
- 容器内程序最终以 UID/GID `10001`（`aetherrelay`）运行。入口程序只在启动时以 root 初始化 `/var/lib/aetherrelay` 这个持久化数据目录的所有权，随后立即降权；它不会修改主机挂载的配置目录。
- 宿主机 `deploy/data/` 保存 DuckDB、图片、缩略图与交互归档；DuckDB 内的 Provider、ChatGPT Web 和 Codex OAuth 可恢复凭据均由外部主密钥加密。`deploy/.env` 中的 `AETHERRELAY_CREDENTIAL_KEY` 必须与数据目录一起安全备份，任一丢失都无法恢复凭据。
- 先按[备份与维护](#备份与维护)停止写入并备份，再进行跨大版本升级或迁移宿主机。不要并发运行两个容器指向同一个数据目录。

## Admin 登录安全（可选）

默认 Admin 仅 loopback 可访问。需要远程运维时，启用账号密码登录：

```bash
# 1. 交互式生成哈希（密码不进入 argv / 环境变量 / 日志）
AetherRelay admin password-hash
export AETHERRELAY_ADMIN_PASSWORD_HASH='...'

# 或直接创建/重置 Admin 登录凭据（自动启用 admin_auth_enabled）
AetherRelay admin set-credentials --username ops-admin --config config.yaml

# 2. 配置 server.admin_auth_enabled=true 与账号，或使用环境变量
#    AETHERRELAY_ADMIN_AUTH_ENABLED / AETHERRELAY_ADMIN_USERNAME / AETHERRELAY_ADMIN_PASSWORD_HASH

# 3. 通过 HTTP 或 HTTPS 对外暴露 <admin_base_path>（默认 /admin）
#    生产环境推荐 HTTPS；若要浏览器仅在 HTTPS 携带会话，设置 admin_session_cookie_secure=true。
#    代理应保留外部 Host；应用不信任 X-Forwarded-*。
```

运维注意：

- 启用后任意来源都必须登录；不再保留 loopback 特权旁路。
- 修改密码哈希、账号或开关并成功热更新后，全部内存会话立即失效。
- 管理接口成功修改 Provider 后，旧 transport 产生的健康样本和熔断会在 PATCH 返回前同步清除；恢复上游后无需等待原 30 秒 cooldown。未修改 Provider 的普通配置热更新不会重置其健康状态。
- 客户端 Key 的 Provider 范围修改在 Admin 临界区内按“准备认证索引 → Store 事务 → 原子激活”执行。Provider 被 `selected` Key 引用时删除返回 409 并列出 Key ID；先在“客户端 Key”中编辑权限。`all` Key 不形成删除引用。
- `/v1/models` 缺少预期模型时，先用同一客户端 Key复查目录，再在 Admin 查看该 Key 的有效 Provider、不可用 Provider 和有效模型。目录不按瞬时熔断过滤；目录存在但调用返回 503 时再排查 Provider 健康度，目录中完全不可见则排查 Key 绑定、Provider 启停和账号模型发现。
- `admin_base_path` 是启动期路由；变更后必须重启进程，并同步反向代理路径规则。
- 连续 5 次登录失败会按对端 IP 锁定 15 分钟（不信任 forwarded IP）。
- `AETHERRELAY_CREDENTIAL_KEY`、客户端 Key 哈希、Admin 密码哈希与 DuckDB 文件仍需主机权限保护；不要把主密钥写入 `config.yaml`、数据库、日志或版本库。

设计细节见[安全与认证设计](design/security.md#admin-登录可选)。

## ChatGPT Web 用量统计

ChatGPT Web 相关调用写入与标准代理相同的 DuckDB 用量权威（`aetherrelayusage` / 使用统计页）：

| 路径 | `provider` | `api_key_id` | token |
| --- | --- | --- | --- |
| 代理 `/v1/chat/completions` → chatgptweb | `chatgptweb` | 客户端 Key ID | 本地估计，`estimated=true` |
| 代理受限 `/v1/responses` → chatgptweb | `chatgptweb` | 客户端 Key ID | 本地估计，`estimated=true` |
| 代理 `/v1/images/*` → chatgptweb | `chatgptweb` | 客户端 Key ID | 上游 Usage（有则 `estimated=false`） |
| Admin 工具集（临时对话、搜索、图片代理/任务） | `chatgptweb` | `builtin-local` | 本地估计或上游 Usage；图片任务详情另存任务级用量 |

- 筛选 `provider=chatgptweb` 可查看全部 Web 流量；`builtin-local` 是服务端内建 scope，不接受外部 Header 认证，也不应轮换或删除。临时会话/搜索历史的管理员 owner 仍独立保存。
- 文本 token 为稳定本地估计，不可当作上游账单。
- 本设计落地前，部分 chatgptweb 成功请求可能被误记为 `error`/`proxy_internal_error`（`completePendingUsage` 兜底），历史行不回溯修正。
- Admin 异步图片任务默认不进全局 usage（仍在任务详情展示任务级 Usage）。

## Codex OAuth 用量与账号池

进程始终注入只读内建 Provider `codexoauth`。它与 `chatgptweb` 是两个独立账号域：不共享 refresh token、账号代理、模型发现、网页会话或临时对话。

| 路径 | `provider` | `api_key_id` | token |
| --- | --- | --- | --- |
| 原生代理 `/v1/responses` → codexoauth | `codexoauth` | 客户端 Key ID | 上游 Response `usage`（缺失时本地估算） |

- 使用统计会记录 `upstream_protocol=codexoauth`、`upstream_endpoint=codex_oauth_responses`、`conversion_mode=codex_oauth_responses`，包括 interaction archive 关闭时的兜底结算。
- 每个账号的代理同时用于 OAuth refresh、Codex `/models` 枚举与 Codex Responses 请求。模型快照按账号缓存 6 小时，失败有独立退避；只有发现并仍在有效期内的账号可调度其模型。导入、刷新凭据和完成 OAuth 都会提交一次立即同步；管理员也可在账号页对选中账号或全部账号执行“同步模型”，并轮询其进度。管理 API 与 Web 表格返回稳定本地 ID、邮箱、状态、结果计数、模型缓存、模型冷却、额度观察与最近刷新状态，不返回 token、account ID 或代理。
- 401 触发单飞 refresh 后只重试一次；429 会记录模型级冷却并切换尚未尝试的账号；上游已开始 SSE 输出后不切换账号，避免重复或拼接两个不同响应。若上游明确返回 `usage_limit_reached`，账号表会记录该模型“额度耗尽”及上游提供的恢复时间；这只是运行期观察，不能当作官方剩余额度。
- `/v1/responses` 的非流式请求在内部要求上游 SSE，并在 `response.completed` 或合法的 `response.incomplete` 终态返回原始 Response 对象；上游若返回原生 JSON Response 也会接受。请求中的 `reasoning.effort` 按模型元数据枚举校验，允许值以 `/v1/models` 的 `capabilities.reasoning.efforts` 为准，不支持时返回 400。Responses WebSocket 使用同一路径的 GET upgrade，`/v1/responses/compact` 提供 unary JSON 及最小 SSE 投影；Realtime、网页会话和插件仍不属于 Codex OAuth 能力。
- `/v1/responses/compact` 的上游实际走原生 remote compaction v2 `/responses`。若某账号返回 2xx 但没有 compaction item，该账号会被标记为 native compact 不支持；升级后旧 unary 端点留下的支持/不支持缓存会自动清空并重新学习。
- 指纹收敛是逐账号显式 opt-in，默认 `off`。只有确有共享账号身份收敛需求时才选择 `device/session/full`；排查额度或设备识别异常时先恢复 `off` 做对照。Turn-State 仅保存哈希来源，不应把其 opaque 原值加入日志或工单。
- 账号定时刷新间隔是启动期设置，修改后需重启；账号池本身始终装配。

## ChatGPT Web 内建 Provider

进程始终注入只读内建 Provider `chatgptweb`（不写 YAML）。模型来自账号池发现结果；运维入口是 ChatGPT Web 账号池，而不是 Provider 编辑表单。`config.yaml` 不声明任何 Provider。

`chatgptweb` 的公开 `POST /v1/chat/completions` 支持纯文本 `messages[].content`，以及 OpenAI content-part 数组中的 `text` 与 `image_url`。`image_url.url` 仅接受 PNG、JPEG、GIF、WebP 的 Base64 data URI；每个请求最多 4 张、合计不超过 20 MiB，且单图像素不得超过 4000 万。代理不会下载远程 URL，因此不会为该字段打开 SSRF 通道。图片仅可用于 `user` 消息；`input_audio`、`file`、工具调用和其他未列出的 content part 会返回 `invalid_request`。

公开 `POST /v1/responses` 是同一文本执行器的无状态受限投影：支持字符串或 message-array `input`、`instructions`、`reasoning.effort`、`input_text` 和 data-URI `input_image`，以及基础 buffered/SSE `output`、`usage`。它不会保存 Responses 会话，也不支持 tools/function calling、JSON Schema、`previous_response_id`、后台/realtime、远程图片 URL 或 file ID。常用采样、追踪和存储字段可兼容忽略，并在 interaction metadata 的 `ignored_features` 中可审计；会改变语义的字段会在访问账号和上游前返回 `conversion_unsupported`。

图片请求的账号结果按模型独立记录。`rate_limit`、TLS、超时或上游故障会生成 60 秒生图冷却；`invalid_token` 会先触发一次 OAuth 刷新，只有尚未创建 ChatGPT conversation 时才重投一次，已有 conversation 永不盲重投。管理页只读展示文本/生图冷却和刷新状态。interaction archive 对 data URI 与 `b64_json` 始终只存 MIME、字节数和 SHA-256 摘要，不存图像字节。

## ChatGPT Web 管理页运维注意

Admin 管理台一级页签「ChatGPT Web」提供账号池、临时对话、图片任务与图片库操作入口，调用既有 `/api/chatgpt/**` 管理 API。页面与 API 共用 Admin 会话、CSRF 与 `X-AetherRelay-Admin` 写保护。

### 临时对话正文保留与删除

- 临时对话的会话、消息、图片附件与上游续聊锚点持久化在 `state.database`（DuckDB）专用表中，不进入 interaction archive，也不写入浏览器 localStorage/sessionStorage。图片正文不会嵌入会话 JSON；页面经同源、管理员鉴权且 `Cache-Control: no-store` 的附件端点预览。
- 保留期由 `chatgpt_web.temporary_chat.retention_days` 控制（默认 30 天）。清理任务只删除已到期且没有活跃流的会话；管理员在页面删除为永久删除，不进回收站。
- 临时对话编辑器可在一轮中附加 PNG、JPEG、GIF、WebP 图片（最多 4 张、合计 20 MiB），可发送纯图片消息；附件会随会话删除或到期清理。图片字节按现有文本估算策略不单独计入本地 token 估算。
- 达到 `max_conversations` 时拒绝新建，需要先删除旧记录，系统不会为腾地方静默删历史。
- 服务重启时，任何 `streaming` 消息会被标记为 `interrupted`，会话进入 `recovery_required`：历史仍可读，但不得在原上游分支继续发送；需新建会话。
- 备份/恢复 `state.database` 即包含临时对话正文；共享或外发该文件等同于泄露管理员调试输入输出，需按主机权限与备份策略保护。
- research / deep_research 专用模型不会进入临时对话模型选择器或公开 `/v1/models`。

- **账号导入/导出**：ChatGPT Web 与 Codex OAuth 均可直接选择导出的 JSON 文件重新导入，也支持粘贴单条凭据对象 `{...}`、对象数组 `[...]` 或 `{accounts:[...]}` 包装；ChatGPT Web 另支持纯 access token 文本。文件和粘贴内容不能同时使用。可一次选择最多 20 个 JSON 文件并合并为一次请求；ChatGPT Web 遇到无效文件时不提交，Codex 会整份忽略无法读取、解析、缺少必要凭据或凭据类型错误的文件，并继续批量导入其余有效文件。整个文件选择集和合并请求各限制 1 MiB，整个批次最多 1000 个有效账号，提交或关闭后页面会清空输入。两个导出接口是仅有的明文凭据出口，必须二次确认且响应带 `Cache-Control: no-store`。不要把导出内容写入日志、工单、浏览器 localStorage/sessionStorage 或截图，下载后立即销毁本地副本。
- **OAuth 导入**：授权 URL、callback 与 session id 只应停留在管理员当前浏览器会话的内存中；不要把它们写进 URL 书签、共享剪贴板记录或监控日志。
- **图片删除**：图片库删除不可恢复；批量删除前确认路径列表。图片内容通过 Admin 鉴权同源端点 `GET .../api/chatgpt/images/content?path=` 读取（可选 `thumb=1`），路径经严格校验，不提供通用 `/files/**`。
- **api_key_id**：图片任务和图片库缺省使用服务端内建 `builtin-local` scope；显式值必须是已存在的客户端 Key。Admin 页面从客户端 Key 选择器提交，不接受任意 owner 字符串；图片资产、缩略图、标签和任务不可跨 Key 读取。
- **尺寸与 SVG 排查**：ChatGPT Web 的 conversation 请求没有 OpenAI Images API 的 `size` / `response_format` 原生字段。旧版本把尺寸追加到 prompt，所以上游可能返回任意像素；现在只有明确的 `WIDTHxHEIGHT` 才触发本地中心裁切/双线性缩放，`auto` 则保留上游尺寸。上游返回的是 raster bytes，服务端会下载认证 URL、验证格式并记录实际宽高；拿不到/无法解码 bytes 会失败闭合。SVG 容器包裹 raster 仍不是矢量，故 `svg`/vector 文件请求明确返回不支持，不会伪造 SVG。
- **失败处理**：失败任务已有 `conversation_id` 时，可使用“恢复轮询”继续读取同一上游任务；该操作不会重新提交生成，适用于轮询超时及历史版本误记为 `"<nil>"` 的记录。`bootstrap` 阶段的 TLS/超时失败尚未建立上游会话，页面会有限退避重试一次；仍失败时显示“重新提交”，以原任务参数重新发起。其它失败不提供盲目重试，避免重复生成或重复扣除额度。
- **取消与清理**：排队或运行中的任务可从操作列取消。取消会先持久化 `cancelled` 终态，再取消 AetherRelay 内部等待上下文，因此迟到的成功或失败结果不会覆盖取消状态；上游已受理的请求仍可能继续并产生额度消耗。成功、失败和已取消等终态记录可删除，删除任务记录不会联动删除图片库资产。所有状态均可从“查看”打开完整任务参数、进度、错误、用量和结果。
- 账号池组件始终装配；若管理 API 返回 `503`，应检查模块启动错误和 DuckDB/主密钥状态，而不是通过配置开关启用。

设计与页面合同见[ChatGPT Web 能力设计](design/chatgpt-web.md)。

## 指标与统计

Prometheus 指标均以 `aetherrelay_` 为前缀：

- `aetherrelay_requests_total{provider,model,route,status,outcome}`：请求完成数。
- `aetherrelay_request_duration_seconds_{sum,count}`：请求耗时。
- `aetherrelay_input_tokens_total`、`aetherrelay_output_tokens_total`、缓存 Token 与命中率：Provider/模型维度 Token 数据。
- `aetherrelay_client_requests_total{api_key_id}` 与 `aetherrelay_client_*_tokens_total{api_key_id}`：客户端 Key 维度累计数据。
- `aetherrelay_usage_store_*`：DuckDB 写入、查询、恢复、checkpoint 与健康状态。

`/stats` 返回进程统计、延迟分位数、缓存、上游错误与 all-time `usage` 视图。DuckDB 是用量最终 authority；Prometheus 与 `/stats` 的 Key 累计镜像在启动时由 DuckDB 初始化，并在成功结算请求后更新。

请求 outcome 用于表示流式首包写出后的真实结束态：

| outcome | 含义 |
| --- | --- |
| `success` | 正常完成。 |
| `client_canceled` | 客户端取消。 |
| `idle_timeout` | SSE 空闲超时。 |
| `limit_exceeded` | 本地体或流限制。 |
| `upstream_truncated`、`upstream_failed` | 上游中断或显式失败。 |
| `endpoint_drift` | Provider 声明的直连端点或模型能力与上游响应不一致。 |
| `incomplete` | 上游未完成。 |
| `client_write`、`protocol`、`conversion`、`error` | 客户端写入、协议、转换或其它错误。 |

统计查询、筛选与导出以管理页和当前实现为准；持久化工作区配置见[配置参考](configuration.md#统一状态工作区)。

## SLO webhook

配置 `slo_violation_webhook` 后，服务只在 SLO 状态变化时异步 POST `entered` / `resolved` 事件。事件带有 `instance_id`、递增 `seq`、`generation` 与稳定 `event_id`。

- 消费方应按 `event_id` 幂等，且只在同一 `instance_id` 内比较 `seq`。
- 投递为有界队列与单 worker；网络、408、425、429、5xx 最多重试三次，429 优先遵循 `Retry-After`。
- shutdown 会取消在途投递，并将剩余队列计入 `aetherrelay_slo_webhook_dropped_total`。

相关指标：`aetherrelay_slo_webhook_dropped_total`、`aetherrelay_slo_webhook_queue_length`、`aetherrelay_slo_webhook_requests_total{result}`。

## 用量与归档

每个已接受请求会先写入 DuckDB `started` 事件，随后结算为 `completed`。管理页可按时间、API Key、Provider、Model、Outcome 与估算标记筛选查看用量。

旧 `usage.csv` 只可显式一次性导入：

```bash
go run ./cmd/aetherrelay-usage-import \
  -source usage.csv \
  -database var/aetherrelay.duckdb \
  -api-key-id default
```

将示例中的 `var/aetherrelay.duckdb` 替换为实际的 `state.database` 完整路径。交互归档位于 `state.dir/interactions/{api_key_id}/{round_id}/`，包含脱敏请求元数据、上游请求/响应摘要、客户端响应与 `metadata.json`。`archive_full_content: false` 可禁止请求与响应正文落盘。归档中的敏感 Header 会脱敏，原始客户端/Provider Key 不会写入。

## 备份与维护

不要直接复制正在写入的 DuckDB 文件。建议流程：停止接收新请求、等待当前写入完成、执行 checkpoint、复制 `state.database`，并将需要保留的 `state.dir/interactions/`、`state.dir/images/` 与 `state.dir/image_thumbnails/` 一并复制，随后恢复服务。数据库与整个 `state.dir` 必须由同一个实例独占。

## Provider live probe

Probe 不会在服务启动时运行，可用于验证某个已配置 Provider 的 direct endpoint：

```bash
go run ./cmd/aetherrelay-probe -config config.yaml \
  -provider <route-owner> -endpoint chat_completions -model <exact-model-id>
```

输出会脱敏，结论为 `success`、`credential_issue`、`endpoint_drift` 或 `environment_undetermined`。带日期的现场审计仅保留当时证据，不能替代对当前配置的重新探测。

Admin 的 Provider 页面还会显示配置启用状态之外的运行期可用性，并提供“检查”按钮。该按钮只对当前
Provider 执行一次最小非流式探测，记录结果但不会改写配置。状态含义如下：

| 状态 | 含义 |
| --- | --- |
| `disabled` | 配置已禁用。 |
| `unknown` | 尚无请求或手动检查结果。 |
| `healthy` | 最近一次记录为成功。 |
| `degraded` | 存在失败，但连续失败少于三次。 |
| `unavailable` | 连续失败至少三次。 |
| `credential_error` | 最近失败为 401 或 403。 |
| `endpoint_drift` | 最近探测表明端点或模型能力与上游不一致。 |

Provider 表的“来源”仅为展示分类：运行时内建 Provider 显示 `builtin`，官方 Base URL 显示 `official`，其余显示 `third_party`。该值不写入 YAML，也不参与路由或安全判断。

## 构建与发布

```bash
make check
make build
make release-package VERSION=v1.2.3
make release VERSION=v1.2.3
```

普通提交 CI 只执行 Linux amd64 的格式、依赖、vet、全量测试与构建。推送 `vX.Y.Z` tag 后，Release workflow 会统一验证源码一次，并在 Linux amd64/arm64、macOS arm64 原生 runner 上打包 `.tar.gz` 与 SHA-256 文件，然后创建 GitHub Release。Windows 不发布原生二进制（依赖 Unix termios，需 MinGW CGO 工具链，从未验证）；Windows 用户用 WSL2 或容器部署。

不要从 amd64 强制交叉编译 Linux arm64：DuckDB Go bindings 需要相应的原生目标 runner。手动重跑 Release workflow 时，输入的版本必须是已有 tag。
