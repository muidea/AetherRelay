# Codex 反向代理首要维护合同

> 合同版本：`3.0.0`
>
> 状态：`active`
>
> 生效日期：2026-08-13
>
> 参考基线：AetherRelay `06aa4b4`、CLIProxyAPI `f43aad76`、sub2api `0e82efe48`

本文是 AetherRelay 的 **Codex 访问反向代理首要维护合同**。凡涉及 Codex 入站路由、请求变换、上游身份、OAuth 账号、调度、重试、HTTP/SSE/WebSocket、compact、模型发现或用量观察的实现、测试和文档，都必须服从本文。

当本文与其它说明冲突时，按以下顺序裁决：

1. 本合同及其自动化验收测试；
2. `docs/configuration.md`、`docs/features.md`、`docs/operations.md`；
3. 其它 `docs/design/` 文档；
4. 历史归档、参考项目和实现注释。

参考项目只提供兼容证据，不能自动改变本合同。合同变化必须先修改本文版本、规则和验收，再修改实现。

## 1. 目标与非目标

### 1.1 目标

`CP-GOAL-001` AetherRelay 必须能够替代 CLIProxyAPI 和 sub2api 的 Codex OAuth 访问反代能力，包括：

- Codex CLI 和 Codex Desktop 使用的 Responses HTTP、SSE、WebSocket；
- Responses compact；
- 原生 Responses 请求、工具调用和多轮工具续链；
- OAuth 账号导入、刷新、模型发现、用量观察、调度、冷却和安全回退；
- OpenAI Chat Completions 与 Anthropic Messages 到 Codex Responses 的兼容入口；
- 可审计、可回归、不会泄漏凭据的运行行为。

`CP-GOAL-002` 兼容的首要基线是当前受支持的 Codex CLI/Desktop 协议，而不是逐行复制任一参考项目。

### 1.2 非目标

`CP-NON-001` 不复制 sub2api 的用户套餐、支付、充值、利润控制、发票或商业结算。

`CP-NON-002` 不因兼容 Codex 而开放请求侧 Provider 指定；客户端仍不能用 header、query 或模型前缀改变 Provider 顺序。

`CP-NON-003` 不伪造有状态 Responses 存储。AetherRelay 不拥有上游会话时，`previous_response_id` 不能被解释成本地会话。

`CP-NON-004` Realtime、语音、SIP 和 Codex Live 不属于当前合同；如需支持，必须另立合同并显式接入。

## 2. 合同变更与版本管理

`CP-VER-001` 本合同使用语义化版本：

- `PATCH`：说明澄清、证据和测试链接变化，不改变外部行为；
- `MINOR`：向后兼容地新增端点、字段或能力；
- `MAJOR`：删除能力、改变默认身份、重试边界或客户端可观察行为。

`CP-VER-002` 每个外部行为必须有稳定规则 ID。测试名称或测试注释必须引用覆盖的规则 ID；实施追踪表必须记录代码与测试证据。

`CP-VER-003` 修改合同的提交顺序必须是：合同及验收变更先出现，随后实现变更；同一提交完成时也必须保持 diff 中合同可独立审阅。

`CP-VER-004` 未实现规则必须标记 `planned`，不得在 `/v1/models`、管理 API 或功能文档中宣称支持。

`CP-VER-005` 从 CLIProxyAPI、sub2api 或真实流量吸收新行为时，必须记录来源版本、最小脱敏样本和选择理由。历史补丁不能无依据进入通用兼容层。

版本记录：`3.0.0` 固化 capacity 降载错误的客户端安全投影，并明确 Chat adapter 必须按 incomplete reason 精确映射终止原因。`2.6.0` 固化 SSE 延迟提交、typed terminal 唯一裁决和 sequential-cutoff reasoning summary 交付。`2.5.0` 固化 `response.incomplete` 合法终态、输出前流内错误切换、WebSocket 终态分类与失败连接处置。`2.4.0` 固化不支持字段清洗、空 `response.completed` 拒绝、确定性 400 安全错误投影和 WebSocket turn 级账号结果登记。`2.3.0` 固化 remote compaction v2、Responses Lite 工具布局、拼接 JSON 文档修复、WebSocket `response.done` 终态、凭据替换能力失效和成功响应额度头观察规则。`2.2.0` 修正 function `call_id` 与 input item `id` 的边界，补充 Responses Lite 和原生持久 WebSocket 增量续 turn 规则，并要求账号级 compact/WebSocket 能力探测参与调度。固定的 ChatGPT 上游 `/backend-api/codex/*` URL 不属于入站端点，不受此变更影响。

## 3. 支持对象与版本策略

`CP-CLIENT-001` 一级客户端是 Codex CLI 与 Codex Desktop；二级客户端是 OpenCode、Claude Code、OpenAI SDK 和 Anthropic SDK。

`CP-CLIENT-002` Codex 上游身份必须集中定义为版本化 profile，不得把版本号、beta 日期和默认 header 分散在 handler 中。

`CP-CLIENT-003` 默认 profile 跟随项目验证过的最新 Codex CLI。升级 profile 必须通过 HTTP、compact、WebSocket、工具续链和模型发现测试。

`CP-CLIENT-004` 不可信的下游身份不得直接透传给 ChatGPT 上游。只有本文 header allowlist 中标记为 `forward` 的字段可以进入上游。

## 4. 入站端点合同

端点分为两级：`core` 是当前 Codex CLI/App 自定义 Provider 所需入口；`AetherRelay adapter` 是网关自身提供的跨协议产品能力，不代表 Codex 客户端协议要求。最新 Codex 使用 `model_providers.<id>.base_url` 配置模型请求；`chatgpt_base_url` 只覆盖 ChatGPT 登录流程，因此 AetherRelay 不提供 `/backend-api/codex/*` 入站别名。

| 规则 | Method | 入站路径 | 分级 | 行为 | 状态 |
| --- | --- | --- | --- | --- | --- |
| `CP-EP-001` | POST | `/v1/responses` | core | OpenAI Responses HTTP/SSE | implemented |
| `CP-EP-002` | GET | `/v1/responses` | core | Responses WebSocket upgrade | implemented |
| `CP-EP-003` | POST | `/v1/responses/compact` | core | Responses compact | implemented |
| `CP-EP-004` | POST/GET | `/backend-api/codex/responses` | retired | 旧参考实现别名；固定返回 404 | rejected |
| `CP-EP-005` | POST | `/backend-api/codex/responses/compact` | retired | 旧参考实现别名；固定返回 404 | rejected |
| `CP-EP-006` | GET/POST | `/backend-api/codex/models` | retired | 旧参考实现别名；固定返回 404 | rejected |
| `CP-EP-007` | POST | `/v1/chat/completions` | AetherRelay adapter | 可转换到 Codex Responses | implemented |
| `CP-EP-008` | POST | `/v1/messages` | AetherRelay adapter | 可转换到 Codex Responses | implemented |
| `CP-EP-012` | GET | `/v1/responses/ws` | retired | 无生产路由证据；固定返回 404 | rejected |
| `CP-EP-013` | GET | `/v1/models` | core | 自定义 Provider 模式的模型发现；与有效目录同代 | implemented |

`POST /v1/models` 是 AetherRelay 在 Codex 补全前已经存在的网关兼容端点，继续服从 AetherRelay 通用 API 合同，但不属于 Codex core 或本次新增端点。CLIProxyAPI、sub2api 与已核对的 Codex 客户端历史均没有要求该 method。

`GET /v1/responses/ws` 与全部 `/backend-api/codex/*` 入站路径不属于支持合同，必须返回 404，不能作为隐藏别名保留。CLIProxyAPI 和 sub2api 中的同名路径仅作为历史证据记录，不覆盖最新 Codex 的自定义 Provider 配置合同。

`CP-EP-009` 同义路径必须共享认证、模型解析、Provider access、用量、归档、限流和错误 envelope；不能建立绕过标准数据面策略的直通 handler。

`CP-EP-010` compact 必须在普通 Responses 前完成路径判定；`/responses/compact` 不能被折叠为 `/responses`。

`CP-EP-011` 未列出的 Codex 子路径必须在认证、用量登记和上游访问前返回 HTTP 404，不得使用 wildcard 透明代理 ChatGPT backend-api。typed `endpoint_unsupported` envelope 另行立项后才能宣称支持。

## 5. 请求变换合同

请求字段只能执行 `pass`、`normalize`、`drop-compatible`、`reject` 四种动作。`drop-compatible` 必须记录有界字段名，不记录值；改变语义的字段必须 `reject`。

| 字段/结构 | 普通 Responses | compact | 规则 |
| --- | --- | --- | --- |
| `model` | trim 后 exact 路由，保留大小写 | 同左，可用 compact-only 映射 | `CP-REQ-001` |
| string `input` | 转为 user/input_text message array | 不允许丢失 compaction item 顺序 | `CP-REQ-002` |
| `instructions` | 保留；缺失按 profile 决定是否补空串 | 缺失/null 补空串 | `CP-REQ-003` |
| `stream` | 上游强制 true，下游按原意投影 | 上游 unary；下游可桥接 SSE | `CP-REQ-004` |
| `store` | 上游强制 false | 删除 | `CP-REQ-005` |
| `reasoning` | 保留并保证所需 include | 按 compact 合同处理 | `CP-REQ-006` |
| `include` | 去重并补 `reasoning.encrypted_content` | 不注入普通 Responses 专属值 | `CP-REQ-007` |
| `tools` | 支持 `function`、`custom`、递归 `namespace`；Responses Lite 额外支持 `tool_search`，并把顶层 `namespace` 迁移到 `input.additional_tools` | 保序；不自动注入图片工具 | `CP-REQ-008` |
| `tool_choice` | 规范化；目标不支持则拒绝 | 删除或拒绝，以能力合同为准 | `CP-REQ-009` |
| `functions/function_call` | 转为 `tools/tool_choice` | 同左 | `CP-REQ-010` |
| `parallel_tool_calls` | boolean 且存在工具时保留；无工具时删除；Responses Lite 强制 false | 删除 | `CP-REQ-011` |
| function/custom/MCP `call_id` | 原样保留，call 与 output 必须一致；禁止改写到 `fc_` 命名空间 | 同左 | `CP-REQ-012` |
| input item `id` | 按 item 类型规范为 `msg_`/`rs_`/`fc_`/`ctc_`/`ctco_`，最长 64 字符，稳定压缩并处理冲突 | 保留顺序并执行同一规范化 | `CP-REQ-013` |
| `previous_response_id` | HTTP 无本地状态时拒绝；原生持久 WS 同 session 增量 turn 保留 | reject | `CP-REQ-014` |
| `prompt_cache_key` | 规范化并绑定 session | 保留适用值 | `CP-REQ-015` |
| `client_metadata` | 只接受已知 Codex 键；当前敏感身份值 drop-compatible 并只审计键名 | 同左 | `CP-REQ-016` |
| sampling/`max_*` | ChatGPT Codex 不支持时 drop-compatible | drop-compatible | `CP-REQ-017` |
| `stream_options` | 仅保留已验证的 `reasoning_summary_delivery=sequential_cutoff`；其它键 drop-compatible | 删除 | `CP-REQ-028` |
| `metadata/user/safety_identifier` | drop-compatible | drop-compatible | `CP-REQ-018` |
| `truncation/prompt_cache_options` | drop-compatible | drop-compatible | `CP-REQ-027` |
| `service_tier` | 仅 `priority` pass；其它值 drop-compatible | 同左 | `CP-REQ-027` |
| 图片/file/computer use | 原生 `input_image` pass；file/computer/image-generation bridge reject | 默认 reject | `CP-REQ-019` |

`CP-REQ-020` system message 必须无损提升到 `instructions`。只有能证明文本语义已完整保留的转换入口，才可以从 `input` 删除被提升项；原生 Responses 默认保留为 developer message。

`CP-REQ-021` HTTP 完整历史中的工具调用与 tool output 必须成对；原生持久 WS 的后续 turn 可以只包含引用上一 turn pending call 的 output。两种路径都不能删除或改写续链需要的 `call_id`、reference 或 encrypted reasoning。

`CP-REQ-022` 请求变换必须发生在账号选择和访问上游之前。确定性客户端错误不得消耗账号或触发 failover。

`CP-REQ-023` 普通 Responses/WS 必须保留 `function_call`、`custom_tool_call`、`mcp_tool_call` 上的 `namespace`；compact 必须删除直接历史输入项上的 `namespace`，不能删除调用项本身或改变顺序。

`CP-REQ-024` `custom_tool_call_output` 与对应 custom call 使用同一 `call_id`；代理不得把 custom call ID 改写为 function 的 `fc_` 命名空间。

`CP-REQ-025` Responses Lite 必须由精确为 true 的内部 header 或对应 `client_metadata` 信号识别。其 `reasoning.context` 必须固定为 `all_turns`；顶层 `namespace` 工具必须迁移到 `input.additional_tools`，按 `type + name` 去重，相同身份的冲突定义必须在访问账号前拒绝。

`CP-REQ-026` 带 `compaction_trigger` 和 `remote_compaction_v2` beta 的请求仍是普通 `/responses` 流式请求，不得提升为 `/responses/compact`，不得删除 trigger 或改变 input 顺序。

`CP-REQ-027` ChatGPT Codex 明确不支持但删除后仍保持标准执行语义的 `truncation`、`prompt_cache_options` 和非 `priority` `service_tier` 必须在账号选择前 drop-compatible；字段名进入有界 ignored-features 记录，字段值不得记录。

`CP-REQ-028` 普通流式 Responses 只允许 `stream_options.reasoning_summary_delivery` 的已验证值 `sequential_cutoff` 进入上游；`include_usage` 等其它键删除。未知值或错误类型必须在账号选择前拒绝，不能静默改变 reasoning summary 的事件交付语义。

## 6. 上游身份与 Header 合同

| Header | 策略 | 规则 |
| --- | --- | --- |
| `Authorization` | generate：只来自选中账号 | `CP-HDR-001` |
| `ChatGPT-Account-ID` | generate：只来自选中账号 | `CP-HDR-002` |
| `User-Agent` | generate：来自版本 profile | `CP-HDR-003` |
| `Originator` | generate：默认 `codex-tui` | `CP-HDR-004` |
| `Accept` | generate：HTTP Responses SSE，compact JSON | `CP-HDR-005` |
| `OpenAI-Beta` | generate/merge allowlist：WebSocket beta | `CP-HDR-006` |
| `Session-Id` / `session_id` | normalize：由 session owner 生成 | `CP-HDR-007` |
| `Thread-Id` | normalize：与 session 一致 | `CP-HDR-008` |
| `X-Client-Request-Id` | normalize：每次请求或 profile 指定 | `CP-HDR-009` |
| `X-Codex-Window-Id` | normalize：绑定 session/window | `CP-HDR-010` |
| `X-Codex-Turn-Metadata` | drop-compatible；无独立 turn metadata owner | `CP-HDR-011` |
| `X-Codex-Turn-State` | drop-compatible；无独立 WS state 合同 | `CP-HDR-012` |
| `X-Codex-Beta-Features` | allowlist normalize；当前只接受并生成 `remote_compaction_v2` | `CP-HDR-013` |
| `Version` | drop-compatible；身份只由 profile 生成 | `CP-HDR-014` |
| `X-OpenAI-Internal-Codex-Responses-Lite` | normalize；仅精确 true 时生成，且不开放内部图片桥接 | `CP-HDR-015` |

`CP-HDR-016` 下游 `Authorization`、Cookie、任意 forwarded header、任意 account ID 和自定义代理 header 绝不能进入上游。

`CP-HDR-017` header 名大小写只在已验证为上游协议组成部分时保留；内部比较必须大小写不敏感，输出必须由 transport profile 决定。

`CP-HDR-018` token、完整 account ID、原始 turn metadata 和 session 原值不得写入日志、归档、指标或错误响应。

## 7. HTTP、SSE、compact 与 WebSocket

`CP-STREAM-001` 非流式下游请求仍可使用上游 SSE；接受 `response.completed` 或 `response.incomplete` 中的 Response 对象作为合法结果。incomplete 必须保留 partial output、usage、status 和 incomplete_details，不得切换账号。

`CP-STREAM-002` 流式成功必须观察到合法 terminal event。仅 EOF、仅 delta 或缺失 terminal event 都是 `upstream_truncated`/protocol error。

`CP-STREAM-003` 一旦向下游写出任一业务 SSE/WS 事件，不得切换账号或重放请求。

`CP-STREAM-004` SSE 允许的 terminal 是 `response.completed`、`response.incomplete`、`response.failed`。错误帧不能伪装成成功 completed。

`CP-STREAM-005` 单个 SSE data 行或 WebSocket message 中若包含 2..16 个、总计不超过 16 MiB 的完整 Responses JSON 文档，必须按原顺序拆成独立事件。其它非法 JSON 必须进入协议错误路径，不能猜测修复。

`CP-STREAM-006` 只有在当前 turn 已观察到语义 output、usage 或明确 error 时，`response.completed`/`response.done` 才能判成功。仅有前导事件和空 completed 是 silent refusal：HTTP 非流式和尚未向客户端提交业务事件的 SSE 必须允许切换账号；原生 WebSocket 必须返回明确失败，不得把空 turn 记为成功。

`CP-STREAM-007` `response.incomplete` 是有界生成、内容过滤等原因形成的合法非成功完成状态，不是账号或 transport 失败。原生 Responses 必须原样保留；Chat/Messages adapter 必须映射对应 finish/stop reason。Chat adapter 的 `max_tokens`/`max_output_tokens` 映射为 `length`，`content_filter` 映射为 `content_filter`，不得把所有 incomplete 原因统一伪装成长度终止。

`CP-STREAM-008` created/in_progress、空 delta、空 output/tool 骨架和可重试 error 不算已向客户端产生业务输出。HTTP 200 后、首个真实业务输出前收到 usage limit、capacity、认证、限流或 transport terminal error 时，必须先分类并允许按失败规则切换账号；已有真实输出时只能转发安全终态，禁止重放。

`CP-STREAM-009` 普通 Responses SSE 不得仅因上游返回 HTTP 200 就提交下游 200；必须等首个实际转发事件。账号在输出前全部失败时返回真实 HTTP error；已提交后发生无 terminal 的 transport/protocol failure 时必须合成一个有界 `response.failed`。业务 terminal 的成败只由 codexupstream typed result 裁决，HTTP emit 回调不得把 incomplete/failed 重分类为 client write。

`CP-STREAM-010` 网关已经无法执行 failover、必须向客户端转发上游 capacity 降载事件时，写给客户端的 `error`/`response.failed` 副本必须把 `server_is_overloaded` 和 `slow_down` code 投影为可重试的 `server_error`。账号分类、额度观察和审计必须继续使用未改写的原始事件；`rate_limit_exceeded` 等其它错误码不得改写。HTTP/SSE 与 WebSocket 必须一致。

`CP-COMPACT-001` compact 上游使用 `/backend-api/codex/responses/compact`，请求不得带普通 Responses 的 `stream`、`store` 或不受支持 `tool_choice`。

`CP-COMPACT-002` 客户端要求流式 compact 时，AetherRelay 必须把 unary JSON 合成为最小合法事件序列：每个 output item 一个 `response.output_item.done`，最后是 `response.completed`。

`CP-COMPACT-003` compact 等待期间可以发送 SSE comment heartbeat；heartbeat 提交 HTTP 200 后，上游失败必须用 `response.failed` 终止，不能混写 JSON。

`CP-WS-001` WebSocket 握手必须执行与 HTTP 相同的客户端认证、Provider access 和模型授权。

`CP-WS-002` 上游连接使用已验证的 `OpenAI-Beta: responses_websockets=<profile-date>`，每个 turn 使用 `response.create`。

`CP-WS-003` 同一 execution session 可以复用上游连接；不同客户端身份、账号或 session 不能共享连接。

`CP-WS-004` 连接池、reader、writer、ping/pong 和关闭任务必须归 `codexupstream` Block 生命周期所有；长期任务使用 framework 注入的 `BackgroundRoutine`，Teardown 必须取消并等待退出。

`CP-WS-005` compact 不使用上游 WebSocket。WebSocket 到 HTTP 的自动降级不属于当前合同；握手或上游连接失败必须返回明确错误，不能把客户端请求静默改成另一种 transport。

`CP-WS-006` WebSocket 单帧、单消息、session 数、空闲时间和存活时间必须有配置上限。

`CP-WS-007` 同一上游连接的后续 `response.create` 必须支持 `previous_response_id + incremental input`。代理不得要求本 turn 的 `function_call_output`、`custom_tool_call_output` 或 `mcp_tool_call_output` 在同一 input 数组中重复其 call。

`CP-WS-008` 客户端在本地 compact 后提交完整替换 transcript 时，代理必须保序转发该 transcript，不得与旧 turn 历史合并或注入旧 `previous_response_id`。

`CP-WS-009` 上游 `response.done` 是成功终态；向标准 Responses 客户端转发前必须规范为 `response.completed`。`response.cancelled`/`response.canceled` 是失败终态，不得等待到连接超时。

`CP-WS-010` 每个 WebSocket terminal 必须携带与 HTTP/SSE 相同的有界错误分类、quota/reset observation 和 turn outcome。`response.failed`、`error`、transport/protocol failure 后必须失效当前上游连接；`response.incomplete` 保持合法终态并允许连接继续复用。

## 8. 账号调度与会话粘性

`CP-SCHED-001` 调度顺序固定为：客户端 Provider access → exact model 能力 → 显式状态 → token 健康 → quota/cooldown → 并发槽 → session 粘性 → priority → LRU/round-robin。

`CP-SCHED-002` session 信号按优先级解析：标准化 session header、`conversation_id`、OpenCode/CodeBuddy 会话头、`prompt_cache_key`、WebSocket execution session。无显式信号时可以生成请求域 session，但不能用完整敏感正文作为持久化 key。

`CP-SCHED-003` session key 必须按客户端 API key ID 和 model 命名空间隔离；存储哈希，不保存原值。

`CP-SCHED-004` 粘性账号不健康、不支持模型、额度耗尽或没有并发槽时可以解除绑定并重新选择；已产生输出的 turn 除外。

`CP-SCHED-005` 每账号并发槽必须覆盖 HTTP/SSE/WS turn 的完整上游生命周期，并在取消、错误和 shutdown 时释放。

`CP-SCHED-006` 账号选择结果和凭据只通过 typed EventHub command/result 跨 Block；HTTP handler 不接收 EventHub、Store 或 OAuth token。

`CP-SCHED-007` 显式替换账号凭据时，模型、额度、compact 和 WebSocket 能力快照都必须失效为 unknown，禁止让新凭据继承旧上游身份的 transport 判定。

`CP-SCHED-008` 成功 Responses/compact 握手返回的 primary/secondary used、window 和 reset header 必须投影为有界账号 usage 快照；非法或缺失 header 不得覆盖独立 usage 查询的有效字段。

## 9. Refresh、错误与 Failover

| 失败 | 同账号重试 | 切账号 | 规则 |
| --- | --- | --- | --- |
| 本地校验/转换失败 | 否 | 否 | `CP-FAIL-001` |
| 上游 400 | 否；仅允许一次有证据的 rejected-field 兼容修复 | 否 | `CP-FAIL-002` |
| 401 | 单飞 refresh 后一次 | refresh 永久失败或再次 401 | `CP-FAIL-003` |
| 403 | 否 | 未输出时是 | `CP-FAIL-004` |
| 408/连接/首事件超时 | 否 | 未输出且无已知副作用时是 | `CP-FAIL-005` |
| 429/usage limit | 否 | 未输出时是，并记录 reset | `CP-FAIL-006` |
| 5xx | 否 | 未输出且无已知副作用时是 | `CP-FAIL-007` |
| SSE/WS terminal failed | 否 | 仅首个真实业务输出前按具体错误类别决定 | `CP-FAIL-008` |
| 客户端取消 | 否 | 否 | `CP-FAIL-009` |

`CP-FAIL-010` 必须保留最后一个真实上游失败的安全 HTTP 状态和错误类别，不能在账号耗尽后统一覆盖为 `provider_unavailable`。

`CP-FAIL-011` refresh 必须区分过期、撤销、重复使用和临时错误；HTTP 400 不能一律标记永久失效。

`CP-FAIL-012` 同账号 refresh 使用 singleflight。成功 rotation 必须原子更新 access/refresh/id token 和 expires；失败不能破坏最近可用 access token。

`CP-FAIL-013` 确定性 upstream 400 必须保持 HTTP 400 且不得切换账号。代理只允许从结构化 JSON 错误中投影经过长度限制和脱敏的 `type`、`code`、`param`、`message`；原始正文、未知字段和凭据形态不得跨 `codexupstream` Block。

`CP-FAIL-014` 原生 WebSocket 的账号结果按 turn terminal 登记，不按 socket close 登记。合法 completed/done/incomplete 记合法完成；failed/cancelled、transport/protocol error 记对应失败并保留 quota observation；客户端在 terminal 前关闭只释放 lease，不得伪造成功或清除 quota observation。

## 10. 模型与能力目录

`CP-CAP-001` 模型来自账号级 `/backend-api/codex/models` 快照；可路由目录是健康账号能力并集，但账号选择仍按账号自身快照过滤。

`CP-CAP-002` 能力至少区分：Responses、compact、WebSocket、function tools、parallel tools、image input、image generation、reasoning efforts。

`CP-CAP-003` 未探测能力使用 `unknown`，不能当作 `supported` 对外宣称；请求期可按明确的兼容策略尝试一次并缓存结果。

`CP-CAP-004` `/v1/models` 与请求路由必须读取同一 effective catalog generation，不得分别拼接目录。

## 11. 安全、资源与可观测性

`CP-SEC-001` OAuth 凭据继续由 Codex account owner 加密保存；不得进入配置 YAML、普通 DuckDB 表、请求归档或浏览器存储。

`CP-SEC-002` 账号代理用于 OAuth、refresh、模型、用量、HTTP Responses、compact 和 WebSocket，保证同一账号出口策略一致。

`CP-SEC-003` 请求体、SSE 行、响应体、WS frame、WS message、连接数、账号并发、idle timeout 都必须有硬上限。

`CP-OBS-001` 用量统一记录客户端身份、模型、上游协议、transport（HTTP/SSE/WS/compact）、账号安全引用、是否估算和最终 outcome。

`CP-OBS-002` outcome 至少包括 `success`、`client_canceled`、`invalid_request`、`authentication_failed`、`rate_limited`、`quota_exhausted`、`upstream_failed`、`upstream_truncated`、`idle_timeout`、`protocol_error`。

`CP-OBS-003` 指标和日志只记录有界错误类别，不记录上游正文、token、代理凭据、原始 session 或完整 account ID。

## 12. 运行时与组件边界

`CP-ARCH-001` 进程只使用 `framework/application` 创建的一套 EventHub 和 BackgroundRoutine。

`CP-ARCH-002` `codexupstream` Block 拥有网络 transport、HTTP/SSE/WS 连接和其运行态；`codexaccountpool` Block 拥有凭据、账号状态、调度、粘性和并发槽；`proxyapi` Application Module 拥有入站协议、转换、用量和错误 envelope。

`CP-ARCH-003` Block 间通信只使用各投递组件 `pkg/events` 中的具体 command/result。需要结果的命令使用同步 Send 并检查缺失、错误和类型；不能注入 repository、adapter 或回调绕过 EventHub。

`CP-ARCH-004` 长期 reader、连接维护和 timer 必须有取消 context，并在 Block Teardown 中先拒绝新工作、取消连接、取消订阅；随后由 application 依次关闭 service、BackgroundRoutine 和 EventHub。

`CP-ARCH-005` observer 不执行无限网络读取。网络读取必须提交到注入的 BackgroundRoutine；shutdown 后不得继续提交任务。

## 13. 验收与发布门禁

`CP-DOD-001` 必须维护脱敏 golden corpus，至少覆盖：HTTP 非流式、SSE、function tools、tool continuation、reasoning、compact unary/SSE、WS 首 turn/续 turn、取消、401、403、429、5xx、断流。

`CP-DOD-002` 对同一 corpus 比较 AetherRelay、目标 Codex 直连以及参考实现的上游请求和下游事件；差异必须被合同允许。

`CP-DOD-003` 每个端点必须测试认证、Provider access、body limit、模型不存在、客户端取消、归档脱敏和用量完成。

`CP-DOD-004` 每个可重试错误必须测试“输出前允许”和“输出后禁止”两个边界。

`CP-DOD-005` 发布前必须通过：相关分包测试、`go test ./...`、`go vet ./...`、格式检查和凭据泄漏扫描。

`CP-DOD-006` 真实账号 smoke test 只能由显式运维命令触发，不能在单元测试或服务启动时访问上游。

## 14. 实施追踪矩阵

状态取值：`implemented`、`in_progress`、`planned`、`blocked`。只有代码和测试证据同时存在才能标记 `implemented`。

| 能力 | 规则 | 状态 | 实现证据 | 测试证据 |
| --- | --- | --- | --- | --- |
| Responses HTTP/SSE | CP-EP-001, CP-STREAM-001..009 | implemented | `codexupstream/biz/biz.go` | `codex_responses_test.go`, `codexupstream/biz/biz_test.go` |
| OAuth refresh/429 切换 | CP-FAIL-003, CP-FAIL-006 | implemented | `proxyapi/biz/codex_responses.go` | `proxyapi/biz/codex_responses_test.go` |
| 核心端点 | CP-EP-001..003, CP-EP-013 | implemented | `proxy/routes.go`, `proxy/handler.go`, `proxy/models.go` | `codex_responses_test.go`, `codex_websocket_test.go`, `models_test.go` |
| 历史端点拒绝 | CP-EP-004..006, CP-EP-011..012 | implemented | `proxy/routes.go`, `proxy/handler.go` | `models_test.go` |
| 请求兼容层 | CP-REQ-001..028 | implemented | `proxy/codex_compat.go` | `codex_responses_test.go`, `codex_normalization_golden.json` |
| 版本化身份/header | CP-CLIENT-002..004, CP-HDR-* | implemented | `codexupstream/biz/identity.go` | `codexupstream/biz/biz_test.go` |
| compact | CP-EP-003, CP-COMPACT-* | implemented | `proxy/codex_responses.go`, `codexupstream/biz/biz.go` | `codex_responses_test.go`, `biz_test.go` |
| session 粘性与并发槽 | CP-SCHED-* | implemented | `codexaccountpool/biz/biz.go` | `codexaccountpool/biz/biz_test.go`, `proxyapi/biz/codex_responses_test.go` |
| 扩展 failover | CP-FAIL-004..014 | implemented | `proxyapi/biz/codex_responses.go` | `proxyapi/biz/codex_responses_test.go` |
| Responses WebSocket | CP-EP-002, CP-WS-001..010 | implemented | `proxy/codex_websocket.go`, `codexupstream/biz/biz.go` | `codex_websocket_test.go`, `codexupstream/biz/biz_test.go` |
| Chat/Messages 转 Codex | CP-EP-007..008 | implemented | `proxy/codex_chat.go`, `proxy/codex_messages.go` | `codex_responses_test.go`, `models_test.go` |
| 离线规范化 corpus | CP-DOD-001 | implemented | `proxy/testdata/codex_normalization_golden.json` | `TestCodexNormalizationGoldenCorpus` |
| 真实上游差分 corpus | CP-DOD-002 | planned | - | - |

## 15. 已知基线差异

- AetherRelay 已使用本机核对的 Codex CLI `0.147.0` 版本 profile；内部 Codex header 不接受客户端透传，新增 header 必须先建立独立能力合同。
- HTTP/SSE、compact 与 WebSocket 已有主链路；custom/namespace/parallel 工具、原生图片输入与 compact namespace 历史清理已纳入离线合同。图片生成/Images API bridge 仍不属于 Codex core。真实账号/参考实现差分仍需显式运维执行。
- `/v1/models` 与请求路由读取同一 effective catalog generation；不提供 Codex OAuth 专用入站模型别名。
- Chat Completions 与 Anthropic Messages 已通过独立适配器转入 Codex Responses；支持范围以 `CP-EP-007..008` 测试和 fail-closed 字段校验为准。

## 16. 兼容证据记录

| 端点/能力 | Git 证据 | 判定 |
| --- | --- | --- |
| `POST /v1/responses`、`GET /v1/responses` | CLIProxyAPI `f43aad76` 的 `internal/api/server_routes.go`；sub2api `0e82efe48` 的生产路由 | Codex core，必须保留 |
| `POST /v1/responses/compact` | CLIProxyAPI `95096bc3` 首次加入；sub2api `2fb212b7`、`a56eb5b4`、`84bb7d07` 持续修复原生 compact 链路 | 当前工作流能力，必须保留 |
| `/backend-api/codex/responses*` | CLIProxyAPI `f43aad76` 注释为 `chatgpt_base_url compatible` direct aliases；sub2api `0e82efe48` 同样注册；最新 OpenAI 配置参考明确 `chatgpt_base_url` 只覆盖登录流程，模型请求使用 `model_providers.<id>.base_url` | 仅历史参考，不提供入站兼容 |
| `GET /v1/models`、`GET /backend-api/codex/models` | sub2api `13e773ef` 引入 Codex manifest 透传，`806bb230` 增加根 alias；最新自定义 Provider 只需要 base URL 下的 `/models` | 前者 core，后者不提供入站兼容 |
| `GET /v1/responses/ws` | CLIProxyAPI `f43aad76` 仅在 SDK WebSocket 测试中自行注册；生产路由未注册，sub2api 生产路由也未注册 | 测试路径，不是生产兼容合同，拒绝 |
| `POST /backend-api/codex/models` | CLIProxyAPI `f43aad76` 与 sub2api `0e82efe48` 均无生产路由 | 无历史依据，拒绝 |
| `POST /v1/models` | AetherRelay `2260888c` 已存在；参考实现与 Codex 客户端没有该 method 依据 | AetherRelay 通用历史兼容，不计入 Codex 合同 |
| `POST /v1/chat/completions`、`POST /v1/messages` | AetherRelay `2260888c` 已存在，Codex 补全只新增协议转换 | AetherRelay adapter，不是 Codex 官方或历史直连端点 |
| `prompt_cache_options`、`truncation`、`service_tier` 清洗 | CLIProxyAPI `2ab25eae` 明确记录 ChatGPT Codex 不支持 `prompt_cache_options`；`f43aad76` 的 Responses translator 同时删除 `truncation`，只保留 `priority` tier | 删除后不改变标准执行语义，按 `CP-REQ-027` drop-compatible |
| 空 `response.completed` | sub2api `280c1c862` 的脱敏样本为 completed + empty output、无 usage/error，真实结果是 silent refusal | 按 `CP-STREAM-006` 在未提交业务输出前失败并允许 HTTP/SSE 切号；WS 返回明确失败 |
| 确定性 upstream 400 | sub2api `591d47fb9` 覆盖 `invalid_function_parameters`、`missing_required_parameter` 等结构化 400 | 保持 400、不切号；只投影 `CP-FAIL-013` 允许的有界安全字段 |

- `CP-WS-002` profile 来源：CLIProxyAPI `f43aad76` 的 `internal/runtime/executor/codex_websockets_connection.go`，验证 beta 值 `responses_websockets=2026-02-06`；测试只使用脱敏本地 WebSocket server。
- 最新配置判定来源：OpenAI Docs `https://developers.openai.com/codex/config-reference/`，其中 `model_providers.<id>.base_url` 定义为模型 Provider API base URL，`chatgpt_base_url` 定义为 ChatGPT login flow base URL override。
