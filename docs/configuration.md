# 配置参考

本文描述当前 `ai-proxy` 的运行配置。完整可复制示例见仓库根目录的 [`config.example.yaml`](../config.example.yaml)。配置路径默认是 `config.yaml`，也可用 `-config` 或 `AI_PROXY_CONFIG` 指定。

配置值支持 `${ENV}` 展开，例如 `api_key: ${OPENAI_API_KEY}`；环境变量只能填充值，不能创建 Provider、模型或路由。

## 最小配置

```yaml
server:
  listen_addr: 127.0.0.1:8080

state:
  dir: var
  database: state.duckdb

providers:
  openai:
    enabled: true
    protocol: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
    endpoints: chat_completions
    models: gpt-5.5

model_metadata:
  gpt-5.5:
    context_window_tokens: 128000
    max_output_tokens: 16384
```

`model_metadata` 是可选的静态模型元数据目录；模型 ID exact 且严格区分大小写。Provider 是否能处理某类请求由 `endpoints` 与共享 transport matrix 唯一决定：

- enabled Provider 的精确 `models` 条目会自动进入运行时模型目录，无需在 `model_metadata` 重复登记。Embedding 模型通常只需配置在 Provider 中。
- `*`、`prefix-*` 等 pattern 只能参与匹配，不能枚举具体模型 ID；具体 ID 必须来自某个 enabled Provider 的精确 `models` 条目或账号池发现。
- metadata 条目不会让模型进入 `/v1/models`，不会建立路由，也不要求当前存在匹配模型；模型以后被配置或发现时会自动获得对应 metadata。
- `context_window_tokens` 与 `max_output_tokens` 都是可选元数据；省略或为 `0` 表示未知或不适用。两者都显式大于 `0` 时，`max_output_tokens` 必须小于 `context_window_tokens`。

| 字段 | 层级 | 枚举 |
| --- | --- | --- |
| `providers.*.endpoints` | Provider 原生端点 | `chat_completions`、`messages`、`responses`、`completions`、`embeddings`、`images` |

每个模型会解析为有序 Provider 候选链，而不是唯一 RouteOwner。启用 ChatGPT Web 或 Codex OAuth 后，运行时有效目录还会合成各自的内建模型并参与同一候选链。端点矩阵、转换限制与 typed error 以当前代码和自动化测试为准；设计背景见[核心代理与路由设计](design/proxy-core.md)。

## Provider 与模型路由

每个 enabled Provider 必须显式设置：

- `protocol`：仅 `openai` 或 `anthropic`。`chatgptweb` 与 `codexoauth` 是保留协议/ID，禁止写入 `providers`；分别由 `chatgpt_web` 与 `codex_oauth` 在运行时注入内建 Provider。
- `base_url`：可带或不带 `/v1`，代理会避免重复拼接。
- `endpoints`：上游原生支持的端点，不能由 protocol 自动推断。它应用于该 Provider 匹配的全部模型；若不同模型的端点集合不同，应拆分 Provider 条目及其 `models` pattern。
- `models`：精确条目会发布可路由模型；pattern 只匹配由其他精确条目或账号池发现提供的具体模型 ID。多个 enabled Provider 可以匹配同一 exact model。
- `priority`：可选整数，范围 `-1000`~`1000`，默认 `100`；数值越高越先被选择，名称只用于同优先级稳定排序。显式 `0` 有效。
- `fallback`：可选布尔值，默认 `true`；Provider 位于非首候选时，是否允许在安全条件下作为回退目标。
- `api_key`：远程 Provider 必填；仅 loopback 上游可显式 `allow_unauthenticated: true`。

每个实际模型按所有 enabled Provider 的 `models` 规则生成候选，并按 `priority` 降序排序；metadata 不参与这一步。请求到达后，再按入站 path 从候选的 `endpoints` 和共享 transport matrix 中筛选可服务 Provider。

回退只会发生在客户端响应尚未提交时，并且仅针对网络错误、`408`、`429`、`5xx` 或流式请求的首事件探测失败。一次已写出的 SSE/HTTP 响应绝不切换 Provider。转换候选会先做语义预检：不支持的 tools、多模态或结构化字段不会被静默删改；若后续存在可原生保留该语义的候选，可改用该候选，否则返回 `conversion_unsupported`。图片任务一旦提交不回退，避免重复创建。

运行时健康度是 5 分钟的有界样本窗口：少于 3 个样本显示 `unknown`；连续 3 次可重试失败会打开 30 秒熔断，路由会跳过熔断、`unhealthy` 或 `credential_error` 候选。健康度不替代账号池的真实可用性判断。管理页将可用性与健康度合并为单一状态视图，显示样本量、成功率、P95、熔断和账号池可路由原因。

## 客户端 API Key

`client_api_keys` 是调用方身份与用量归属的唯一配置 authority：

```yaml
client_api_keys:
  codex:
    api_key: ${CODEX_API_KEY}
    enabled: true
  ci-agent:
    # 由本地 Admin 管理端创建的 Key 只保存 SHA-256 摘要。
    api_key_hash: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    enabled: true
  batch:
    api_key: ${BATCH_API_KEY}
    enabled: false
```

- Key ID 需匹配 `[a-z0-9][a-z0-9._-]{0,63}`，`default` 为历史用量保留 ID，不能配置。
- 每个数据请求必须携带 Key；缺失、空 Header、未知、禁用、格式错误或两个身份 Header 冲突时均返回 401，且不产生用量记录。
- OpenAI 使用 `Authorization: Bearer <key>`，Anthropic 使用 `X-API-Key: <key>`；两种 Header 可兼容，但同时出现时必须为同一 Key。
- 原始客户端 Key 不写入日志、DuckDB、归档或管理 API，也不会转发给上游。
- Admin 可创建、启停、轮换或删除客户端 Key。创建和轮换仅在成功响应中显示一次明文；托管 Key 的 YAML 使用 `api_key_hash`，不能与 `api_key` 同时配置。
- `inbound_api_key`、`AI_PROXY_INBOUND_API_KEY`、`usage_file` 与 `AI_PROXY_USAGE_FILE` 已删除，配置中出现会启动失败。

客户端 Key 是必需的应用层认证；若监听 `0.0.0.0:8080` 或 `:8080`，仍应在防火墙、反向代理或私有网络层实施额外访问控制。

## Server 配置

| 配置或环境变量 | 说明 |
| --- | --- |
| `server.listen_addr` / `AI_PROXY_LISTEN_ADDR` | 完整监听地址，默认 `127.0.0.1:8080`。 |
| `AI_PROXY_PORT` | 仅替换端口，生成 `127.0.0.1:<port>`。 |
| `max_request_body_bytes` / `AI_PROXY_MAX_REQUEST_BODY_BYTES` | 客户端请求体上限。 |
| `max_upstream_response_bytes` / `AI_PROXY_MAX_UPSTREAM_RESPONSE_BYTES` | 非流式上游响应上限。 |
| `max_stream_bytes`、`max_sse_line_bytes` | 流式累计输出与单条 SSE 行上限。 |
| `request_timeout_seconds` | 非流式总超时及流式等待响应头超时。 |
| `stream_idle_timeout_seconds` | 连续未收到 SSE 数据的超时；`0` 禁用。 |
| `archive_full_content` | 是否落盘完整请求/响应正文。 |
| `debug_log`、`log_format` | 调试日志和 `json`/`text` 格式。 |
| `metrics_remote_access`、`metrics_allowed_cidrs` | `/metrics`、`/stats` 的远程访问控制。 |
| `admin_auth_enabled` / `AI_PROXY_ADMIN_AUTH_ENABLED` | Admin 登录开关，默认 `false`（保持 loopback-only）。 |
| `admin_base_path` / `AI_PROXY_ADMIN_BASE_PATH` | Admin 页面与 API 前缀，默认 `/admin`；启动期路由，变更需重启。 |
| `admin_default_language` | Admin Web 的实例默认语言，仅 `zh-CN` 或 `en-US`，默认 `zh-CN`；可在管理页热更新。 |
| `admin_username` / `AI_PROXY_ADMIN_USERNAME` | 单管理员账号（开启认证时必填，区分大小写）。 |
| `admin_password_hash` / `AI_PROXY_ADMIN_PASSWORD_HASH` | Argon2id PHC 哈希（开启认证时必填；禁止明文）。 |
| `admin_session_cookie_secure` / `AI_PROXY_ADMIN_SESSION_COOKIE_SECURE` | 会话 Cookie 是否仅随 HTTPS 请求发送，默认 `false`。 |
| `admin_session_ttl_seconds` / `AI_PROXY_ADMIN_SESSION_TTL_SECONDS` | 会话绝对有效期，默认 `28800`（8h），范围 `300~86400`。 |

环境变量与配置键的完整默认值、上限和校验以 [`config.example.yaml`](../config.example.yaml) 与 `internal/pkg/aiproxyconfig` 为准。

## 模型流式 SSE

文本生成统一通过 SSE 增量输出。标准推理端点仅在 JSON 请求体包含 `"stream": true` 时进入流式生命周期；`Accept: text/event-stream` 不能单独开启流式响应。

- `/v1/chat/completions` 返回 OpenAI Chat Completions SSE，必要时转换 Anthropic 上游事件。
- `/v1/messages` 返回 Anthropic Messages SSE，必要时转换 OpenAI 上游事件。
- `/v1/responses` 支持 OpenAI 协议 Provider 的原生 Responses；内建 `chatgptweb` 额外提供无状态受限投影：基础文本、data-URI 图片输入、基础 SSE/output/usage。唯一工具例外是单个 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11`：它启动一次隔离的 ChatGPT Web 强制搜索会话，返回 `web_search_call`、来源和 `url_citation`。它不支持 function calling、混合工具、JSON Schema、`previous_response_id`、后台/realtime、远程图片 URL 或 file ID。
- `/v1/search` 是 ai-proxy 的非流式扩展端点，不是 OpenAI 官方端点别名。请求体仅接受 `model` 与纯文本 `query`；响应为 `search.result`，含 `output_text`、`sources` 与估算 `usage`。它只选择内建 `chatgptweb` 的已发现模型，静态 Provider 即使有同名模型或更高优先级也不会接收该请求；无可用 ChatGPT Web 搜索能力时返回明确错误，不降级为普通文本生成。
- 内建 `codexoauth` 只服务原生 `POST /v1/responses`：请求与 SSE 事件不经过 ChatGPT Web 消息树转换，非流式结果从上游 `response.completed` 提取原始 Response 对象。P0 不支持 WebSocket/realtime、`/responses/compact` 或网页会话/插件能力；`/v1/chat/completions` 不能路由到该 Provider。
- 跨协议转换只保证基础文本，tools、thinking、多模态等未支持能力在访问上游前拒绝。

浏览器客户端应使用 `fetch()` + `ReadableStream` 发送 POST 请求和认证 Header，不使用只支持 GET 语义的原生 `EventSource`。完整合同见[核心代理与路由设计](design/proxy-core.md#统一流式-sse)。

## 统一状态工作区

```yaml
state:
  dir: var
  database: state.duckdb
  memory_limit: 256MB
  threads: 2
  query_cache_seconds: 15
  interaction_retention: 500
```

`state.dir` 是单实例唯一的持久化工作区，相对路径按 `config.yaml` 所在目录解析。`state.database` 必须是该目录下的本地 DuckDB 文件；它是用量、ChatGPT 账号、图片任务、图片索引和标签的唯一结构化状态 authority。多个实例不得共享同一个工作区。数据库不可打开、不可迁移或资源参数不一致时，启用对应能力的模块会在启动期失败，不会降级为空状态运行。

`state.database` 的业务表按 owner 划分：用量 owner 管理其用量表；ChatGPT 账号池管理 `chatgpt_accounts`；Codex OAuth 账号池管理 `codex_oauth_accounts`；图片任务管理 `chatgpt_image_tasks`；图片库管理 `chatgpt_images` 与 `chatgpt_image_tags`；Proxy 的 Admin 在线搜索管理 `chatgpt_web_search_history`。不使用通用 JSON 文档表。图片元数据和搜索来源保留 JSON 扩展列，但账号、任务、图片和搜索历史的归属主键及查询字段均为独立列。

Admin「功能集 → 在线搜索」仅将成功结果保存到该历史表。历史以登录管理员用户名隔离；未启用 Admin 登录时使用稳定的本地 `admin` 作用域。每个作用域最多保留 200 条，自动清理 30 天前的记录；答案、查询和来源始终只保存在服务器 DuckDB，不写入浏览器存储。`POST /v1/search` 及协议内的单次搜索保持无状态，不会创建这些历史记录。

工作区固定包含 `interactions/`、`images/`、`image_thumbnails/` 与 DuckDB 文件。原始图片仍保存在文件系统，数据库只保存其元数据与索引；交互归档目录固定为 `interactions/`。整个目录应由运行用户以私有权限持有，且不得提交到版本库。

## ChatGPT Web 本地数据

```yaml
chatgpt_web:
  enabled: false
  provider_enabled: false
  priority: 10
  refresh_account_interval_minute: 0
  temporary_chat:
    enabled: true
    retention_days: 30
    max_conversations: 2000
    max_messages_per_conversation: 200
    max_message_bytes: 262144
    turn_timeout_seconds: 300
```

- 默认关闭。`enabled: true` 后，ChatGPT Web 账号池、上游、图片存储、异步图片任务与 Admin 临时文本对话会一并装配。
- `provider_enabled` 只控制已装配的内建 Provider 是否参与模型路由；省略时跟随 `enabled`，可通过 Provider 管理页热更新。它不会停止账号刷新、图片任务或临时对话。
- `priority` 是内建 Provider 的候选优先级（`-1000` 到 `1000`，默认 `10`），可通过 Provider 管理页热更新；ChatGPT Web 不作为回退候选。
- 账号、任务、图片索引和标签保存于 `state.database`；不得写入 YAML、环境变量、日志或版本库。
- 旧的 `usage_store`、`chatgpt_web.data_dir`、`interaction_dir` 及相应环境变量均不再支持；所有本地路径和 DuckDB 资源参数只能在 `state` 中声明。
- `refresh_account_interval_minute: 0` 关闭周期刷新；正数为刷新间隔（分钟）。它不触发密码重登。
- `temporary_chat` 控制 Admin「临时对话」：
  - `enabled` 默认 `true`（在 `chatgpt_web.enabled` 为真时生效）；显式 `false` 可关闭。
  - `retention_days` 必须为正；过期且无活跃流的会话会被清理。管理员删除不进入回收站。
  - `max_conversations` 达到上限时拒绝新建并提示先删旧记录，不会静默删除历史。
  - `max_messages_per_conversation` / `max_message_bytes` / `turn_timeout_seconds` 均为正数上限。
  - 会话正文只写入 `state.database` 的专用表；浏览器不得使用 localStorage/sessionStorage 保存消息或上游锚点。
- 启用后自动注入固定 ID 为 `chatgptweb` 的内建 Provider（不持久化到 YAML）。公开模型目录来自账号池对 ChatGPT Web `/backend-api/models` 的模型 ID 枚举并集；上游发现的模型特征只供账号池内部执行选择，不进入 `model_metadata` 或 `/v1/models`。
- 临时对话会持久化创建时的请求模型；仅当 ChatGPT Web SSE 的 assistant `message.metadata.model_slug` 明确返回时，才记录并展示该轮上游实际模型。模型正文的自述不作为路由证据；上游未返回该字段时管理页显示“上游未返回”。
- 自动发现结果只存在于进程内有效目录，驱动 `/v1/models`、`/v1/chat/completions`、受限 `/v1/responses`、`/v1/search`、`/v1/images/generations` 与 `/v1/images/edits`。不得在 `providers` 中手工声明 `chatgptweb` 路由；同 ID 的 `model_metadata` 只补充容量。
- 对内建 `chatgptweb`，`/v1/chat/completions` 中唯一的 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11` 工具（或 `web_search_options`）会启动一次强制搜索；仅使用最后一条纯文本 user 消息作为 query。图片、文件、function/tool 调用、结构化输出和工具循环会在访问上游前返回 `conversion_unsupported`。非流式结果含来源和 `url_citation`；流式是在上游搜索完成后一次性发送完整 delta 的兼容 SSE，不是增量搜索流。
- Admin「临时对话」的“联网搜索”是逐轮开关，且「功能集」提供独立的在线搜索页面；二者都使用 `/v1/search` 的强制 ChatGPT Web 选择规则。启用时不接受图片/文件附件；它不创建持久搜索线程，也不承诺深度研究、网页插件或多轮工具调用。
- 自动模型与静态 Provider 同名时会保留全部候选；静态 Provider 默认优先级为 `100`，Codex OAuth 默认 `90`，ChatGPT Web 默认 `10` 且不作为回退。Provider 表提供内建 Provider 的路由启停与优先级控制，并显示重叠摘要。

## Codex OAuth 本地账号池

```yaml
codex_oauth:
  enabled: false
  provider_enabled: false
  priority: 90
  refresh_account_interval_minute: 0
```

- 每个正常 Codex OAuth 账号会通过带该账号凭据、`ChatGPT-Account-ID` 与账号代理的 `GET /backend-api/codex/models` 自动发现模型；结果以受限投影持久化到账号池，6 小时后过期。失败账号以 30 秒到 5 分钟的指数退避重试，不影响其它账号。
- 导入凭据、刷新凭据或完成 OAuth 后会立即提交模型同步；管理页也可对选中账号或全部账号执行“同步模型”。`POST <admin_base_path>/api/codex/accounts/discovery` 接受可选 `account_ids`，返回 `progress_id`；`GET .../discovery/progress/{progress_id}` 返回进度。任务记录只在当前进程中保留 30 分钟，持久化模型快照才是重启后的权威状态。
- 管理页可按账号读取 `GET /backend-api/wham/usage`，并展示上游观测到的套餐类型、主/次窗口、代码审查和附加窗口的 `used_percent`、恢复时间与限制状态。导入、凭据刷新、OAuth 完成会触发一次账号范围刷新；也可在账号池选中账号后手动刷新。快照有效期为 15 分钟，刷新失败会保留上一份快照并标记错误；不会高频轮询，也不把窗口百分比伪装为 Token 数、请求数或路由可用性。
- 调用中上游明确返回的 `usage_limit_reached` 仍会另行记录为账号/模型级额度耗尽与可选恢复时间，并驱动该模型冷却；普通 429 仍只产生模型冷却。这个运行时观察与管理页的套餐用量窗口相互补充，不能彼此替代。
- 可路由模型始终是全部健康账号模型快照的并集；不提供 `codex_oauth.models` 筛选项。静态 Provider 使用同名模型时，两者都会进入候选链；静态默认优先级为 `100`，Codex OAuth 默认 `90`，可在安全的原生 Responses 失败场景回退。`provider_enabled` 与 `priority` 是可热更新的路由策略；升级前若配置了 `codex_oauth.models`，必须将其移除。
- 账号（access/refresh/id token、ChatGPT account ID、邮箱、到期时间与账号代理）仅写入 `state.database`。管理列表严格脱敏，不返回 token、账号 ID 或代理 URL。
- 账号代理一旦配置，会同时用于 OAuth 授权码换令牌、refresh token 刷新、模型发现、`https://chatgpt.com/backend-api/wham/usage` 和 `https://chatgpt.com/backend-api/codex/responses` 请求，避免刷新、发现、用量读取与实际调用的出口 IP 不一致。
- 上游 `401` 会按本地账号 ID 单飞刷新，然后仅重试一次尚未向客户端写出的请求；刷新永久失败或第二次仍被拒绝时账号标为异常。`429`、超时、网络和上游失败按模型冷却，`Retry-After`（最多 3600 秒）优先。
- `refresh_account_interval_minute: 0` 关闭临期刷新；正数只刷新有可解析到期时间且将在 5 分钟内失效的正常账号。没有到期元数据的导入凭据仍可在实际 `401` 时刷新，不会被定时任务反复触碰。
- `enabled` 与 `refresh_account_interval_minute` 决定 Block 的订阅和定时器，修改后必须重启 ai-proxy。模型快照由运行时定时刷新；首次启用该能力也必须重启。

## 本地管理页

访问 `http://127.0.0.1:8080/admin/`（或自定义 `admin_base_path`）可管理 Provider、客户端 Key、查看 API Key 用量；「账号池」按 ChatGPT Web 与 Codex OAuth 分组，「功能集」提供图片任务、图片库、在线搜索与临时对话。Codex 账号表展示每个账号的模型缓存、发现进度/退避、上游用量窗口/刷新进度、模型冷却与调用中观察到的额度耗尽状态；内建 Provider 会直接显示不可用原因、可路由账号数和模型数。相关管理 API 位于该前缀下的 `/api/chatgpt/**`、`/api/codex/**` 与 `/api/features/**`。

管理页支持简体中文与 English。语言选择优先级为 URL `?lang=zh-CN|en-US`（仅当前访问）> 浏览器语言偏好 Cookie > `server.admin_default_language` > 浏览器语言 > `zh-CN`。页面顶部选择器会保存非敏感的浏览器偏好；“设为默认”通过 `PUT <admin_base_path>/api/admin/preferences` 更新实例默认语言并立即热加载。该设置不影响代理请求、账号池或 OAuth 行为。

Provider 表的“来源”字段仅作展示：运行时内建 Provider 为 `builtin`，官方 Base URL 为 `official`，其余为 `third_party`。它不会写回 YAML，也不影响路由或安全判断。静态 Provider 可编辑优先级与回退开关；内建 Provider 不可删除，但可热更新路由启停与优先级。状态列合并账号池可用性与请求健康度，避免同一 Provider 出现两套相互矛盾的状态。

ChatGPT Web 管理页依赖对应运行时组件（账号池 / 图片任务 / 图片存储）已装配；若组件未启用，页面会显示不可用状态而不是空数据。图片预览通过 Admin 鉴权的同源读取端点 `GET <admin_base_path>/api/chatgpt/images/content` 加载，不暴露通用 `/files/**`。账号导出、OAuth 回调与完整 token 不会写入浏览器持久化存储。

`chatgpt_web.enabled` 在进程启动时决定 ChatGPT Web 的运行组件是否装配。关闭时，`/api/chatgpt/**` 返回 `503 chatgpt web is not enabled`；不能只在运行中修改 YAML 后立即使用，启用或关闭该能力后必须重启 ai-proxy。

`codex_oauth.enabled` 同样在进程启动时装配独立账号池 Block。关闭时，`/api/codex/**` 返回 `503 Codex OAuth is not enabled`；Provider 管理页不会把它当作可编辑的静态 Provider。Codex 管理页支持脱敏列表、批量刷新、删除、JSON 凭据导入与 PKCE OAuth 导入；OAuth callback、token 与代理不会写入浏览器持久化存储。

ChatGPT 账号列表 `GET <base>/api/chatgpt/accounts` 始终脱敏 access token，且不返回账号代理。列表会分别投影仍生效的文本与生图模型冷却（模型、错误类别、恢复时间），以及最近凭据刷新成功时间或失败类别/时间，供管理页只读展示；绝不返回原始 OAuth 错误、Token 或代理。冷却窗口目前固定为 60 秒，不提供 YAML 或 Web 调参。修改、删除、刷新和导出均使用稳定的 `id`，而不是 token：

```text
POST   <base>/api/chatgpt/accounts                 # {"tokens":[...],"source_type":"web"}
PATCH  <base>/api/chatgpt/accounts/{id}            # type/status/quota/proxy 的局部更新
DELETE <base>/api/chatgpt/accounts                 # {"ids":[...]}
POST   <base>/api/chatgpt/accounts/refresh         # {"account_ids":[...]}；空数组表示全部
POST   <base>/api/chatgpt/accounts/export          # {"ids":[...]}；返回凭据，Cache-Control: no-store
```

导出属于刻意的敏感操作，只应在受控的本机或已启用 HTTPS 登录保护的 Admin 会话中调用；不要把响应写入日志、浏览器持久化存储或工单。

### 默认模式（`admin_auth_enabled: false`）

- `<admin_base_path>` 及 API **永远限制 loopback**，即使代理监听在非 loopback 地址。
- 写接口仍要求浏览器意图头 `X-AI-Proxy-Admin: 1`（不是身份凭据）。
- Provider API Key 只显示“已配置”，从不回显明文；保存时保留未修改的已有值或 `${ENV}` 表达式。
- 使用统计页查询 DuckDB，并支持按时间、API Key、Provider、Model、Outcome 和估算标记筛选。

### 安全登录模式（`admin_auth_enabled: true`）

开启后取消 Admin 的 loopback 限制；任意来源都必须先登录并持有有效会话。设计详见[安全与认证设计](design/security.md#admin-登录可选)。

```bash
# 交互式生成 Argon2id 密码哈希（仅 TTY；密码不进参数/日志）
ai-proxy admin password-hash
export AI_PROXY_ADMIN_PASSWORD_HASH='...'

# 或直接创建/重置账号密码：自动写入 admin_auth_enabled、账号与哈希，并要求重启生效。
ai-proxy admin set-credentials --username ops-admin --config config.yaml
```

```yaml
server:
  admin_auth_enabled: true
  admin_base_path: /ops/ai-proxy   # 可选；默认 /admin；变更需重启
  admin_default_language: zh-CN    # 可选；zh-CN 或 en-US，可在管理页热更新
  admin_username: ops-admin
  admin_password_hash: ${AI_PROXY_ADMIN_PASSWORD_HASH}
  admin_session_cookie_secure: true # 可选；开启后仅 HTTPS 可携带会话 Cookie
  admin_session_ttl_seconds: 28800
```

要点：

- 必须配置合法 Argon2id PHC（固定参数 `m=65536,t=3,p=1`）；缺失或非法哈希会使进程在监听前启动失败。
- 会话为进程内内存 Cookie（`HttpOnly` + `SameSite=Strict`）；`admin_session_cookie_secure=true` 时额外带 `Secure`，浏览器仅会在 HTTPS 请求中携带会话。HTTP 仅适用于受信网络，生产环境推荐 HTTPS。代理部署时应保留外部 `Host`；应用不信任 forwarded header。
- 状态变更请求需要会话 Cookie 与 `X-AI-Proxy-CSRF`；未登录 API 返回 JSON `401`，页面 `303` 到 `<basePath>/login`。
- 认证相关配置热更新成功后清空全部会话；`admin_base_path` 变更必须重启。
- 该模式不替代 TLS、主机账户隔离或配置文件权限保护。

Admin usage API 的筛选参数、导出边界与响应格式以当前管理页、`internal/modules/application/adminapi/service/admin` 的合同测试和 DuckDB 查询实现为准。
