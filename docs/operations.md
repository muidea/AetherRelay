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
- Admin 路径的响应（包括未登录跳转、已登录访问登录页的跳转及认证错误）统一使用 `Cache-Control: no-store`。反向代理不得为整个 Admin 路径追加 `private, max-age=300` 等缓存规则；否则旧的登录跳转可能在会话变化后继续被浏览器复用。Nginx 配置示例见[部署说明](deployment.md#admin-反向代理缓存)。修正后若浏览器仍反复跳转，清除此站点缓存或在禁用缓存状态下重新访问，以排除已缓存的旧 303。
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

### Codex 原生搜索工具

标准 Responses HTTP/SSE、WebSocket 和 Responses Lite 接受顶层 `tools: [{"type":"web_search"}]`。它由 Codex 上游执行，与本地 ChatGPT Web `/v1/search` 无关；代理不自动注入搜索工具、不删除工具绕过错误、不自动替换模型。Lite 中搜索工具留在顶层，namespace 仍按原规则迁移到 `input.additional_tools`。

`external_web_access`（包括 `false`）、`search_context_size`、`filters`、`user_location` 及 `include` 来源选项保持原值；已知选项做基本类型校验，未验证扩展交给上游裁决。当前不提供 preview 工具别名兼容，也不把标准工具降级成 preview，以免缓存搜索限制失效。搜索调用、来源引用及多轮历史保留；搜索进度/结果已交付后禁止重放，必须收到完整 Responses 终态才能按正常完成处理。

代理兼容不代表每个 OAuth 账号/模型都具备搜索权限。上线验收应分别用普通 Responses 和 Lite 发起最小请求，覆盖“携带搜索工具但无需搜索的问答”、实际搜索、`external_web_access=false` 和历史续接，检查完整终态、来源引用及 usage。离线测试不访问真实账号，发布前真实 smoke 需单独执行。工具错误与 `model_not_found`、compaction 模型选择问题分别排查。

`tool_search` 是工具发现，不是网页搜索，也不是 Lite 专属功能。普通 Responses、WS 和 Lite 均保留其配置；`execution=client` 时由客户端执行发现并返回配对的 `tool_search_output`，代理不代为执行、不仅凭该工具自动开启 Lite。两种搜索工具可同时声明，普通问答不要求一定调用它们。缺少 Lite 标记导致的本地 `tool_search` 400 与上游模型权限 404 是两类独立问题。

### 账号池与用量

Codex 模型不可用排查（`CP-FAIL-018` / `CP-CAP-010`）：

账号选择严格复用逐账号 `available_models` 的服务端计算规则（`CP-SCHED-009`），在每次选择/切号时重新计算，而不是信任浏览器已显示的旧列表。粘性账号、首次请求和重试都必须精确匹配原模型；号池并集出现某模型，不代表所有账号都支持。没有满足模型、账号状态、快照有效期、额度、冷却以及 transport/并发限制的候选时直接失败，不降级到不支持的账号或另一模型。
- 同为可用候选时按「额度证据」排序（`CP-SCHED-011`）：拥有新鲜用量快照且没有未到期耗尽窗口的账号，优先于额度未知或快照已过期的账号。准入结论不变——额度未知或快照过期的账号仍会被尝试（`CP-CAP-005`），只是排在有证据的候选之后；当全部候选都没有新鲜证据时保持原有轮转。粘性账号（`CP-SCHED-004`）仍优先于本排序。因此出现「某账号明明已耗尽却被选中」时，先看该账号的用量快照观测时间：超过 15 分钟即视为未知，代价是该轮请求本身成为它的探测。可用 `usage_refresh_interval_minute` 缩短后台补拉间隔来收窄这个窗口。

- 统一账号池的 Codex 槽位和独立 Codex 账号页均可展开完整模型列表。管理 API 新增 `available_models` 与 `model_availability`（model/available/reason/until）。可用性按账号自身模型快照、账号状态、额度及冷却判断；不等于公共 API 模型能力、不保证即时并发或上游权限。缺失/过期快照显示待同步或过期，不宣称可用；刷新列表更新状态，“同步模型”更新发现快照。
- 两处 Codex 账号管理视图也都可修改账号级“最大并发”（默认 `2`，范围 `1–32`）。指纹模式与最大并发的编辑先进入同账号页面草稿，点击“保存设置”才合并提交一次；“取消修改”不请求服务端。它限制该账号跨所有下游客户端合计持有的上游 turn 租约；不是每客户端配额。达到上限时调度器先尝试其它合格账号，全部占满才返回 `accounts_busy`。运行中降低上限不会中断在途请求，排障时不要把 `accounts_busy` 误判为 `accounts_cooling`。
- 上游 HTTP 404 且结构化 `error.code=model_not_found`，或 SSE/WS 的同码失败终态，记录 5 分钟的账号 × exact model 冷却。其它模型和账号状态不受影响；过期自动恢复准入，普通目录刷新不会提前清除此观察，显式重新导入替换凭据会清除旧模型不可用观察。
- HTTP Responses/compact 在尚未输出、无 `previous_response_id`、无非空 turn-state 时，最多尝试 3 个不同账号且始终使用原模型；候选耗尽保留本次最后真实上游错误。普通 404/参数错误不适用；WS 不因该错误迁移会话。这里的"无非空 turn-state"只按客户端请求的原值判断，代理回填的值不参与该判定。冷却期间新的请求若已无可调度账号，可能直接收到 `provider_unavailable`，可在逐账号列表核对具体原因。
- `Codex attempt failed` 日志包含服务端 `request_id`、`inbound_model`、`upstream_model`、`account_attempt`、白名单 `request_kind` / `compaction_reason` / `compaction_phase`、`prompt_cache_key_source`、`turn_state_source`、`client_identity_reason` 和错误码；成功逐次日志为 DEBUG。`client_identity_reason` 只记录验证结果枚举，不记录完整 UA/Originator。`turn_state_source` 为 `client` / `session` / `default` / `stripped` / `forced` / `absent` 有界枚举，只表明 `X-Codex-Turn-State` 的来源，不记录该 header 的值；`session` 与 `default` 表示本次由代理回填，`forced` 表示强制默认开关（`turn_state_force_default`）直接用默认值覆盖了客户端与会话记录，`stripped` 表示存在一个值（客户端提供或记录回放的）但被跨账号守卫剥离，`absent` 表示本次确实没有可用值。`account_attempt` 是本次请求内的账号尝试序号，同账号认证刷新不增加序号。日志不记录完整 turn metadata、上下文或凭据，也不猜测 UI 目标模型。
- `comp_hash_changed` / `pre_turn` 表示客户端在正式对话前请求压缩；这次实际入站仍是旧模型时，代理不强制换成新模型。保持各模型真实的 `comp_hash` 差异，不把所有 hash 改成同值。若旧模型在全部账号均不可用，新建独立会话使用可用模型；旧线程能否恢复需要对应 CLI 版本另行验证。

部署验收：先刷新并核对模型发现结果，再测试新会话及 Astra → 5.5 → Astra 切换，按 request_id 区分 turn 与 compaction。离线回归覆盖错误分类、限次切号、输出后不重放、冷却隔离和管理显示；不代表已验证真实 OAuth 账号访问权限或已修复客户端恢复逻辑。

进程始终注入只读内建 Provider `codexoauth`。它与 `chatgptweb` 是两个独立账号域：不共享 refresh token、账号代理、模型发现、网页会话或临时对话。

| 路径 | `provider` | `api_key_id` | token |
| --- | --- | --- | --- |
| 原生代理 `/v1/responses` → codexoauth | `codexoauth` | 客户端 Key ID | 上游 Response `usage`（缺失时本地估算） |

- 使用统计会记录 `upstream_protocol=codexoauth`、`upstream_endpoint=codex_oauth_responses`、`conversion_mode=codex_oauth_responses`，包括 interaction archive 关闭时的兜底结算。
- 本地准入失败会在使用明细与 CSV 中保留安全的 `failure_class`、`retryable` 和 `retry_after_seconds`；汇总日志同时记录 Usage Event ID，管理员可直接区分账号冷却、并发占满及其它无可用账号原因。以上字段不包含账号身份或凭据。
- 每个账号的代理同时用于 OAuth refresh、Codex `/models` 枚举与 Codex Responses 请求。模型快照按账号缓存 6 小时，失败有独立退避；只有发现并仍在有效期内的账号可调度其模型。导入、刷新凭据和完成 OAuth 都会提交一次立即同步；管理员也可在账号页对选中账号或全部账号执行“同步模型”，并轮询其进度。管理 API 与 Web 表格返回稳定本地 ID、邮箱、状态、结果计数、模型缓存、模型冷却、额度观察与最近刷新状态，不返回 token、account ID 或代理。
- 401 触发单飞 refresh 后只重试一次；普通 429 会记录模型级冷却并切换尚未尝试的账号；上游已开始 SSE 输出后不切换账号，避免重复或拼接两个不同响应。若上游明确返回 `usage_limit_reached`，账号表会记录凭据级“额度耗尽”及上游提供的恢复时间，并冷却该凭据全部模型；这只是运行期观察，不能当作官方剩余额度。
- `/v1/responses` 的非流式请求在内部要求上游 SSE，并在 `response.completed` 或合法的 `response.incomplete` 终态返回原始 Response 对象；上游若返回原生 JSON Response 也会接受。请求中的 `reasoning.effort` 按模型元数据枚举校验，允许值以 `/v1/models` 的 `capabilities.reasoning.efforts` 为准，不支持时返回 400。`POST /v1/responses/input_tokens` 复用同一模型与权限目录但只做本地非计费预估，不获取账号或访问上游。Responses WebSocket 使用同一路径的 GET upgrade，`/v1/responses/compact` 提供 unary JSON 及最小 SSE 投影；Realtime、网页会话和插件仍不属于 Codex OAuth 能力。
- `/v1/responses/compact` 的上游实际走原生 remote compaction v2 `/responses`。若某账号返回 2xx 但没有 compaction item，该账号会被标记为 native compact 不支持；持久化账号文档必须已经使用当前 `remote_compaction_v2` 标记，否则启动失败，不在加载阶段自动改写。
- compact 的普通 request/capability fault 不继续切号；结构化 `model_not_found` 是上述模型级冷却及有限切号的窄例外。其它非 credential 临时失败不会污染普通 Responses 的账号状态、模型冷却或额度观察；credential/429 事实仍保留。WebSocket 后续 turn 只在任何业务帧输出前、完整历史和工具调用可在消息上限内安全重放时迁移账号，最多两次；输出后禁止重放。
- Codex 的 `X-Codex-Turn-Metadata` 与 body `client_metadata` 按 `CP-HDR-011` 分层处理：`installation_id`/`session_id`/`thread_id`/`window_id` 属于代理身份（指纹收敛时由账号 seed 派生，否则等于本次发送的 `Session-Id`/`Thread-Id`/`X-Codex-Window-Id`），三个载体（身份 header、turn metadata、body client_metadata）始终自洽；其中 `window_id` 的**号段取客户端声明**（`X-Codex-Window-Id` 后缀优先，其次 `window_number`，都没有则为 `0`），会话段仍由代理决定，因此 `window_id` 与 `window_number` 不会互相矛盾；`turn_id`/`root_turn_id`/`turn_started_at_unix_ms` 客户端声明即采用；`window_number`/`context_window_id`/`request_kind`/`thread_source`/`sandbox`/`sandbox_mode`/`agent_name`/`auto_review_enabled`/`node_repl_*` 原样透传。白名单之外的键与所有身份字段的客户端原值只记入 `metadata.json` 的 `ignored_features`（字段名，不记值），不影响请求成功。
- 指纹模式按账号设置，默认 `scoped`，仅在排查额度、设备识别或身份投影问题时临时切换 `off` 做对照。Turn-State 的溯源表只保存状态哈希，回填记录按「铸造账号 + 客户端声明的会话」在进程内存持有 opaque 原值（不设过期、重启即清空、多实例不共享），两者都不应把原值加入日志、归档或工单；归档里的 `upstream_request.json` / `upstream_response.json` 只会显示该 header 存在且值为 `<redacted>`。
- `scoped` 还会为每个账号选择客户端来源的 User-Agent/Originator profile：仅接纳 `codex-tui`，剔除 `codex_exec` 和其它工具；存量非 codex-tui profile 在加载时清除，同 family 自动向格式合法且达到最低基线的更高版本提升，不设静态版本上限，同版本平台保持不变。排障时若同一账号仍出现多组上游 User-Agent，先确认流量是否来自升级前、账号是否处于 `off`、或候选身份是否缺失、不一致、低于最低版本或格式非法；不得手工编辑加密账号文档。升级服务后，须由新版 Codex 客户端发起原生 Responses 请求并实际选中该账号，才能自动保存其完整身份；之后同步模型、额度查询与 token 刷新都会复用新 profile。仅更新模型能力 metadata 或点击同步不会凭空提升客户端版本。内置 fallback 为真实观察过的 codex-tui 0.155.0；`off` 也仅透传合法 codex-tui 身份。
- 客户端在某次请求里丢失 `X-Codex-Turn-State` 时，代理会回填该会话最近一次观测到的值；没有任何观测时使用 `default_turn_state`，该值也为空时不发送这个 header。`turn_state_fallback: false` 可立即停用回填（热更新生效，记录仍继续维护）。`turn_state_force_default: true` 是更强的运维开关：该请求只发送 `default_turn_state`，客户端值与记录都不参与（force 优先于 `turn_state_fallback`），用于把会话固定到已知良好的状态；它不写记录，因此关掉开关即恢复原回填链路，同样热更新生效。客户端完全没有声明会话（既无会话 header、无 `client_metadata.session_id`/`thread_id`，也无显式 `prompt_cache_key`）时不记录也不回放，因此不会与其它无状态请求串线。被判定为其它账号铸造而剥离的值不会被回填替换。排查时按 `request_id` 核对日志的 `turn_state_source`；归档 metadata 的 `turn_state_fallback` 在产生结果的 round 上显式写入 `true`/`false`，只有输出前失败才没有该字段，此时以日志为准。
- 完全没有声明会话的请求还会使用服务端请求级 nonce 生成一次性调度 Session 和默认 cache identity；即使客户端重复使用同一个 `X-Request-ID` 也不会合并。需要跨请求账号粘性或缓存连续性的第三方客户端必须显式发送受支持的 Session、conversation metadata 或 `prompt_cache_key`。
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
- `aetherrelay_provider_admission_denials_total{provider,model,reason}`：未发起上游请求的本地准入拒绝数；`reason` 为有界分类，包括账号冷却、并发占满、无合格账号和熔断等。
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

使用统计页同时展示缓存使用率、缓存读取 / 创建 Token、每日使用率趋势、API Key 缓存汇总和每次调用的缓存使用率。`/admin/api/usage/dashboard` 的 `summary`、`daily`、`by_api_key` 均返回成功请求口径的 `cache_input_tokens`、`cached_input_tokens`、`cache_creation_input_tokens`、`cache_hit_rate`；明细接口仍按单次原始记录返回 `cache_hit_rate`。

- 聚合缓存口径为 `cache_hit_rate = sum(cached_input_tokens) / sum(input_tokens)`，分子与分母都只取 `outcome=success` 的请求；先累计 Token 再计算比例，不平均单次请求的百分比，也不是有缓存的请求数占比。
- 缓存创建 Token 单独展示，不计入使用率分子。输入 Token 统一为“含缓存读取与创建”的口径：Anthropic 上游的 `input_tokens` 本就不含缓存读取/创建，网关在解析上游用量时先相加再记账，因此 Anthropic 协议请求的缓存使用率不会超过 100%，跨 Provider / Model 的输入分母也可直接比较；按 Anthropic 协议输出（原生归档响应、Responses→Anthropic 转换）时按同一口径反向扣除，报文形态与上游保持一致。其它协议仍按上游自报值展示，统计层不重写、不截断，也不为掩盖上游异常而做上限截断；历史数据不因口径修正而改写：升级前的 Anthropic 事件仍保留旧分母，跨升级时点的聚合窗口会混合两种口径，排障时按部署时间切分即可。
- 缓存读取是逐次请求命中的累计 Token，不是缓存容量。Responses 的 `usage.input_tokens_details.cached_tokens` 计入读取量，`cache_write_tokens` 计入创建量；普通响应、SSE 和 compact 使用同一缓存字段解析。继续兼容 `cache_creation_input_tokens` / `input_tokens_details.cache_creation_tokens`，但有效的 `cache_write_tokens`（包括显式 `0`）优先，不能将别名相加或用“输入减读取”推算创建量。上游未报告写入时保留现有 `0` 口径；显示 `0` 不代表缓存没有生效，也不据此回填历史数据。
- 所有缓存统计遵循当前时间和维度筛选，但只让成功请求参与缓存读取、创建、字段完整性和使用率聚合。失败、超时、转换拒绝及未完成事件仍完整保留在调用次数、成功/失败统计、原始事件、CSV 和交互归档中，便于排障；它们即使带有局部或估算 Token，也不会污染缓存统计。成功请求中的未知缓存字段按读取、创建分别剔除；使用率分母仅包含读取字段已知的成功请求。聚合标志表示存在有效样本；无有效样本仍显示未提供，不推测为零。默认同时包含精确与估算数据，可切换为“仅精确”。
- 输入 Token 为 0 时接口比例返回 `0`，页面显示 `—`（无分母）；有输入但无缓存时显示 `0%`。字段由已有 DuckDB 明细聚合，无需数据库迁移或回填。

CSV 仅用于导出当前用量，不提供旧 CSV 导入。交互归档默认关闭：不创建 `state.dir/interactions/`，也不保存脱敏元数据。受控排障时先显式设置 `archive_interactions: true`；只有再设置 `archive_full_content: true` 才保存请求和响应正文。归档中的敏感 Header 会脱敏，原始客户端/Provider Key 不会写入。

开启归档后，管理型 Provider 与 Codex OAuth 的每个 round 目录 `state.dir/interactions/{api_key_id}/{round_id}/` 包含：`metadata.json`（路由、耗时、用量与结算结果）、`request.meta.json` 与 `upstream_request.json`（客户端请求与最终上游 attempt 请求的 header）、`upstream_response.json`（该 attempt 的响应 header、状态与耗时）、`response.meta.json`（网关提交给客户端的响应状态与应用层 header）；只有 `archive_full_content: true` 时才另有 `request.json`、`response.{json,sse,txt,bin}` 与最终 HTTP 出站正文 `upstream_request_body.json`。Codex HTTP Responses/compact 另按尝试保存 `upstream_request_001.json`、`upstream_request_001.body.json`、`upstream_response_001.json` 等；编号包含账号切换和刷新重试，握手与终态合并到同一编号，未编号文件保留最近观测。元信息 `body_path` 指向实际归档正文，`body_bytes` 为线上原始字节数，脱敏/附件摘要后文件长度可不同。本地转换失败未发送时不生成上游文件，查看 `conversion_error_path` 与 `unsupported_features`。四个方向的 header 默认走同一脱敏名单，`Authorization` / `X-API-Key` / `Cookie` / `Set-Cookie` / `WWW-Authenticate` 以及 Codex account/session/thread/turn/fingerprint 身份头等以 `<redacted>` 落盘。受控排障需要 header 原值时，可在 `archive_interactions=true` 之外再设置 `archive_unredacted_headers: true`（`CP-OBS-009`）：四类 header 信息全部按原值写入归档，包含明文凭据；最终 Codex 正文中代理注入的 `client_metadata` 身份及内嵌 turn metadata 也随此开关保真，关闭时它们脱敏；该开关不影响日志、指标、错误响应与管理视图，它们仍走同一脱敏名单；开启时启动日志会输出明文凭据告警，归档目录应视同凭据保管。ChatGPT Web 一轮可能包含上传、准备、conversation、轮询和下载等多个 HTTP stage，其逐 attempt 上游归档是已登记的后续待办，当前不会生成上述两个上游文件。

`response.meta.json` 的 `status` 是 HTTP 层实际定型的值，与 `metadata.json` 的业务结算状态可能不同（例如响应已写出 200 后客户端取消，metadata 记 499）；`at` 是响应首次定型的时刻，不是落盘时刻。Codex Responses WebSocket 的协议升级只记录 101 与升级前网关已设置的 header，`hijacked: true` 标明握手响应行由升级方直接写到底层连接、不经过网关的 ResponseWriter。

## 备份与维护

不要直接复制正在写入的 DuckDB 文件。建议流程：停止接收新请求、等待当前写入完成、执行 checkpoint、复制 `state.database`，并将需要保留的 `state.dir/interactions/`、`state.dir/images/` 与 `state.dir/image_thumbnails/` 一并复制，随后恢复服务。数据库与整个 `state.dir` 必须由同一个实例独占。

### DuckDB WAL 回放失败

2026-10-08 x600 在加载 `aetherrelay.duckdb.wal` 时出现 `GetDefaultDatabase with no default database set`，容器循环退出，尚未接收业务请求。用 DuckDB v1.5.4 复现：对带 `length(outcome)` CHECK 的用量表执行 `ALTER TABLE ... ADD COLUMN` 后异常退出，重开文件触发同一断言。这与上游 [WAL 回放问题](https://github.com/duckdb/duckdb/issues/21490) 属于相同错误族；现场的具体回放栈为 `ReplayAlter → SetDefault → BindCheckConstraint`。

启动时只创建最终 schema 的新表或校验已有表，包括 `cached_input_tokens_known` 与 `cache_creation_input_tokens_known`；运行期不再执行 ALTER、补列或升级迁移。现有最终 schema 数据库直接复用，不需要重建或导出导入，历史观测标志原值保留。缺少必需字段的旧布局明确启动失败并保留数据，不自动重置。回归测试覆盖新库、已有最终库的异常退出和重开，以及旧布局拒绝后原始行和字段布局保持不变。离线 WAL 恢复命令用于完整回放已提交操作，不升级 schema。

出现上述回放失败时，先停止服务及所有写入者，保留数据库与 `.wal` 成对备份。使用包含恢复子命令的同版本驱动构建执行：

```bash
./AetherRelay admin recover-state --database /path/to/aetherrelay.duckdb
```

命令要求数据库和 WAL 均为非空普通文件，在同目录生成权限为 `0700` 的 `*.recovery-backup-*`，保存原始两个文件（`0600`）并核对 SHA-256。随后在已初始化的内存数据库中 ATTACH 原库，完整回放 WAL，再执行 checkpoint；最后通过普通打开路径重开，核对所有 main schema 表的行数及聚合行指纹。只输出备份位置和表行数，不输出行内容或凭据。恢复失败保留备份并返回非零，不自动删除 WAL、不创建空库、不自动回滚覆盖原文件。确认恢复成功后再启动服务，并核对健康检查和业务调用。

容器可先 `docker compose stop aetherrelay`，再用相同镜像和数据挂载执行 `docker compose run --rm --no-deps aetherrelay AetherRelay admin recover-state --database /var/lib/aetherrelay/aetherrelay.duckdb`；恢复成功后 `docker compose up -d aetherrelay`。镜像必须包含该子命令。文件锁仍是最后一道保护，不能用它替代停止写入；备份目录包含敏感运行状态，应与主数据一起受控保管。

现场验收（2026-10-08）：原库与 WAL 已保存在 x600 `/home/workspace/deploy/recovery-20261008/original/`，替换前文件另保存在 `before-restore/`；原文件 SHA-256 在替换前再次确认不变。真实副本用恢复命令完整回放并校验各表行数与聚合行指纹，包括 48,622 条用量事件、32 条加密文档、8 条客户端 Key 元数据与 14 条 Provider access 关系。恢复文件传回后再次核对 SHA-256，在停止容器期间替换数据库并移走已回放的原 WAL 至备份。20:29（北京时间）现有 `1985354` 容器重新启动，健康检查 200、重启次数 0；20:30 最小 `gpt-6.1-sol` Responses 非流式请求 round `034054` 返回 200、`outcome=success`，耗时 2.476 秒。数据库恢复已完成，本次迁移防复发代码尚未部署；这一次最小成功请求不能证明长流或持续并发均已恢复。

### 停机与发布屏障

主进程在正常信号、启动失败和运行失败后都等待 `ShutdownChecked`。每次排空使用独立的 30 秒 context，未完成则保留剩余 owner 并每秒重试；只有请求、后台任务、EventHub 和数据库全部释放才记录 `AetherRelay shutdown completed`。HTTP Initiator 在 BeginShutdown 停止接入，在 Quiesce 等待已接收请求；超时不释放路由及依赖。用量和共享状态最后一个 owner 在 checkpoint 成功后才关闭数据库；checkpoint 失败保留句柄供重试。`database/sql` 的底层 Close 错误无法重试，后续调用保留失败回执，不误报成功。

仓库 Compose 与部署脚本生成的 Compose 均使用 `stop_grace_period: 120s`。部署脚本在 `up` 前显式停止旧容器，检查退出状态、OOM 状态以及从该容器当前 StartedAt 起的停机完成记录；检查失败保留旧容器和数据库并中止发布。退出码 0 本身不是数据库安全关闭的证明。直接 `docker compose up -d` 不具备脚本的回执检查，升级使用 `scripts/deploy-docker.sh`。首次从没有完成记录的旧版本切换时，脚本会中止：停止所有写入后成对备份数据库/WAL，使用目标镜像执行离线 checkpoint（存在故障 WAL 时使用恢复命令）并验证普通打开，再由运维显式切换镜像；不添加跳过校验的发布参数。

后续 schema 变更使用独立离线迁移及备份、checkpoint、重开验证，业务启动继续只支持最终 schema。异常断电或 SIGKILL 仍需 WAL 与备份恢复能力；不能通过删除 WAL 强行恢复。

2026-10-08 第二次现场核对：Docker 在 20:49:49 向旧 `1985354` 容器发送 SIGTERM，20:49:59 记录 `Container failed to exit within 10s of signal 15 - using the force` 并发送 SIGKILL；事件同时报告退出码 0。紧接着 `1de0829` 在配置 Block 打开数据库时回放旧 ALTER WAL 失败。证据保存在 x600 `/home/workspace/deploy/recovery-20261008-final/{deploy-events.txt,docker-journal.txt,before.log}`。旧容器已删除，无法进一步从其应用日志确定卡住的具体排空阶段；强制终止及旧 DDL WAL 留存已得到验证。代码核对发现临时聊天 purgeLoop 注册为长期 BackgroundRoutine 任务，停止信号原先只在 Teardown 发送，而框架先等待后台任务退出再执行 Teardown，构成等待循环。维护任务/聊天回合、图片任务及账号刷新现已在 BeginShutdown 取消，保留数据库及 EventHub 直到真实排空。入口回归加载与生产相同的全部组件，并显式启用临时聊天维护任务。

20:56 使用运行镜像 `1de0829` 的 `admin recover-state` 恢复成功。成对原件备份在 `/home/workspace/deploy/data/aetherrelay.duckdb.recovery-backup-927207541/`；恢复前后校验包括 48,720 条用量事件、32 条加密文档、8 条 Key 元数据、14 条 Provider access 关系和 3 条图片记录。恢复后健康检查 200、重启次数 0，真实流式 round `034152` 返回 200，耗时 3.947 秒；本轮最终核对恢复后 161 条已结算业务请求均为 200，容器保持 healthy、重启次数 0。此次恢复运行的仍是 `1de0829`，后续停机改动尚未部署。

DuckDB 更新评估：官方稳定版 [DuckDB 1.5.6](https://github.com/duckdb/duckdb/releases/tag/v1.5.6) 对应 [Go 驱动 v2.10506.0](https://github.com/duckdb/duckdb-go/releases/tag/v2.10506.0)。该版对历史故障副本的普通打开仍产生 `GetDefaultDatabase with no default database set`，不能宣称升级消除了此断言。驱动及 bindings 同步更新到稳定版以采用已有修复（包括中断重试等待及时退出）；离线恢复与停机屏障仍为必要措施。新驱动构建的恢复命令已对本次 48,720 条用量记录的真实副本完成 WAL 回放、checkpoint 和全表行数/聚合行指纹校验，普通打开路径重新验证成功。

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

### Codex 长流超时与账号冷却排查

Codex HTTP 流的持续输出不再受非流式 `request_timeout_seconds` 总时限约束。先检查 `Codex stream stopped` 的 `phase`、`error_class`、`first_event_duration_ms`、`total_duration_ms`、`event_count`、`stream_bytes` 和 `last_event_at`；兼容字段 `duration_ms` 与 `total_duration_ms` 相同。`first_event_timeout` 表示未交付首个业务事件，`idle_timeout` 表示后续事件停滞，`stream_lifetime_timeout` 表示显式配置的本地最大时长到期。诊断不包含工具参数正文或凭据。

账号池 503 的 `accounts_cooling` 与 `Retry-After` 表示暂时冷却，网络/超时/上游失败默认冷却 30 秒；仅一个合格账号时，冷却期间无法切换账号。增加可用账号需由管理员按实际授权启用，不能靠自动启用禁用账号或清空冷却绕过。`accounts_cooling` 不等于新一次上游故障；`no_eligible_account` 或 `accounts_busy_or_excluded` 需结合账号模型、状态和并发占用检查。准入拒绝、客户端取消/写失败和本地最大流时长不会追加 Provider 熔断样本。已有熔断到期后允许恢复请求，真实成功才清零连续失败；HTTP 200 的流仍须以合法终态确认成功。

Codex HTTP/SSE/compact 失败日志新增 `transport_reason`，逐 attempt 的上游响应归档也记录同名字段。按 `request_id`、`account_attempt` 与 round 对照：`dns`、`connection_refused`、`connection_reset`、`connection_aborted`、`broken_pipe`、`tls`、`eof`、`timeout`、`canceled`、`http2_stream_error`、`http2_stream_cancel`、`http2_refused_stream`、`http2_goaway`、`http2_connection_error`、`sse_line_limit`、`unknown`；空值表示没有传输失败观测。HTTP/2 原因按类型识别，不输出 GOAWAY 的 DebugData、StreamError 的 Cause 或原始错误文本；`sse_line_limit` 归协议失败，避免被误判为网络故障。该字段只表示结构化错误链提供的证据，不指明故障发生在代理还是目标端；TLS 未提供可识别类型时也可能为 unknown，不凭文本猜测。关闭交互归档仍输出逐次 WARN 日志。错误分类和冷却仍以原有 error_class 为准，不能把诊断原因作为新的重试或调度策略。

2026-09-30 已在 x600 `/home/workspace/deploy/config/config.yaml` 的 `model_metadata` 补充 `gpt-6.1-sol`，与配置模板一致：272,000 / 872,000 上下文、128,000 最大输出、默认 `low`、reasoning 档位到 `ultra`、原生 Responses tools 与图片输入。来源及 API/Codex 差异见[配置参考](configuration.md)。原配置备份为同目录 `config.yaml.bak-20260930-022632-gpt61-sol`；修改前后解析对比确认其它模型和配置未变。目标元数据规范化 SHA-256 为 `df33c0dbe9ebf85c5bf50dbf919b43756e4a2b1c5e1d682a0e95f63a0ca74c1f`。本次仅更新配置文件，未重新加载运行实例；需重启或通过管理页保存后核对实际目录与账号模型发现，文件更新不证明在线访问成功。

2026-09-29 x600 Event `ae8d95028c58c470c6116c8852d79bdf` 对应 `claude-owner/005588`：09:29:55.664Z 前次 `005586` 发生上游 network 失败，触发 30 秒模型级冷却；09:30:25.382Z 重试在冷却到期前被本地拒绝，`Retry-After=1` 为剩余不足一秒向上取整，没有新的上游请求。09:31:29 起的 `005589` 及 09:35:12 起的 `005591` 均重新准入但再次 network 失败，不能把建议等待时间当作网络恢复保证。四次入站正文哈希相同。旧日志只保留泛化 network，不能回溯确认 DNS、连接或 TLS 原因；本次新增诊断仍需部署后观测，不代表网络故障已修复。

## 2026-09-20 转换观测升级说明

13.0.0 不新增配置开关，配置模板无需改动。交互归档及 content/脱敏开关仍仅控制归档行为，不再决定转换和上游用量观测是否可见。

当前 DuckDB schema 在启动时追加缓存读写 `*_known` 两个布尔列，保留原有用量和客户端配置；更早的不兼容布局仍拒绝启动，不自动重建。历史记录无法证明缓存字段是否存在，因此标为未知，不将旧零值解释为真实未命中。升级前按既有流程备份数据库。Claude 有效会话的默认 cache identity 命名空间发生变化，升级首轮可能冷缓存。

部署后以同一 Event ID 对比用量详情和 metadata.json，再核对最终 upstream_response 与逐次尝试记录。检查 level=2、降级项一致、响应头观测、缓存存在性；继续执行工具续轮直到真实 end_turn。没有 Content-Type 或未知长度可为上游真实缺失，不能仅凭管理页空值判定转发失败。详见[转换设计](design/responses-anthropic-conversion.md)与[身份语义基准](design/codex-identity-semantics.md)。

## 首事件超时收口（13.1.0）

默认及 config.example.yaml 的 `server.stream_first_event_timeout_seconds` 为 300 秒，原配置显式 90/180/0 不会被覆盖；升级已有实例时需要显式调整配置。保留 `stream_idle_timeout_seconds: 300`。不要用改 request_timeout 代替首事件预算，也不要把 safety buffering 当作自动免超时依据。

2026-09-28 核对 x600 留存的 714 条 Codex 请求归档：500 条成功流式请求中，gpt-5.6-luna 首事件最高 179.730 秒，另有 32 次首事件超时；gpt-6-astra 成功首事件最高 83.296 秒，但 Anthropic 转换 + max 路径只有 1 条成功样本，不能证明延长预算必定解决续轮阻塞。300 秒作为扩大等待窗口的有界调整，仍需上线后观测。比较首事件耗时，不用总请求时长判断首事件预算是否不足。

`first_event_timeout` 保留 HTTP 504、usage、归档和请求指标，但不进入 Provider/模型健康失败计数，也不触发或延长熔断。流中 `idle_timeout` 与真实上游失败仍保留健康失败和熔断语义。此前 002512–002514 连续三次首事件超时经请求健康统计开启 30 秒熔断，002515 在熔断期返回 503（`failure_class=circuit_open`、`Retry-After: 24`）；仅设置 `CountUpstream=false` 不能阻止独立的请求健康计数路径。

同日已将 x600 `/home/workspace/deploy/config/config.yaml` 的首事件超时从 180 写为 300 秒，备份为 `config.yaml.bak-20260928-103500-first-event-300`。仅修改配置文件不会自动更新运行实例；需要部署包含健康计数修复的新代码并重新加载配置后，才同时具备 300 秒预算和首事件超时不熔断的行为。

日志应分别保留上游 HTTP 200、下游 HTTP 504 与 first_event_timeout；metadata.error_code 和用量一致。首事件超时与 idle_timeout 分开显示，新统计不回填历史分类。首事件前的响应头即归档；同一请求的重复观测不再重复写正文，相同响应不重复记录，终态失败仍更新。13.0.0 的配置模板“无需改动”仅指该次缓存/观测补全，13.1.0 已更新首事件预算模板。

### Nginx 超时与应用 499 的关联核查

2026-10-09 x600 的 `claude-owner/035632–035635` 均为 `stream=false`，同一请求连续提交；上游在约 1.6–2.4 秒返回 200，并已读取约 1.1 万个事件。Nginx `/v1` 当时继承 `proxy_read_timeout 300s`，四次 access log 均为 504，error log 均为 `while reading response header from upstream`；关闭应用连接后，归档记录 499 `client_canceled`。客户端也声明 300 秒，不能仅凭应用上下文取消确定是哪一层先断开。

排障必须按请求时间和事件 ID 对照应用归档、Nginx access/error log，分别查看上游响应头状态和应用最终状态。后台将 499 标记为“下游连接取消”；失败或处理中记录若总 token 全为零且两个缓存观测标记均为 false，则展示“未取得”，不将这组缺少终结用量的值解释为零消耗。此规则只改变明细展示，不回填数据库或改写聚合值。

Codex 上游归档及安全诊断增加 `last_upstream_event_at`、`last_upstream_event_duration_ms`、`upstream_terminal_event`。最后事件时间仅由业务 data 更新，注释和 keepalive 不续期；终结类型只保留固定白名单。终结类型为空且已有事件，表示读取已开始但未观察到终结；结合读取错误原因判断 EOF、读取超时或连接重置。未知类型的网络错误保留 `unknown`，不得从错误文本猜测或打印原始网络错误及生成内容。

API 代理关闭响应缓冲，以便 SSE 及时交付；`stream=false` 仍等待完整结果。Nginx read timeout 是相邻读取间隔，应用非流式超时是整个请求预算，不能混为同一时限。修改后运行 `nginx -t`，再平滑 reload；新应用预算逻辑及后台展示须部署对应服务版本后生效。此前 `035631` 的流式网络截断仍需部署后继续核对新诊断，不把代理配置调整宣称为上游网络故障已经修复。
