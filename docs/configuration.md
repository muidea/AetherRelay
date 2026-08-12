# 配置参考

本文描述当前 `AetherRelay` 的运行配置。完整可复制示例见仓库根目录的 [`config.example.yaml`](../config.example.yaml)。配置路径默认是 `config.yaml`，也可用 `-config` 或 `AETHERRELAY_CONFIG` 指定。

`config.yaml` 只描述进程启动与静态模型元数据。管理页创建的 Provider 和两类账号池凭据使用 `AETHERRELAY_CREDENTIAL_KEY` 加密后写入 `state.database`，不会写回配置文件。

## 最小配置

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
```

`model_metadata` 是可选的静态模型元数据目录；模型 ID exact 且严格区分大小写。Provider 是否能处理某类请求由 `endpoints` 与共享 transport matrix 唯一决定。`GET/POST /v1/models` 会把这套运行时结果以模型级 `supported_endpoints` 返回；该字段不是配置项，也不应写入 `model_metadata`：

- enabled Provider 的精确 `models` 条目会自动进入运行时模型目录，无需在 `model_metadata` 重复登记。Embedding 模型通常只需配置在 Provider 中。
- `*`、`prefix-*` 等 pattern 只能参与匹配，不能枚举具体模型 ID；具体 ID 必须来自某个 enabled Provider 的精确 `models` 条目或账号池发现。
- metadata 条目不会让模型进入 `/v1/models`，不会建立路由，也不要求当前存在匹配模型；模型以后被配置或发现时会自动获得对应 metadata。
- `context_window_tokens` 与 `max_output_tokens` 都是可选元数据；省略或为 `0` 表示未知或不适用。两者都显式大于 `0` 时，`max_output_tokens` 必须小于 `context_window_tokens`。

当前容量元数据：

| Exact model ID | Context window | Max output |
| --- | ---: | ---: |
| `deepseek-v4-flash` / `DeepSeek-V4-Flash` | 1,000,000 | 29,000 |
| `gpt-5.6-luna` / `gpt-5.6-sol` / `gpt-5.6-terra` | 272,000 | 128,000 |
| `gpt-5.4-mini` | 400,000 | 128,000 |
| `gpt-5.4` | 1,050,000 | 128,000 |
| `gpt-5.5` | 272,000 | 128,000 |
| `gpt-5.3-codex-spark` / `gpt-5.3-codex` | 128,000 | 未声明 |
| `codex-auto-review` | 1,050,000 | 128,000 |
| `grok-4.5` | 500,000 | 未声明 |
| `minimax-m3` | 1,050,000 | 131,100 |
| `minimax-m2.7-highspeed` | 204,800 | 131,100 |

表中 `/` 分隔的是大小写敏感的独立 exact ID，不是别名匹配规则。最大输出未声明时 `/v1/models` 省略 `maxOutputTokens`，应用不得自行推断。

| 字段 | 层级 | 枚举 |
| --- | --- | --- |
| `providers.*.endpoints` | Provider 原生端点 | `chat_completions`、`messages`、`responses`、`completions`、`embeddings`、`images` |

每个模型会解析为有序 Provider 候选链，而不是唯一 RouteOwner。启用 ChatGPT Web 或 Codex OAuth 后，运行时有效目录还会合成各自的内建模型并参与同一候选链。端点矩阵、转换限制与 typed error 以当前代码和自动化测试为准；设计背景见[核心代理与路由设计](design/proxy-core.md)。

## Provider 与模型路由

每个由管理页创建的 enabled Provider 必须显式设置：

- `protocol`：仅 `openai` 或 `anthropic`。`chatgptweb` 与 `codexoauth` 是保留协议/ID，不能由管理型 Provider 目录创建；分别由 `chatgpt_web` 与 `codex_oauth` 在运行时注入内建 Provider。
- `base_url`：可带或不带 `/v1`，代理会避免重复拼接。
- `endpoints`：上游原生支持的端点，不能由 protocol 自动推断。它应用于该 Provider 匹配的全部模型；若不同模型的端点集合不同，应拆分 Provider 条目及其 `models` pattern。
- `models`：精确条目会发布可路由模型；pattern 只匹配由其他精确条目或账号池发现提供的具体模型 ID。多个 enabled Provider 可以匹配同一 exact model。
- `priority`：可选整数，范围 `-1000`~`1000`，默认 `100`；数值越高越先被选择，名称只用于同优先级稳定排序。显式 `0` 有效。
- `fallback`：可选布尔值，默认 `true`；Provider 位于非首候选时，是否允许在安全条件下作为回退目标。
- `api_key`：所有 Provider 必填。API Key 只进入加密 Provider 目录，不进入 YAML。

Provider 目录以 DuckDB 为运行期 authority，并通过管理页维护。`config.yaml` 不应声明 Provider，尤其不得保存明文 Key。数据库已有 Provider 密文时，缺少或使用错误的 `AETHERRELAY_CREDENTIAL_KEY` 会使启动失败，不会回退为空目录。

每个实际模型按所有 enabled Provider 的 `models` 规则生成候选；metadata 不参与模型成员资格。请求到达后，再按入站 path 从候选的 `endpoints` 和共享 transport matrix 中筛选可服务 Provider，并依次按语义等级（native/Codex OAuth、ChatGPT Web Responses 投影、跨协议转换）、`priority`、健康分数和名称稳定排序。

回退只会发生在客户端响应尚未提交时，并且仅针对网络错误、`408`、`429`、`5xx` 或流式请求的首事件探测失败。一次已写出的 SSE/HTTP 响应绝不切换 Provider。转换候选会先做语义预检：不支持的 tools、多模态或结构化字段不会被静默删改；若后续存在可原生保留该语义的候选，可改用该候选，否则返回 `conversion_unsupported`。ChatGPT Web、Codex OAuth 和图片等可能创建上游状态的执行器采用更严格的专用回退边界，避免重复执行。native 请求保留安全 query；跨协议转换清空客户端 query。

运行时健康度是 5 分钟的有界样本窗口：少于 3 个样本显示 `unknown`；连续 3 次可重试失败会打开 30 秒熔断，路由会跳过熔断、`unhealthy` 或 `credential_error` 候选。401/403 credential 状态按 Provider + exact model 隔离，避免共享 Provider 中一个未授权模型阻断其他已授权模型；传输失败和 open circuit 仍属于 Provider 整体。Provider 定义新增、删除或实际变更时，热更新会在返回成功前同步清除该 Provider 的旧健康窗口与熔断状态；与 Provider 无关的配置更新不会清除健康状态。健康度不替代账号池的真实可用性判断。管理页将可用性与健康度合并为单一状态视图，显示样本量、成功率、P95、熔断和账号池可路由原因。

## 客户端 API Key

客户端 API Key 不属于 YAML 配置。外部 Key 由 Admin 创建并保存在 `state.database` 的 DuckDB 中；Usage runtime 每次启动会幂等维护一个服务端内建作用域 `builtin-local`，因此初始数据库即使没有外部 Key，管理台工具集仍有固定的归属。

创建时 `provider_access` 必填：`mode: selected` 要求至少一个已知 Provider ID，`mode: all` 要求 `provider_ids` 为空。已禁用或暂时无模型的 Provider 仍可预授权；内建 ID 为 `chatgptweb`、`codexoauth`。`selected` 绑定不会随未来 Provider 自动扩展，`all` 会。权限更新、启停和轮换使用专用认证索引原子热切换，不重载 Provider 配置。

- Key ID 需匹配 `[a-z0-9][a-z0-9._-]{0,63}`，`default` 为历史用量保留 ID，不能配置。
- `builtin-local` 是内建工具 scope，不是可供外部请求携带的 secret：不保存 raw key/hash，不能创建同名 Key、修改权限、启停、轮换或删除；管理台可以查看它的有效模型。临时对话、搜索、图片代理、图片任务和图片库在未指定作用域时使用它。
- 每个数据请求必须携带 Key；缺失、空 Header、未知、禁用、格式错误或两个身份 Header 冲突时均返回 401，且不产生用量记录。
- OpenAI 使用 `Authorization: Bearer <key>`，Anthropic 使用 `X-API-Key: <key>`；两种 Header 可兼容，但同时出现时必须为同一 Key。
- 原始客户端 Key 不写入日志、DuckDB、归档或管理 API，也不会转发给上游。
- Admin 可创建、启停、轮换或删除客户端 Key。创建和轮换仅在成功响应中显示一次明文；Key 摘要由运行时存储管理。
- Key 的摘要、启用状态、创建时间、轮换/撤销时间和最后使用时间均保存在 DuckDB；Admin 列表只展示非敏感管理字段。
- `inbound_api_key`、`AETHERRELAY_INBOUND_API_KEY`、`usage_file` 与 `AETHERRELAY_USAGE_FILE` 已删除，配置中出现会启动失败。

客户端 Key 是必需的应用层认证；若监听 `0.0.0.0:8080` 或 `:8080`，仍应在防火墙、反向代理或私有网络层实施额外访问控制。

## Server 配置

| 配置或环境变量 | 说明 |
| --- | --- |
| `server.listen_addr` / `AETHERRELAY_LISTEN_ADDR` | 完整监听地址，默认 `127.0.0.1:8080`。 |
| `AETHERRELAY_PORT` | 仅替换端口，生成 `127.0.0.1:<port>`。 |
| `max_request_body_bytes` / `AETHERRELAY_MAX_REQUEST_BODY_BYTES` | 客户端请求体上限。 |
| `max_upstream_response_bytes` / `AETHERRELAY_MAX_UPSTREAM_RESPONSE_BYTES` | 非流式上游响应上限。 |
| `max_stream_bytes`、`max_sse_line_bytes` | 流式累计输出与单条 SSE 行上限。 |
| `request_timeout_seconds` | 非流式总超时及流式等待响应头超时。 |
| `stream_idle_timeout_seconds` | 连续未收到 SSE 数据的超时；`0` 禁用。 |
| `stream_first_event_timeout_seconds` | HTTP 上游 SSE 首个有效事件等待超时，默认 `30` 秒；用于防止上游只返回响应头或空注释后长期无数据。 |
| `upstream_body_idle_timeout_seconds` | 非流式上游响应体连续无新数据的超时，默认 `180` 秒；`0` 禁用。用于允许 DeepSeek 等推理模型在已返回响应头后持续生成较长时间，同时避免请求无限等待。 |
| `archive_full_content` | 是否落盘完整请求/响应正文。 |
| `verbose_logging`、`log_format` | 是否输出详细请求/上游观测日志，以及 `json`/`text` 格式。 |
| `metrics_remote_access`、`metrics_allowed_cidrs` | `/metrics`、`/stats` 的远程访问控制。 |
| `admin_auth_enabled` / `AETHERRELAY_ADMIN_AUTH_ENABLED` | Admin 登录开关，默认 `false`（保持 loopback-only）。 |
| `admin_base_path` / `AETHERRELAY_ADMIN_BASE_PATH` | Admin 页面与 API 前缀，默认 `/admin`；启动期路由，变更需重启。 |
| `admin_default_language` | Admin Web 的实例默认语言，仅 `zh-CN` 或 `en-US`，默认 `zh-CN`；可在管理页热更新。 |
| `admin_username` / `AETHERRELAY_ADMIN_USERNAME` | 单管理员账号（开启认证时必填，区分大小写）。 |
| `admin_password_hash` / `AETHERRELAY_ADMIN_PASSWORD_HASH` | Argon2id PHC 哈希（开启认证时必填；禁止明文）。 |
| `admin_session_cookie_secure` / `AETHERRELAY_ADMIN_SESSION_COOKIE_SECURE` | 会话 Cookie 是否仅随 HTTPS 请求发送，默认 `false`。 |
| `admin_session_ttl_seconds` / `AETHERRELAY_ADMIN_SESSION_TTL_SECONDS` | 会话绝对有效期，默认 `28800`（8h），范围 `300~86400`。 |

环境变量与配置键的完整默认值、上限和校验以 [`config.example.yaml`](../config.example.yaml) 与 `internal/pkg/aetherrelayconfig` 为准。

## 模型流式 SSE

文本生成统一通过 SSE 增量输出。标准推理端点仅在 JSON 请求体包含 `"stream": true` 时进入流式生命周期；`Accept: text/event-stream` 不能单独开启流式响应。

- `/v1/chat/completions` 返回 OpenAI Chat Completions SSE，必要时转换 Anthropic 上游事件；转换期间识别并省略上游 thinking 块及 delta，不将推理内容折叠为 assistant 文本。
- `/v1/messages` 返回 Anthropic Messages SSE，必要时转换 OpenAI 上游事件。
- `/v1/responses` 支持 OpenAI 协议 Provider 的原生 Responses；原生 Provider 的 JSON Schema 等高级能力不由 `responses` 端点标记自动推导，必须由独立 capability 验证并声明。内建 `chatgptweb` 额外提供无状态受限投影：基础文本、data-URI 图片输入、基础 SSE/output/usage。唯一工具例外是单个 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11`：它启动一次隔离的 ChatGPT Web 强制搜索会话，返回 `web_search_call`、来源和 `url_citation`。它不支持 function calling、混合工具、JSON Schema、`previous_response_id`、后台/realtime、远程图片 URL 或 file ID。
- `/v1/search` 是 AetherRelay 的非流式扩展端点，不是 OpenAI 官方端点别名。请求体仅接受 `model` 与纯文本 `query`；响应为 `search.result`，含 `output_text`、`sources` 与估算 `usage`。它只选择内建 `chatgptweb` 的已发现模型，管理型 Provider 即使有同名模型或更高优先级也不会接收该请求；无可用 ChatGPT Web 搜索能力时返回明确错误，不降级为普通文本生成。
- 内建 `codexoauth` 只服务原生 `POST /v1/responses`：请求与 SSE 事件不经过 ChatGPT Web 消息树转换，非流式结果从上游 `response.completed` 提取原始 Response 对象。它使用实现内固定的 Codex OAuth 上游 Responses、模型发现和用量端点，不支持通过 Provider 管理页切换 protocol、base URL 或 endpoints；需要可切换上游端点时应创建管理型直连 Provider。P0 不支持 WebSocket/realtime、`/responses/compact` 或网页会话/插件能力；`/v1/chat/completions` 不能路由到该 Provider。
- 跨协议转换按模型方向化 capability 开放：Level 1 为非流式文本，Level 2 增加纯文本 SSE，Level 3 增加非流式 function tools；流式工具、多模态、结构化输出、continuation 仍在访问上游前拒绝。thinking/reasoning 只有配置方向专用 adapter 时才以降级模式开放。最近调用、usage 与 Prometheus 请求指标会同时记录 `conversion_mode`、`conversion_level`、转换耗时和拒绝/降级能力。

浏览器客户端应使用 `fetch()` + `ReadableStream` 发送 POST 请求和认证 Header，不使用只支持 GET 语义的原生 `EventSource`。完整合同见[核心代理与路由设计](design/proxy-core.md#统一流式-sse)。

## 统一状态工作区

```yaml
state:
  dir: var
  database: aetherrelay.duckdb
  memory_limit: 256MB
  threads: 2
  query_cache_seconds: 15
  interaction_retention: 500 # 每个客户端 API Key 独立保留最近 500 轮
```

`state.dir` 是单实例唯一的持久化工作区，相对路径按 `config.yaml` 所在目录解析。`state.database` 必须是该目录下的本地 DuckDB 文件；它是用量、ChatGPT 账号、图片任务、图片索引和标签的唯一结构化状态 authority。多个实例不得共享同一个工作区。数据库不可打开、不可迁移或资源参数不一致时，启用对应能力的模块会在启动期失败，不会降级为空状态运行。

`state.database` 的业务表按 owner 划分：Provider、ChatGPT Web 账号和 Codex OAuth 账号以不同 scope 写入 `secure_documents`，payload 在进入数据库前已加密；用量、图片任务、图片索引与标签、Admin 在线搜索历史继续使用各自的查询表。`builtin-local` 只作为服务端工具调用的稳定 `api_key_id` 元数据，不与管理员登录用户名/临时会话 owner 混用。图片元数据和搜索来源可保留 JSON 扩展列，但不包含上述三类可恢复凭据。

Usage runtime 当前使用重新基线化的最终 schema v1（版本名 `usage_provider_access_v1`），不再执行历史增量 migration。首次遇到旧 usage schema 时会原子重建 `usage_events`、`client_api_key_metadata`、`client_api_key_provider_access` 和 usage 的 `schema_migrations` 记录，因此旧用量和旧客户端 API Key 会被清除；Provider、账号池、任务、图片、搜索历史和临时会话等其他 owner 的表不受影响。完成该次重建后，后续启动会复用最终 v1 并保留新产生的数据。升级前如需回查旧数据，应先备份整个 `state.database`。

Admin「功能集 → 在线搜索」仅将成功结果保存到该历史表。历史以登录管理员用户名隔离；未启用 Admin 登录时使用稳定的本地 `admin` 作用域。每个作用域最多保留 200 条，自动清理 30 天前的记录；答案、查询和来源始终只保存在服务器 DuckDB，不写入浏览器存储。`POST /v1/search` 及协议内的单次搜索保持无状态，不会创建这些历史记录。

工作区固定包含 `interactions/`、`images/`、`image_thumbnails/` 与 DuckDB 文件。原始图片仍保存在文件系统，数据库只保存其元数据与索引；交互归档目录固定为 `interactions/{api_key_id}/{round_id}/`。图片任务与图片资产同样以客户端 API Key ID 作用域隔离：磁盘目录为 `images/{安全作用域}/{日期路径}` 和 `image_thumbnails/{安全作用域}/{日期路径}.png`，数据库索引与标签主键为 `(api_key_id, path)`；Admin 图片任务/图片库缺省使用 `builtin-local`，显式作用域仍需是已存在的 Key。整个目录应由运行用户以私有权限持有，且不得提交到版本库。

## ChatGPT Web 本地数据

```yaml
chatgpt_web:
  provider_enabled: true
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

- ChatGPT Web 账号池、上游、图片存储和异步图片任务始终装配，不提供生命周期开关。
- `provider_enabled` 只控制内建 Provider 是否参与模型路由；省略时为 `true`，可通过 Provider 管理页热更新。它不会停止账号刷新、图片任务或临时对话。
- `priority` 是内建 Provider 的候选优先级（`-1000` 到 `1000`，默认 `10`），可通过 Provider 管理页热更新；ChatGPT Web 不作为回退候选。
- 账号、任务、图片索引和标签保存于 `state.database`；不得写入 YAML、环境变量、日志或版本库。
- 旧的 `usage_store`、`chatgpt_web.data_dir`、`interaction_dir` 及相应环境变量均不再支持；所有本地路径和 DuckDB 资源参数只能在 `state` 中声明。
- `refresh_account_interval_minute: 0` 关闭周期刷新；正数为刷新间隔（分钟）。它不触发密码重登。
- `temporary_chat` 控制 Admin「临时对话」：
  - `enabled` 默认 `true`；显式 `false` 只关闭临时对话。
  - `retention_days` 必须为正；过期且无活跃流的会话会被清理。管理员删除不进入回收站。
  - `max_conversations` 达到上限时拒绝新建并提示先删旧记录，不会静默删除历史。
  - `max_messages_per_conversation` / `max_message_bytes` / `turn_timeout_seconds` 均为正数上限。
  - 会话正文只写入 `state.database` 的专用表；浏览器不得使用 localStorage/sessionStorage 保存消息或上游锚点。
  - 消息查询、附件读取、turn 更新均要求 `owner_id + conversation_id`；构造每轮上游文本请求时只回放当前会话的成功历史，使用随机 `parent_message_id` 和 `history_and_training_disabled=true`。管理页切换/创建/删除会话时取消旧请求，迟到响应按会话代际丢弃。
- 进程自动注入固定 ID 为 `chatgptweb` 的内建 Provider（不持久化到 YAML）。公开模型目录来自账号池对 ChatGPT Web `/backend-api/models` 的模型 ID 枚举并集；上游发现的模型特征只供账号池内部执行选择，不进入 `model_metadata` 或 `/v1/models`。
- 临时对话会持久化创建时的请求模型；仅当 ChatGPT Web SSE 的 assistant `message.metadata.model_slug` 明确返回时，才记录并展示该轮上游实际模型。模型正文的自述不作为路由证据；上游未返回该字段时管理页显示“上游未返回”。
- 自动发现结果只存在于进程内有效目录，驱动 `/v1/models`、`/v1/chat/completions`、受限 `/v1/responses`、`/v1/search`、`/v1/images/generations` 与 `/v1/images/edits`。管理型 Provider 目录不得创建 `chatgptweb` 路由；同 ID 的 `model_metadata` 只补充容量。
- 对内建 `chatgptweb`，`/v1/chat/completions` 中唯一的 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11` 工具（或 `web_search_options`）会启动一次强制搜索；仅使用最后一条纯文本 user 消息作为 query。图片、文件、function/tool 调用、结构化输出和工具循环会在访问上游前返回 `conversion_unsupported`。非流式结果含来源和 `url_citation`；流式是在上游搜索完成后一次性发送完整 delta 的兼容 SSE，不是增量搜索流。
- Admin「临时对话」的“联网搜索”是逐轮开关，且「功能集」提供独立的在线搜索页面；二者都使用 `/v1/search` 的强制 ChatGPT Web 选择规则。每次搜索的 prepare/start 请求使用独立随机根并显式发送 `history_and_training_disabled=true`，不复用固定网页根。启用时不接受图片/文件附件；它不创建持久搜索线程，也不承诺深度研究、网页插件或多轮工具调用。ChatGPT Web 账号级 Memory/Reference chat history 由上游控制，不能由代理完全屏蔽。
- 自动模型与管理型 Provider 同名时会保留全部候选；管理型 Provider 默认优先级为 `100`，Codex OAuth 默认 `90`，ChatGPT Web 默认 `10` 且不作为回退。Provider 表提供内建 Provider 的路由启停与优先级控制，并显示重叠摘要。

## Codex OAuth 本地账号池

```yaml
codex_oauth:
  provider_enabled: true
  priority: 90
  refresh_account_interval_minute: 0
```

- 每个正常 Codex OAuth 账号会通过带该账号凭据、`ChatGPT-Account-ID` 与账号代理的 `GET /backend-api/codex/models` 自动发现模型；结果以受限投影持久化到账号池，6 小时后过期。自动发现只处理正常账号；操作员显式选择账号同步模型时可重试异常账号，但不会绕过显式禁用。失败账号以 30 秒到 5 分钟的指数退避重试，不影响其它账号。
- 导入凭据、刷新凭据或完成 OAuth 后会立即提交模型同步；管理页也可对选中账号或全部账号执行“同步模型”。`POST <admin_base_path>/api/codex/accounts/discovery` 接受可选 `account_ids`，返回 `progress_id`；`GET .../discovery/progress/{progress_id}` 返回进度。任务记录只在当前进程中保留 30 分钟，持久化模型快照才是重启后的权威状态。
- 管理页可按账号读取 `GET /backend-api/wham/usage`，并展示上游观测到的套餐类型、主/次窗口、代码审查和附加窗口的 `used_percent`、恢复时间与限制状态。导入、凭据刷新、OAuth 完成会触发一次账号范围刷新；也可在账号池选中账号后手动刷新。快照有效期为 15 分钟，刷新失败会保留上一份快照并标记错误；不会高频轮询，也不把窗口百分比伪装为 Token 数、请求数或路由可用性。
- Codex refresh token 请求遵循当前 CLI 合同：以 JSON 提交 `client_id`、`grant_type=refresh_token` 和 `refresh_token`，不附加刷新阶段的 `scope`。只有上游明确返回 `refresh_token_expired`、`refresh_token_reused`、`refresh_token_invalidated`，或返回 HTTP 401 时，账号才按永久凭据失败处理；普通 400、网络错误和服务端错误不会被误标为 `invalid_token`。新的 PKCE 登录请求包含当前 Codex CLI 使用的离线与 connector scopes。
- Codex 的 refresh token 健康与当前 access token 路由健康分别投影：凭据刷新失败会保留安全错误类别和时间，但不会仅凭该结果把仍能通过鉴权的账号移出路由。成功的模型发现、用量查询或 Responses 请求会恢复系统判定的异常状态；操作员显式设置的 `disabled` 永不被后台成功结果覆盖。恢复状态后会立即刷新有效模型目录。
- Codex Responses 在账号切换耗尽时保留最后一个真实上游失败，不再用后续的“无可用账号”覆盖首个 401、403、429 或 5xx。安全错误响应和日志会携带上游 HTTP 状态但不记录响应正文、Token、账号头或代理；上游 401 且 refresh token 恢复失败时按 `invalid_token` 反馈，不再误记为普通“上游故障”。
- 调用中上游明确返回的 `usage_limit_reached` 仍会另行记录为账号/模型级额度耗尽与可选恢复时间，并驱动该模型冷却；普通 429 仍只产生模型冷却。这个运行时观察与管理页的套餐用量窗口相互补充，不能彼此替代。
- 可路由模型始终是全部健康账号模型快照的并集；不提供 `codex_oauth.models` 筛选项。管理型 Provider 使用同名模型时，两者都会进入候选链；其默认优先级为 `100`，Codex OAuth 默认 `90`，可在安全的原生 Responses 失败场景回退。`provider_enabled` 与 `priority` 是可热更新的路由策略。
- 账号（access/refresh/id token、ChatGPT account ID、邮箱、到期时间与账号代理）以 AES-256-GCM 加密载荷写入 `state.database`。管理列表直接显示邮箱，但不返回 token、账号 ID 或代理 URL。
- 账号代理一旦配置，会同时用于 OAuth 授权码换令牌、refresh token 刷新、模型发现、`https://chatgpt.com/backend-api/wham/usage` 和 `https://chatgpt.com/backend-api/codex/responses` 请求，避免刷新、发现、用量读取与实际调用的出口 IP 不一致。
- 上游 `401` 会按本地账号 ID 单飞刷新，然后仅重试一次尚未向客户端写出的请求；刷新永久失败或第二次仍被拒绝时账号标为异常。`429`、超时、网络和上游失败按模型冷却，`Retry-After`（最多 3600 秒）优先。
- `refresh_account_interval_minute: 0` 关闭临期刷新；正数只刷新有可解析到期时间且将在 5 分钟内失效的正常账号。没有到期元数据的导入凭据仍可在实际 `401` 时刷新，不会被定时任务反复触碰。
- `refresh_account_interval_minute` 决定定时刷新周期，修改后必须重启 AetherRelay；账号池本身始终启用。

## 本地管理页

访问 `http://127.0.0.1:8080/admin/`（或自定义 `admin_base_path`）可管理 Provider、客户端 Key、查看 API Key 用量；「账号池」使用统一账号列表展示 ChatGPT Web 与 Codex CLI 两个凭据槽，「功能集」提供图片任务、图片库、在线搜索与临时对话。账号关联使用不可逆 `identity_key`，不向 Admin 暴露上游 account ID；每个槽分别展示凭据刷新、额度、模型缓存、用量窗口和能力故障，不能跨槽共享 refresh token。统一页面统计正常/限流、异常/禁用账号数、正常或限流 ChatGPT Web 槽的可用图片额度合计，以及凭据刷新失败槽位数（ChatGPT Web / Codex CLI 分开统计）；禁用或异常槽不计入图片额度。内建 Provider 会直接显示不可用原因、可路由账号数和模型数。槽位操作仍通过该 Admin 前缀下的受鉴权账号 API 执行。

管理页支持简体中文与 English。语言选择优先级为 URL `?lang=zh-CN|en-US`（仅当前访问）> 浏览器语言偏好 Cookie > `server.admin_default_language` > 浏览器语言 > `zh-CN`。页面顶部选择器会保存非敏感的浏览器偏好。该设置不影响代理请求、账号池或 OAuth 行为。

Provider 表的“来源”字段仅作展示：运行时内建 Provider 为 `builtin`，官方 Base URL 为 `official`，其余为 `third_party`。它不会写回 YAML，也不影响路由或安全判断。管理型 Provider 使用单项接口新增（`POST <admin_base_path>/api/providers`）、局部更新（`PATCH <admin_base_path>/api/providers/{name}`）和删除（`DELETE <admin_base_path>/api/providers/{name}`）；Web 编辑表单只提交实际变化的字段。PATCH 请求未提供 API Key 或提供空字符串时保留原凭据，只有显式 `clear_api_key: true` 才会清空。Provider 名称是不可变标识，改名需要删除旧项后新增。内建 Provider 不可删除，通过独立接口热更新路由启停与优先级。状态列合并账号池可用性与请求健康度，避免同一 Provider 出现两套相互矛盾的状态。

ChatGPT Web 账号池、图片任务、图片存储与 Codex OAuth 账号池始终装配。图片预览通过 Admin 鉴权的同源读取端点 `GET <admin_base_path>/api/chatgpt/images/content` 加载，不暴露通用 `/files/**`。ChatGPT Web 生图只提供可验证的 raster bytes：`size` 为 `auto` 时保留上游尺寸，明确 `WIDTHxHEIGHT` 时本地裁切/缩放并记录实际宽高；SVG/vector 文件输出没有上游协议支持，会被明确拒绝。两类账号导出是唯一有意返回完整凭据的管理操作，均固定返回可直接重新导入的 JSON 数组并使用 `Cache-Control: no-store`；OAuth callback、token 与代理不会写入浏览器持久化存储。

ChatGPT 账号列表 `GET <base>/api/chatgpt/accounts` 始终脱敏 access token，且不返回账号代理。列表会分别投影仍生效的文本与生图模型冷却（模型、错误类别、恢复时间），以及最近凭据刷新成功时间或失败类别/时间，供管理页只读展示；绝不返回原始 OAuth 错误、Token 或代理。冷却窗口目前固定为 60 秒，不提供 YAML 或 Web 调参。修改、删除、刷新和导出均使用稳定的 `id`，而不是 token：

ChatGPT Web 的 `quota` 是上游 `conversation/init` 返回的图片生成剩余额度，不是文本 Token 或请求额度。刷新兼容额度字段和图片能力名称的 snake_case / camelCase 变体；无法识别额度时该账号刷新失败并保留原值，只有上游明确返回零额度或图片能力阻断时才记为 `0`。纯 access token、OAuth 登录、完整 OAuth 导入和密码登录来源采用同一刷新语义；手工刷新可重试异常账号，但不会绕过显式禁用。手工刷新同时尝试续期临期 OAuth 凭据；即使旧 access token 仍能读取账号信息，续期失败也会计入该账号失败并在进度中明确显示，不再报告为整体成功。所选账号无一可刷新时任务返回明确错误，不显示为成功的 `0/0`。单账号信息刷新最多等待 45 秒，取消会传递至上游 HTTP 请求；刷新进度使用独立调度通道，不会被长耗时账号命令阻塞。Codex OAuth 用量使用独立的窗口快照；显式用量刷新可重试异常账号并在 `401` 后刷新凭据一次，但不会绕过显式禁用。未进入候选的所选账号计入失败和总数，不显示为 `0/0`。未知 `used_percent` 不按 `0%` 处理，无有效窗口或解析失败时保留最近一次成功快照并记录安全错误类别。

```text
POST   <base>/api/chatgpt/accounts                 # {"tokens":[...],"accounts":[...],"source_type":"web"}；tokens/accounts 至少一项
PATCH  <base>/api/chatgpt/accounts/{id}            # type/status/quota/proxy 的局部更新
DELETE <base>/api/chatgpt/accounts                 # {"ids":[...]}
POST   <base>/api/chatgpt/accounts/refresh         # {"account_ids":[...]}；空数组表示全部
POST   <base>/api/chatgpt/accounts/export          # {"ids":[...]}；返回可直接作为 accounts 重新导入的凭据数组，Cache-Control: no-store
POST   <base>/api/codex/accounts/export            # {"ids":[...]}；返回可直接重新导入的凭据数组，Cache-Control: no-store
```

导出属于刻意的敏感操作，只应在受控的本机或已启用 HTTPS 登录保护的 Admin 会话中调用；不要把响应写入日志、浏览器持久化存储或工单。

两类账号池的导入/导出遵循同一使用约定：导入兼容既有 `{ "accounts": [...] }` 包装，也接受裸对象数组 `[...]` 或单条凭据对象 `{...}`；导出固定返回数组。完整 OAuth 对象需要包含可用的 `access_token` 与 `refresh_token`，`id_token` 可选。凭据类型由 `/api/chatgpt/accounts` 或 `/api/codex/accounts` 导入入口决定，不强制要求冗余 `credential_type` 字段；服务端仍拒绝把一类 OAuth 导出导入另一类账号池，避免使用错误 OAuth client 刷新 refresh token。ChatGPT Web 额外接受 `tokens` 字符串数组及管理页中的换行/逗号分隔 token 文本，适合只有 access token 的场景；完整对象不会降级成单 token 导入。

管理页以 JSON 文件作为主要导入方式，可直接选择上述导出文件；粘贴 JSON 是辅助方式，两者不能同时使用。分槽位导入可一次选择最多 20 个 JSON 文件，浏览器会先读取选择集，再合并为一次 HTTP 请求。ChatGPT Web 任一文件无效时不会提交；Codex 会整份忽略无法读取、解析、缺少必要凭据或凭据类型错误的文件，并批量提交其余有效文件。文件选择集、合并后的 HTTP 请求体均限制为 1 MiB，整个批次最多导入 1000 个有效账号。浏览器只在提交期间读取文件，不预览、不写入 Web Storage，并在提交或关闭弹窗后清空输入；后端会再次执行数量和凭据结构校验。

### 默认模式（`admin_auth_enabled: false`）

- `<admin_base_path>` 及 API **永远限制 loopback**，即使代理监听在非 loopback 地址。
- 写接口仍要求浏览器意图头 `X-AetherRelay-Admin: 1`（不是身份凭据）。
- Provider API Key 只显示“已配置”，从不回显明文；保存时保留未修改值，并将完整 Provider 目录重新加密写入 DuckDB。
- 使用统计页查询 DuckDB，并支持按时间、API Key、Provider、Model、Outcome 和估算标记筛选。

### 安全登录模式（`admin_auth_enabled: true`）

开启后取消 Admin 的 loopback 限制；任意来源都必须先登录并持有有效会话。设计详见[安全与认证设计](design/security.md#admin-登录可选)。

```bash
# 交互式生成 Argon2id 密码哈希（仅 TTY；密码不进参数/日志）
AetherRelay admin password-hash
export AETHERRELAY_ADMIN_PASSWORD_HASH='...'

# 或直接创建/重置账号密码：自动写入 admin_auth_enabled、账号与哈希，并要求重启生效。
AetherRelay admin set-credentials --username ops-admin --config config.yaml
```

```yaml
server:
  admin_auth_enabled: true
  admin_base_path: /ops/AetherRelay   # 可选；默认 /admin；变更需重启
  admin_default_language: zh-CN    # 可选；zh-CN 或 en-US，可在管理页热更新
  admin_username: ops-admin
  admin_password_hash: ${AETHERRELAY_ADMIN_PASSWORD_HASH}
  admin_session_cookie_secure: true # 可选；开启后仅 HTTPS 可携带会话 Cookie
  admin_session_ttl_seconds: 28800
```

要点：

- 必须配置合法 Argon2id PHC（固定参数 `m=65536,t=3,p=1`）；缺失或非法哈希会使进程在监听前启动失败。
- 会话为进程内内存 Cookie（`HttpOnly` + `SameSite=Strict`）；`admin_session_cookie_secure=true` 时额外带 `Secure`，浏览器仅会在 HTTPS 请求中携带会话。HTTP 仅适用于受信网络，生产环境推荐 HTTPS。代理部署时应保留外部 `Host`；应用不信任 forwarded header。
- 状态变更请求需要会话 Cookie 与 `X-AetherRelay-CSRF`；未登录 API 返回 JSON `401`，页面 `303` 到 `<basePath>/login`。
- 认证相关配置热更新成功后清空全部会话；`admin_base_path` 变更必须重启。
- 该模式不替代 TLS、主机账户隔离或配置文件权限保护。

Admin usage API 的筛选参数、导出边界与响应格式以当前管理页、`internal/modules/application/adminapi/service/admin` 的合同测试和 DuckDB 查询实现为准。

模型级 `model_metadata` 可声明 `reasoning_supported`、`reasoning_default_effort` 与 `reasoning_efforts`。`native_responses_tools` 和 `native_responses_images` 只描述模型原生 Responses 路径能力，不代表 Responses↔Anthropic 转换能力。转换能力以 `exact model + upstream endpoint` 为唯一配置键：`messages` 对应 Responses→Anthropic，`responses` 对应 Anthropic→Responses，不绑定 Provider。

endpoint 下只允许选择固定 profile：`level1`、`level2`、`level2_reasoning`、`level3`、`level3_reasoning`。模板自动展开 Level、text、streaming、tools 和方向匹配的 reasoning adapter，普通用户不能逐字段修改。`_reasoning` 模板使用模型的 `reasoning_default_effort`，因此要求该值已声明且包含在 `reasoning_efforts`。Provider 配置只保留模型、协议和原生 endpoints；切换 endpoint 后运行时自动重新匹配模板。

| Profile | Level | 非流式文本 | 文本 SSE | 非流式 function tools | Reasoning 降级适配 |
| --- | ---: | --- | --- | --- | --- |
| `level1` | 1 | 是 | 否 | 否 | 否 |
| `level2` | 2 | 是 | 是 | 否 | 否 |
| `level2_reasoning` | 2 | 是 | 是 | 否 | 是 |
| `level3` | 3 | 是 | 是 | 是 | 否 |
| `level3_reasoning` | 3 | 是 | 是 | 是 | 是 |

所有现有 profile 的 images、documents、structured output、continuation 和流式 tools 均为不支持。`messages` endpoint 的 reasoning adapter 固定为 `responses_to_anthropic_adaptive`，`responses` endpoint 固定为 `anthropic_to_responses_effort`；二者的目标 effort 都取模型 `reasoning_default_effort`。不得通过 profile 名推断表中未列出的能力。

跨协议 `reasoning/thinking` 只能通过 `_reasoning` 固定模板降级开放。模板按 endpoint 自动选择 adapter：Responses→Anthropic 使用 `responses_to_anthropic_adaptive`，Anthropic→Responses 使用 `anthropic_to_responses_effort`，目标 effort 固定为模型默认值。客户端传入的具体 effort 或 manual `budget_tokens` 不会跨协议换算，Anthropic manual `thinking.type=enabled` 会拒绝。上游返回的 reasoning/thinking 块和 delta 只会被识别后省略，不会转成普通文本；归档和“最近调用明细”记录降级状态。

当前转换实现覆盖纯文本非流式、纯文本 SSE，以及 Level 3 的 function tools 非流式请求/响应；图片、documents、JSON Schema/structured output、continuation 和流式工具事件仍会拒绝。只有 exact model 存在与候选当前 upstream endpoint 匹配的固定模板时才会进入转换候选；`/v1/models` 会发布最终有效能力，其中转换 reasoning 标记 `reasoning_mode: "degrade"`。

当前 `grok-4.5` 的 `messages` 与 `responses` endpoint 均使用 `level3_reasoning` 模板；任意 Provider 只有在提供对应模型和 endpoint 时才激活相应方向。其 hosted web search 不属于转换 tools 的发布范围。

当前 `gpt-5.6-luna` 同样完成双向 Level 3 验证。模型元数据发布 `272,000` context window 和 `128,000` max output tokens。Luna 的 Responses output item 私有 metadata 会被有界省略并标记降级，不属于可转换内容。

`gpt-5.6-luna`、`gpt-5.6-sol` 与 `gpt-5.6-terra` 统一声明 reasoning 支持：允许值均为 `none/low/medium/high/xhigh/max`，默认值均为 `medium`。Reasoning 声明只用于能力发布；Sol 与 Terra 未配置方向化转换模板时，不会因此自动开放跨协议转换。

`gpt-5.5` 发布 `272,000` context window、`128,000` max output tokens，以及 `none/low/medium/high/xhigh` reasoning effort；默认值为 `medium`。当前模型由内建 `codexoauth` 发现并只发布原生 `/v1/responses`，已验证文本、SSE、function tools、tool result 闭环及全部五档 reasoning；不据此开放跨协议转换或图片能力。

`gpt-5.4-mini` 按 [OpenAI 官方模型页](https://developers.openai.com/api/docs/models/gpt-5.4-mini) 发布 `400,000` context window、`128,000` max output tokens，以及 `none/low/medium/high/xhigh` reasoning effort；默认值为 `none`。当前模型由内建 `codexoauth` 发现并只发布原生 `/v1/responses`，已验证文本、SSE、function tools 与 tool result 闭环，不据此开放跨协议转换。固定 Codex OAuth 上游不接受客户端 `max_output_tokens` 字段，代理会在上游调用前明确拒绝；目录中的 `maxOutputTokens` 仅表示模型输出能力上限。
