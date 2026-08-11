# 功能说明

本文按功能域说明 `AetherRelay` 当前提供的能力：入口、启用条件与使用方式。外部应用接入模型能力发现请参阅[外部应用集成指南](integration.md)。配置项的完整含义见[配置参考](configuration.md)，安装部署见[安装与部署](deployment.md)，运行期观测、备份与发布见[运维与发布](operations.md)。

## 功能总览

| 功能域 | 入口 | 启用条件 |
| --- | --- | --- |
| OpenAI / Anthropic 标准代理 | `/v1/chat/completions`、`/v1/messages`、`/v1/responses`、`/v1/completions`、`/v1/embeddings`、`/v1/models` | 管理页配置 Provider |
| 模型路由候选链 | 请求体 exact `model` → 有序候选 | Provider 精确模型 / 账号池发现 + Provider pattern |
| 协议转换 | Chat↔Messages 文本、Responses↔Messages Level 1–3 | 候选链中跨协议 Provider且方向 capability 已发布 |
| 客户端 API Key | 全部数据端点认证 | 外部 Key 由 Admin 创建并保存到 DuckDB；工具集由服务端内建 `builtin-local` scope |
| 用量统计与 DuckDB 持久化 | Admin「使用统计」、`/admin/api/usage/export.csv` | 默认启用（`state.database`） |
| Admin 管理页 | `/admin`（默认 loopback-only） | 默认启用；远程访问需 `admin_auth_enabled` |
| — Provider 管理与健康检查 | Admin「Provider」 | — |
| — 客户端 Key 管理 | Admin「客户端 Key」 | — |
| — 系统信息 | Admin「系统信息」 | 默认启用 |
| — 账号池（ChatGPT Web / Codex OAuth） | Admin「账号池」 | 始终启用 |
| — 功能集：临时对话 / 在线搜索 / 图片任务 / 图片库 | Admin「功能集」 | 始终装配（临时对话可单独关闭） |
| ChatGPT Web 文本与图片代理 | 同上标准端点路由到内建 `chatgptweb` | `chatgpt_web.provider_enabled` |
| ChatGPT Web 在线搜索 | `/v1/search`、协议内 `web_search` 工具 | 始终装配，需可用账号与模型 |
| Codex OAuth 原生 Responses | `/v1/responses` 路由到内建 `codexoauth` | `codex_oauth.provider_enabled` |
| Prometheus 指标 / 统计快照 | `/metrics`、`/stats`、`/stats/stream` | 默认 loopback-only |
| SLO 违规 webhook | 状态变化时异步 POST | `slo_violation_webhook` 配置 |
| 交互归档 | `state.dir/interactions/{api_key_id}/{round_id}/` | 默认启用 |
| Provider live probe | `cmd/aetherrelay-probe` / Admin「检查」按钮 | 独立命令，不进启动流程 |

## 标准数据端点

入站白名单（其它 `/v1/*` 一律 404）：

- **OpenAI**：`POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/completions`、`POST /v1/embeddings`，`GET|POST /v1/models`
- **Anthropic**：`POST /v1/messages`
- **AetherRelay 扩展**：`POST /v1/search`（非 OpenAI 官方别名，仅服务内建 `chatgptweb` 搜索）
- **OpenAI Images**：`POST /v1/images/generations|edits`（OpenAI native 或内建 `chatgptweb` 图片能力）

`GET/POST /v1/models` **本地合成**，不访问上游；返回有效目录中的模型、已知的 `contextWindowTokens` / `maxOutputTokens`、已声明的 `capabilities.reasoning`，以及运行时推导的 `supported_endpoints`。reasoning 能力由 `model_metadata` 按 exact model ID 声明，未声明模型不会被推断支持。`supported_endpoints` 是客户端可调用的完整路径列表，由模型候选 Provider、Provider 原生 `endpoints` 和统一传输矩阵计算，不是静态模型元数据，也不暴露 provider 名、base URL 或密钥。

`supported_endpoints` 只表示模型至少有一个已配置或已发现的目录候选具备该客户端路径合同，不包含请求期健康度和熔断过滤。例如 Anthropic Provider 声明原生 `messages` 时，模型可以同时显示 `/v1/messages` 与经协议转换的 `/v1/chat/completions`；ChatGPT Web 的 `chat_completions` 还会派生 `/v1/search`，`images` 会派生图片生成和编辑两个路径。系统信息中的“开放 API 端点”是实例级路由清单，需与模型的 `supported_endpoints` 取交集后再发起请求；即使目录命中，全部候选临时熔断时仍会返回 `provider_unavailable`。

转发矩阵（`endpoints` 只表示上游直连能力；客户端可服务 path 由矩阵决定，含跨协议基础转换）：

| 客户端 | 上游 protocol | Provider endpoint | 上游 path | 模式 |
| --- | --- | --- | --- | --- |
| `/v1/chat/completions` | openai | `chat_completions` | 同 path | native |
| `/v1/chat/completions` | anthropic | `messages` | `/v1/messages` | `openai_to_anthropic` |
| `/v1/messages` | anthropic | `messages` | 同 path | native |
| `/v1/messages` | openai | `responses` | `/v1/responses` | `anthropic_to_responses`（需发布 capability） |
| `/v1/messages` | openai | `chat_completions` | `/v1/chat/completions` | `anthropic_to_openai` |
| `/v1/responses` | openai | `responses` | 同 path | native |
| `/v1/responses` | anthropic | `messages` | `/v1/messages` | `responses_to_anthropic`（需发布 capability） |
| `/v1/responses` | chatgptweb | `responses` | `chatgptweb_responses` | `chatgptweb_responses` |
| `/v1/responses` | codexoauth | `responses` | `codex_oauth_responses` | `codex_oauth_responses` |
| `/v1/completions` | openai | `completions` | 同 path | native |
| `/v1/embeddings` | openai | `embeddings` | 同 path | native |
| `/v1/search` | chatgptweb | `chat_completions` | `chatgptweb_search` | native |
| `/v1/images/generations\|edits` | openai | `images` | 同入站 path | native |
| `/v1/images/generations\|edits` | chatgptweb | `images` | `chatgptweb_images` | native |

## 模型路由与候选链

路由只按请求体中的 exact `model`（严格区分大小写）解析。**不存在** `default_provider`、旧 `fallbacks` 列表、`X-AI-Provider` / `?provider=` / `provider/model` 前缀等请求侧覆盖手段。

- enabled Provider 的精确 `models` 与账号池发现结果决定模型成员资格；所有 Provider pattern 为这些具体 ID 生成候选，并按 `priority` 降序排序。
- `model_metadata` 只按 exact ID 补充容量信息；它不发布模型、不创建候选，未匹配条目保持未使用状态。
- 有效目录始终合成两个账号池的内建模型并参与同一候选链；同名模型保留全部候选，不相互覆盖。管理型 Provider 默认 `priority=100`，Codex OAuth 默认 `90`（可作原生 Responses 回退），ChatGPT Web 默认 `10` 且不作为回退候选。
- 健康度与熔断：5 分钟有界样本窗口，少于 3 个样本显示 `unknown`；连续 3 次可重试失败打开 30 秒熔断，路由跳过熔断 / `unhealthy` / `credential_error` 候选。不替代账号池真实可用性判断。
- 回退仅发生在客户端响应未提交时，且只针对网络错误、`408`、`429`、`5xx` 或流式首事件探测失败；一次已写出的 SSE/HTTP 响应绝不切换 Provider。普通 HTTP 候选使用统一回退链；ChatGPT Web、Codex OAuth 和图片等可能创建上游状态的执行器只在专用路径能够证明尚未产生副作用时回退，避免重复执行。
- 热更新：管理页保存 Provider 后经与启动期相同的完整校验激活；Client API Key 变更直接刷新认证索引；`state.database` 资源参数与账号定时刷新间隔不热切换。

## 协议转换

Chat Completions↔Messages 的兼容路径只保证纯文本和纯文本 SSE。Responses↔Messages 使用方向化 capability：Level 1 为非流式文本，Level 2 增加纯文本 SSE，Level 3 增加非流式 function tools；图片、documents、JSON Schema、continuation 和流式 tools 仍在访问上游前返回 `conversion_unsupported`。同一 OpenAI Provider 同时声明 `chat_completions` 与 `responses` 且已发布 `anthropic_to_responses` 时，`/v1/messages` 优先使用 Responses Level 转换，旧 Chat 文本转换作为兼容后继。转换请求不把客户端 query 参数带到另一协议上游。转换前后统一 SSE 响应头与边界，见[Responses 与 Anthropic 双向转换](design/responses-anthropic-conversion.md)。

标准推理端点只在请求体 `"stream": true` 时进入流式生命周期；`Accept: text/event-stream` 不能隐式改变请求模式。

## 客户端 API Key

- 所有数据端点必需认证：OpenAI 用 `Authorization: Bearer <key>`，Anthropic 用 `X-API-Key: <key>`；两 Header 可兼容，同时出现必须同一 Key。
- 缺失、空、未知、禁用、格式错误或冲突 Key 均为 401，且不产生用量记录。
- Key ID 匹配 `[a-z0-9][a-z0-9._-]{0,63}`；`default` 为历史用量保留 ID，不能配置。
- 原始 Key 不写入日志、归档或管理 API，也不转发给上游；DuckDB 只保存 SHA-256 摘要。创建/轮换仅在成功响应中显示一次明文。
- Admin 可创建、启停、轮换、删除 Key；创建时间和最后使用时间保存在 DuckDB 并在管理页展示。完整配置见[配置参考](configuration.md#客户端-api-key)。
- 每个 Key 必须选择 `selected`（一个或多个具体 Provider）或 `all`（当前及未来 Provider）访问范围。模型目录、端点能力、转换能力、路由和 fallback 都先按该范围裁剪；零值/非法策略拒绝激活，不会隐式放宽。

## 用量统计

- 每个已接受请求先写 DuckDB `started` 事件，随后结算为 `completed`；流式真实结束态用 outcome（`success`、`client_canceled`、`idle_timeout`、`upstream_truncated`、`upstream_failed` 等）统一写入 DuckDB / Prometheus / `metadata.json`。客户端取消不计为上游故障。
- Admin「使用统计」支持按时间（今日 / 7 天 / 30 天 / 自定义）、API Key、Provider、Model、Outcome 与估算标记筛选，并导出 CSV（单次最大 31 天、100,000 行）。
- ChatGPT Web 相关调用写入同一用量权威：代理文本/受限 responses 为本地估计 token（`estimated=true`），`/v1/images/*` 有上游 Usage 则 `estimated=false`；Admin 工具调用统一归 `api_key_id=builtin-local`。临时会话和搜索历史仍按管理员 owner 隔离，二者不是同一个维度。
- Codex OAuth 原生 Responses 记录 `upstream_protocol=codexoauth` 与上游 Response `usage`（缺失时本地估算）。
- 旧 `usage.csv` 只能一次性显式导入（`cmd/aetherrelay-usage-import`）；`usage_file` 配置已删除。

## Admin 管理页

默认位于 `http://127.0.0.1:8080/admin/`（`admin_base_path` 可改，默认 loopback-only）。支持简体中文与英文（URL `?lang=` > 浏览器偏好 > `admin_default_language` > 浏览器语言 > `zh-CN`）。未登录时写接口需要 `X-AetherRelay-Admin: 1` 意图头；启用登录后需要会话 Cookie 与 `X-AetherRelay-CSRF`。

### Provider

- 管理型 Provider：新增、编辑、启停和删除均使用单项接口；编辑表单只提交实际发生变化的字段，避免旧页面值覆盖其他内容。`PATCH <base>/api/providers/{name}` 只合并请求中出现的字段，未重新输入的 API Key 保持不变，只有显式 `clear_api_key: true` 才会清空。变更加密写入 DuckDB 并热更新。
- Provider 启停、模型匹配或端点变更后会原子刷新有效模型目录；`/v1/models`、Admin 功能模型接口及临时对话模型选择器在下一次读取时使用新目录。仅由已禁用 Provider 提供的模型不会继续显示或路由。
- 内建 Provider（`chatgptweb` / `codexoauth`）：展示路由启停与优先级控制、可路由账号数和模型数、不可用原因；不可删除。
- 运行期可用性：「检查」按钮对当前 Provider 执行一次最小非流式探测，不改写配置；状态合并账号池可用性与请求健康度（`disabled` / `unknown` / `healthy` / `degraded` / `unavailable` / `credential_error` / `endpoint_drift`）。
- 来源列仅作展示：`builtin` / `official` / `third_party`，不参与路由或安全判断。

### 客户端 Key

创建、启停、轮换、删除客户端 API Key，并可编辑 Provider 访问范围、查看当前有效 Provider 与去重模型目录。管理端生成的 Key 仅以 SHA-256 摘要保存在 DuckDB。删除 Key 时同步删除其 Provider 关联、用量调用明细与 `interactions/{api_key_id}/` 交互归档。静态 Provider 被 `selected` Key 引用时拒绝删除并列出引用 Key；先解除绑定后才能删除。

### 使用统计

查询 DuckDB 用量并按多维度筛选、导出 CSV。

### 系统信息

展示服务元数据（名称、版本、Go 版本、启动时间、运行时长）与当前可访问方式（OpenAI / Anthropic / Web search 三类协议入口与认证方式）、注册的 HTTP 端点清单（方法、路径、协议、认证与远程访问策略）。便于核对当前实例暴露面。

### 账号池

账号池页面按统一账号聚合展示，并保留两个互不混用的凭据槽：

- `chatgpt_web` 槽：ChatGPT Web 账号信息、图片额度、对话、搜索和图片能力；只能使用 ChatGPT Web OAuth client 续期。
- `codex_cli` 槽：Codex Responses、模型目录和用量观察；只能使用 Codex CLI OAuth client 续期。
- 运行时 `identity_key` 仍优先由上游 account ID 单向摘要生成，缺失时退化为规范化邮箱摘要；Admin 不返回原始上游 account ID。Web 统一账号展示优先按规范化邮箱跨槽位聚合，邮箱缺失时才使用槽位 `identity_key`，因此同邮箱但不同上游 account ID 的两个凭据可以显示在同一账号行。统一账号状态只是管理投影，不允许两个槽共享或复制 refresh token。
- 统一账号状态按最严重的有效槽位聚合：任一槽异常则为“异常”，否则 ChatGPT Web 限流则为“限流”，全部已配置槽均禁用才为“禁用”，其余存在正常槽则为“正常”。页面同时保留各凭据槽原始状态，并分别统计正常/限流、异常/禁用账号数、所有正常或限流 ChatGPT Web 槽的可用图片额度总和，以及 ChatGPT Web / Codex CLI 两类凭据槽的刷新失败数。

- ChatGPT Web：导入纯 access token 或完整 OAuth 凭据、批量刷新、删除、OAuth 导入；不提供 ChatGPT Web 槽位单独导出。账号池入口本身决定凭据类型，不强制要求冗余 `credential_type` 字段；所有 ChatGPT Web 凭据来源采用同一刷新语义，手工刷新只处理所选账号并尊重显式禁用，无可刷新账号时显示明确错误而非成功 `0/0`。手工刷新若同时续期 OAuth 凭据，即使旧 access token 仍可读取账号信息，凭据续期失败也会计入失败并明确显示。账号刷新兼容上游图片额度字段的 snake_case / camelCase 变体和常见图片能力名称，并明确展示成功/失败数量。未返回可识别额度时刷新失败并保留原额度，不会静默写成 `0`；明确返回额度耗尽或图片能力被阻断时才写入 `0`。单账号上游刷新限制为 45 秒，进度读取独立于长耗时账号命令。只读展示文本/生图模型冷却、最近凭据刷新状态；支持「同步模型」。
- Codex OAuth：导入完整凭据、刷新、删除、PKCE OAuth 导入、批量刷新所选用量、同步模型并轮询进度；不提供 Codex 槽位单独导出。账号池入口本身决定凭据类型，不强制要求冗余 `credential_type` 字段。refresh token 使用当前 Codex CLI 的 JSON 请求合同，并区分明确的过期、重复使用、撤销与暂时性刷新错误，不再把所有 HTTP 400 归为 `invalid_token`。导入后的模型同步和用量刷新只针对本批受影响账号，不触发全量账号扫描。凭据刷新健康与当前 access token 路由健康独立展示，刷新失败不会否定随后成功的模型、用量或 Responses 鉴权；成功结果可恢复系统异常状态，但不会覆盖显式禁用。显式用量刷新和模型同步都可重试异常账号；用量请求遇到 `401` 时会刷新凭据一次。两类任务都会将未进入候选的所选账号计入失败和总数，不显示为成功 `0/0`。用量解析兼容 snake_case / camelCase，使用 `used_percent_known` 区分未知值与 `0%`。响应没有任何有效用量窗口时刷新失败并保留最近一次成功快照，页面显示失败数量和安全错误类别。账号统计与 ChatGPT Web 统一为四列（总数、正常、异常/禁用、可路由），具体用量限制保留在账号行展示；同时展示模型缓存、发现进度/退避、上游用量窗口（套餐、`used_percent`、恢复时间）、模型冷却与额度耗尽状态。
- Codex Responses 的账号切换耗尽后会返回最后一个真实上游失败及安全 HTTP 状态，不再把 401、403、429 或 5xx 覆盖成泛化的 `provider_unavailable`；不会记录上游响应正文或任何账号凭据。
- Web 端保留 ChatGPT Web 和 Codex 两个独立导入入口，并在统一账号列表工具栏提供始终可见的“账号池迁移 ▾”分组入口，集中管理整体包导入与导出；整体包是唯一账号池导出格式。整体包导入先完成整包预检，文件内冲突返回安全的冲突列表且不写入任一槽位；预检通过后按 `account_id`/邮箱建立目标匹配，只有显式 `replace=true` 才替换同邮箱的不同上游账号，跨 Store 失败时明确标记部分成功。分槽位导入和整体包导入均在成功后重新计算统一账号行。ChatGPT Web 额外支持粘贴纯 access token。文件与粘贴内容不能同时使用，单次限制 1 MiB、1000 个账号，提交成功后清除浏览器内存引用，冲突时保留文件以便修改或确认替换后重试。
- Codex 导入、OAuth 完成和凭据刷新自动触发的模型同步与用量刷新在后台静默轮询，只保留主操作的一次结果提示；手工点击“同步模型”或“刷新用量”时才显示对应的独立进度，成功完成信息短暂展示后自动隐藏，轮询错误保留以便排查。
- 所有列表返回稳定本地 ID、邮箱、状态与结果计数，不返回 token、account ID 或代理。整体账号池导出接口是唯一有意返回明文 token 的接口，需二次确认且 `Cache-Control: no-store`。

### 功能集

- **临时对话**：Admin 服务端持久化的多轮文本对话（DuckDB 专用表，浏览器不落会话正文）；历史严格按 `(owner_id, conversation_id)` 读取，管理页切换会话时会取消旧详情/历史/发送/轮询请求，并用会话代际拒绝迟到响应回写，因此一个气泡不会因前端竞态混入另一会话的消息；可附加图片（最多 4 张、合计 20 MiB，PNG/JPEG/GIF/WebP）、逐轮启用联网搜索；保留期 `temporary_chat.retention_days`（默认 30 天）；达到 `max_conversations` 拒绝新建，不静默删历史；重启时 `streaming` 消息标记 `interrupted`，会话进入 `recovery_required`。research / deep_research 专用模型不进入选择器。上游 ChatGPT Web 请求每轮使用独立根并发送 `history_and_training_disabled=true`；账号级 Memory 仍由上游控制，不能宣称绝对隔离。
- **在线搜索**：隔离的强制 ChatGPT Web 搜索页面；每次上游搜索使用独立随机根和 `history_and_training_disabled=true`，结果（答案、查询、来源）服务端保存于 `state.database` 的 `chatgpt_web_search_history`，按登录管理员用户名隔离（未启用登录时用本地 `admin` 作用域）；每个作用域最多 200 条，自动清理 30 天前记录；搜索历史不写入浏览器存储。ChatGPT Web 账号级 Memory/Reference history 仍可能由上游注入，发现跨主题回答时需结合账号设置排查。
- **图片任务**：文生图 / 图生图任务提交与轮询；`api_key_id` 缺省使用 `builtin-local`，显式值必须是已存在的客户端 Key；以该 scope 隔离任务。ChatGPT Web 的 conversation 协议没有原生 `size` 字段，旧实现仅把尺寸写入 prompt，无法保证像素；现在 `size` 只接受 `auto` 或正整数 `WIDTHxHEIGHT`，拿到栅格 bytes 后本地裁切/缩放，明确 WxH 才保证实际尺寸，详情记录实际宽、高、格式。SVG/vector 文件输出不支持并会在上游调用前返回明确错误。所有任务可查看完整详情，排队或运行中的任务可取消，终态任务记录可删除。取消采用协作式取消：AetherRelay 会停止本地等待并尽力取消上游请求，但上游已经受理时不保证立即停止或免除额度消耗；持久化的 `cancelled` 状态不会被迟到结果覆盖。删除仅移除任务记录，已保存到图片库的资产继续保留。失败任务可恢复轮询（不重复生成）或按原参数重新提交（仅 `bootstrap` 阶段失败）；已有 conversation 的任务永不盲目重投。
- **图片库**：图片列表、标签、删除（不可恢复）与缩略图默认使用 `builtin-local`，也可显式选择已存在的其它 Key；只返回该 scope 的资产。内容经 Admin 鉴权同源端点读取，不暴露通用 `/files/**`。客户端 Key 删除时同步清除其图片任务、图片资产、缩略图、标签和交互归档。

两个账号池始终装配；没有可用账号时，页面显示空池状态，数据面返回明确的无可用账号或模型错误。

## ChatGPT Web 能力

进程自动注入只读内建 Provider `chatgptweb`，该 ID 不能由管理型 Provider 目录创建。模型来自账号池对 `/backend-api/models` 的枚举并集，是 `/v1/models` 与路由的唯一模型权威。

- **文本代理**：`/v1/chat/completions` 支持纯文本与 `text` / `image_url` content parts（仅 PNG/JPEG/GIF/WebP Base64 data URI，最多 4 张、合计 20 MiB、单图 ≤4000 万像素；不下载远程 URL，无 SSRF 通道；图片仅限 `user` 消息）。
- **受限 Responses 投影**：`/v1/responses` 无状态投影，支持字符串/message-array `input`、`instructions`、`reasoning.effort`、`input_text`、data-URI `input_image` 与基础 buffered/SSE；不保存会话，不支持 tools（除 web_search）、JSON Schema、`previous_response_id`、realtime、远程图片 URL、file ID。可兼容忽略的字段在 `ignored_features` 中可审计；改变语义的字段返回 `conversion_unsupported`。
- **图片**：`/v1/images/generations` / `/v1/images/edits` 代理上游生图；成功响应中的图片字节按认证得到的 `api_key_id` 存储，原始 API Key 不进入路径、数据库或日志。ChatGPT Web 返回认证后的图片 URL 时，内部会下载并验证栅格 bytes；明确 `size=WIDTHxHEIGHT` 会本地规范化为精确 PNG，`auto` 保留上游尺寸。`response_format` 仅支持 `b64_json` / `url`，SVG/vector 不支持；上游仅有不可下载或不可解码内容时请求失败，不声称已归档。
- **在线搜索**：`/v1/search` 扩展端点（仅接受 `model` + 纯文本 `query`，返回 `search.result` 含 `output_text`、`sources`、估算 `usage`），只选择内建 `chatgptweb` 的已发现模型；协议内唯一工具例外是单个 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11`（或 `web_search_options`），启动一次隔离的强制搜索会话，仅使用最后一条纯文本 user 消息作为 query。无可用搜索能力时返回明确错误，不降级为普通文本生成。`POST /v1/search` 保持无状态，不写搜索历史。

## Codex OAuth 账号池

进程自动注入只读内建 Provider `codexoauth`，**只服务原生 `POST /v1/responses`**（`/v1/chat/completions` 不能路由到它）：

- 模型按账号从 `/backend-api/codex/models` 自动发现并缓存 6 小时，失败指数退避；可路由模型是全部健康账号模型快照的并集，不提供 allowlist。
- 上游 `401` 触发单飞 refresh 后仅重试一次尚未写出的请求；`429` 记录模型级冷却并切换未尝试账号；上游已开始 SSE 输出后不切换账号；明确 `usage_limit_reached` 记录账号/模型额度耗尽与上游恢复时间（运行期观察，非官方额度）。
- 非流式 `/v1/responses` 在内部要求上游 SSE，并仅从 `response.completed` 事件返回原始 Response 对象。
- P0 不支持 realtime/WebSocket、`responses/compact`、网页会话或插件；`/v1/search` 与临时对话不经过 Codex 账号域。
- 账号凭据、代理与到期时间只写 `state.database`；管理 API 直接显示邮箱，但不返回 token、账号 ID 或代理。账号代理同时用于 OAuth 换令牌、refresh、模型发现、用量读取与 Responses 请求，保证出口 IP 一致。
- 管理页不提供 Codex 槽位单独导出；选中的统一账号通过整体账号池导出接口获取完整凭据包，该接口显式返回 `Cache-Control: no-store`，页面不预览且仅用短生命周期 Blob 触发下载。

## 可观测性

- **Prometheus 指标**（前缀 `aetherrelay_`）：`requests_total`、`request_duration_seconds`、token 统计与缓存命中、客户端 Key 维度累计、`usage_store_*` 与 `slo_webhook_*` 等。`/stats` 返回进程统计、延迟分位数与 all-time usage 视图；`/stats/stream` 提供 SSE 流式快照。
- **SLO webhook**：配置阈值（缓存命中率、上游错误率、p99 延迟）与巡检周期后，状态变化时异步 POST `entered` / `resolved` 事件，带 `instance_id`、递增 `seq`、`generation` 与稳定 `event_id`；消费方按 `event_id` 幂等。有界队列 + 单 worker，429 优先遵循 `Retry-After`。
- **交互归档**：`state.dir/interactions/{api_key_id}/{round_id}/` 按客户端 API Key 作用域保存脱敏请求元数据、上游请求/响应摘要、客户端响应与 `metadata.json`；`archive_full_content: false` 可禁止正文落盘；每个 API Key 默认保留最近 N 轮（`interaction_retention`）。目录名使用 API Key ID，不包含原始密钥。
- **Provider live probe**：`go run ./cmd/aetherrelay-probe -config config.yaml -provider <owner> -endpoint chat_completions -model <exact-model-id>`，结论为 `success` / `credential_issue` / `endpoint_drift` / `environment_undetermined`；不在服务启动时运行。

## 安全与隐私边界

- 默认仅监听 `127.0.0.1:8080`；非 loopback 监听时仍需网络层另行保护。
- Admin / `/metrics` / `/stats` 默认 loopback-only；远程访问分别由 `admin_auth_enabled`（账号密码 + 会话 + CSRF，任意来源均需登录，无 loopback 旁路）与 `metrics_remote_access` + `metrics_allowed_cidrs` 控制。
- Provider Key 只显示"已配置"，不回显明文；原始客户端 Key 不进日志 / DuckDB / 归档 / Web / 上游。
- 日志与归档脱敏 `Authorization` / `X-API-Key` / `Cookie` 等 Header。
- 会话为进程内内存 Cookie（`HttpOnly` + `SameSite=Strict`，可选 `Secure`）；认证配置热更新后全部会话立即失效。

## 限制与不支持项

- Chat Completions↔Messages 转换仅保证纯文本与纯文本 SSE；Responses↔Messages 可按公开 Level 3 合同支持非流式 function tools。多模态、JSON Schema、continuation 和流式 tools 在转换路径访问上游前拒绝。
- `completions` / `embeddings` 不能由 chat/messages 转换派生，必须由具备对应 `endpoints` 的上游直连服务。
- 不提供 WebSocket / OpenAI Realtime 代理、`responses/compact`；`/v1/search` 不是 OpenAI 官方端点别名。
- 不提供请求侧 Provider 覆盖；候选顺序由语义等级、配置的 `priority` / `fallback`、请求期健康状态和稳定名称共同决定。
- ChatGPT Web 不提供通用 function/tool calling、工具循环、深度研究、网页插件；Codex 不提供网页会话与插件能力。
- 单进程单工作区：`state.database` 不可多实例共享；账号定时刷新间隔修改后必须重启。
