# Codex OAuth 账号池设计

功能域：Codex CLI OAuth 账号池、模型发现、原生 Responses 代理与额度观察。对应正式合同见[配置参考](../configuration.md)与[功能说明](../features.md)；Installation、Session、Thread、Window、Turn、fingerprint 与 Turn-State scope 的术语和维护门槛以 [Codex 身份与会话语义基准](codex-identity-semantics.md) 为准。

## 设计目标

- 复用 Codex CLI 登录的 OAuth 账号，提供原生 Responses HTTP/SSE、compact 与 WebSocket 能力，无需上游 API Key。
- 与 ChatGPT Web 是两个**独立账号域**：不共享 refresh token、账号代理、模型发现、网页会话或临时对话。
- 账号凭据、代理与到期时间只存 `state.database`；管理 API 直接显示邮箱，但不返回 token、account ID 或代理。

## 账号池与模型发现

- 进程始终注入只读内建 Provider `codexoauth`；`codexaccountpool` 是单一凭据与模型快照 owner（安全文档 scope `codex_oauth_accounts`）。
- 模型按账号从 ChatGPT 上游 `GET /backend-api/codex/models` 自动发现，快照 6 小时有效；该路径不作为 AetherRelay 入站端点。失败按账号独立指数退避（30 秒 ~ 5 分钟），仅持有有效快照的账号可被调度；可路由模型是全部健康账号快照的并集，不提供 allowlist 筛选项。
- 每账号代理同时用于授权码换令牌、refresh、模型发现、用量读取与 Responses 请求，保证出口 IP 一致；管理读模型从不返回代理值。`scoped` 账号从实际选中它的推理请求中原子观察 `User-Agent`/`Originator`，按验证过的 family 优先级和版本单调选择账号 profile；推理、refresh、模型发现与用量读取共用选择结果，尚无有效观察值时回落内置 profile。授权码交换没有已选账号，始终使用内置 profile；`off` 只在推理路径逐请求复用两个字段都存在且安全的完整客户端组合，任一缺失或非法则整组回落。非 Codex family 不具 scoped 候选资格，但不因此拒绝业务请求。credential endpoint 不发送 inference-only `Version`，模型发现的 `client_version` 与已选 profile 对齐。客户端候选只影响所选账号的上游自述，不参与账号选择、凭据或其它 header 策略。
- OAuth 新增与重新认证使用同一 owner 合同但语义分流：账号行发起时 session 绑定稳定本地 ID并复用既有代理；通用入口按精确上游 `account_id`（缺失时仅按唯一邮箱）收敛轮换凭据。不同 `account_id` 的同邮箱 workspace 不合并，目标身份冲突明确失败。
- 导入凭据、刷新凭据或完成 OAuth 后立即提交一次模型同步和用量刷新；管理页的「刷新凭据」表示强制续期 OAuth 凭据，成功后自动同步模型与上游用量。「同步模型」和「刷新用量」仍可独立执行并轮询有界进度任务（当前进程保留 30 分钟，持久化快照才是重启后权威）。进程启动必须先用持久化账号快照同步构建首个有效目录，再开放 HTTP 路由；上游重新发现继续异步执行，不能在此期间用空目录覆盖仍有效的持久化模型。
- 自动触发的模型同步与用量刷新由管理页静默轮询，避免一次账号操作重复显示两条完成通知；只有用户手工启动的同步任务显示进度，成功完成信息自动隐藏，轮询错误保持可见。
- 管理页可按稳定本地 ID 导出选中账号的完整凭据；导出固定返回可直接作为 `accounts` 重新导入的 JSON 数组，响应 `Cache-Control: no-store`，不进入列表、日志或浏览器持久化存储。该交互与 ChatGPT Web 账号导入/导出一致。

## 原生 Responses 代理

- `codexoauth` 服务原生 `POST /v1/responses`、`POST /v1/responses/compact` 与 Responses WebSocket；请求与事件不经过 ChatGPT Web 消息树转换；`/v1/chat/completions` 和 `/v1/messages` 可通过受限协议适配路由到该 Provider。`POST /v1/responses/input_tokens` 只复用 Responses 的认证与有效目录，本地估算后直接返回，不选择 Codex 账号或访问上游。
- `codexoauth` 是只读内建 Provider，上游 Responses、模型发现和用量端点由实现固定，不参与管理型 Provider 的 protocol/base URL/endpoints 切换；如需切换上游接入端点，必须使用独立的管理型直连 Provider。
- 非流式请求在内部要求上游 SSE；优先返回 `response.completed` 的原始 Response 对象，也可从完整 `output_item.done` 重建标准文本和 function-call output。上游若返回原生 JSON Response 也接受。
- 流式响应透传标准 Responses 事件。若工具调用已收到完整 `function_call_arguments.done` 和 `response.output_item.done`，随后 clean EOF 可作为成功结束；代理不会伪造缺失的 `response.completed` data。
- `/v1/responses/compact` 的客户端形态仍保留，但 OAuth 上游统一改写为 streaming `/backend-api/codex/responses`：强制 `stream=true`、`store=false`，并在 input 末尾放置唯一 `compaction_trigger`。只有响应中确实出现 `compaction`/`compaction_summary` item 才学习为支持；账号文档只接受当前 `remote_compaction_v2` 标记，不在加载阶段升级旧缓存。
- compact 的 400/404/405/409/413/422/501 request fault 停止 fallback；其它非 credential 临时失败可以继续切号，但只累计失败观测，不写普通 Responses 冷却、账号异常或额度事实。401/402/结构化 403/429 保留原账号反馈，HTML endpoint 403 仍按端点故障处理。
- `X-Codex-Beta-Features` 是会话 profile：客户端未声明时默认 `remote_compaction_v2`，显式非空集合保持原样，原生压缩请求则强制包含 v2。`X-Codex-Turn-State` 作为有界 opaque 值转发和回传，只记录其哈希对应的铸造账号；已知跨账号回放在 failover 出站前剥离。
- `X-Codex-Turn-State` 另有会话级记忆（`CP-HDR-022`/`CP-HDR-023`）：按「铸造账号 + 客户端声明的会话」在进程内保存最近观测值，客户端未提供该 header 时回填该值，没有任何观测时使用 `codex_oauth.default_turn_state`，该值也为空时不发送。声明会话按显式会话 header、`client_metadata.session_id`/`thread_id`、显式 `prompt_cache_key` 依次取值；完全没有声明时不记录也不回放，并以服务端请求级 nonce 生成一次性调度 Session 与默认 cache identity。客户端提供但被判定为其它账号铸造而剥离时保持为空，失败换号后也不沿用上一账号的记录；`/v1/chat/completions` 与 `/v1/messages` 适配入口同样解析并传递客户端值。记录不落盘、不设过期、受条数上限与字节预算约束，`codex_oauth.turn_state_fallback: false` 可只停回填。诊断以 `turn_state_source`（`client/session/default/absent`）与归档 `turn_state_fallback` 三态布尔表达来源，原值不进入日志、归档、指标或管理视图。
- 账号指纹模式只允许 `off/scoped`，默认 `scoped`；`off` 是显式排障开关。`scoped` 使用加密账号文档内的系统随机 seed，而不是本地账号 ID；账号级 Installation、账号与 LogicalConversation 共同派生的 Session、以及账号/会话/LogicalThread 共同派生的 Thread保证不同下游会话不会因收敛合并。
- HTTP、SSE、compact 与 WebSocket 握手共用同一请求级语义胶囊和 attempt 投影。账号切换只替换账号物理投影，LogicalTurn、Window Number、cache identity、正文和合法 turn 属性保持不变；header 与 `client_metadata` 使用同一组 installation/session/thread/turn/window。客户端显式或按客户端会话生成的 `prompt_cache_key` 不随账号指纹改写。
- 官方 CLI 的 Turn-Metadata `compaction` 作为有界结构化属性保留到上游 header 与 body 内嵌 metadata，仅接受已验证的 `reason`/`phase` 枚举；其它嵌套内容继续 fail closed。它提供压缩上下文但不替代正文 `compaction_trigger` 或 `remote_compaction_v2` beta。
- WebSocket 支持规范入口 `GET /v1/responses`；第二个及后续 turn 在客户端尚未收到业务帧且 429 已同步写入旧账号冷却时，可以关闭旧 session、重新选择账号并发送去掉 `previous_response_id` 的完整 transcript。只有 transcript 在消息上限内、顺序完整且 function/custom/MCP tool output 全部能匹配 call 时才允许重放，单 turn 最多迁移两次；任一业务帧写出后禁止迁移。`GET /v1/responses/ws` 仅见于参考实现的 SDK 测试，不作为生产兼容入口。Realtime、网页会话或插件能力不在本合同范围。

## 账号韧性

- 上游 `401`：按本地账号 ID 单飞 refresh，成功且尚未向客户端写出时仅重试一次；刷新永久失败或第二次仍被拒绝时账号标为异常。
- `429` / 瞬态错误：写模型级冷却（`Retry-After` 上限 3600 秒），尚未写出 SSE 时改用未尝试账号；上游已开始 SSE 输出后不切换账号，避免拼接两个不同响应。
- **额度观察**：仅上游明确 `usage_limit_reached` 才记录凭据级额度耗尽与上游提供的恢复时间，并驱动全部模型的单调冷却；普通 429 仍只驱动当前模型冷却，不伪装为套餐额度。
- 用量窗口（`GET /backend-api/wham/usage`）只保存计划类型、`used_percent`、窗口长度、恢复时间与 `allowed` / `limit_reached`。配置正数周期后按账号到期、小批量自动刷新；刷新失败保留最后成功值并退避。它是额度窗口观测，不改变模型路由或冷却。
- `refresh_account_interval_minute: 0` 关闭临期刷新；正数只刷新有可解析到期时间且将在 5 分钟内失效的正常账号。没有到期元数据的导入凭据仍可在实际 401 时刷新。
- 管理型 Provider 与 Codex 自动模型同名时都进入候选链：管理型默认 `priority=100`，Codex 默认 `90`，可在安全的原生 Responses 失败场景回退。

## 演进记录

- 2026-09-20：合同 `12.1.0` 恢复 Turn-Metadata `compaction.reason/phase` 的有界结构化透传，保持 header/body/failover 信息守恒。
- 2026-09-19：合同 `12.0.1` 收口非 Codex/残缺身份的原子回落、scoped fallback、请求级无状态 Session/cache 隔离及有界原因诊断。
- 2026-09-19：合同 `11.0.0` 收口为 `off/scoped`，默认 `scoped`，删除旧模式入口，并冻结 failover 请求级语义胶囊。
- 2026-09-18：合同 `9.0.0` 把推理路径的出站默认身份改为复用下游客户端身份（`User-Agent` / `Originator`，非法值回落 profile，凭据与账号域请求不接收客户端值）。
- 2026-09-18：合同 `8.1.0` 新增 `CP-HDR-022`/`CP-HDR-023` 会话级 Turn-State 记录与缺失回填，含 `codex_oauth.turn_state_fallback` / `default_turn_state` 两个热更新配置与 `turn_state_source` 有界诊断。
- 2026-08-21：对齐近期 CLIProxyAPI/sub2api 的 input-token preflight、nested cache hint、GPT-5.6 双上下文、compact availability-neutral、OAuth identity 与后续 WebSocket turn 安全迁移。
- 2026-08-17：对齐 sub2api 的 native remote compaction v2、会话 beta、Turn-State 来源守卫和显式 opt-in 指纹收敛。
- 2026-07-30：Codex OAuth 账号池收口设计 → 归档 `docs/archive/codex-oauth-account-pool-design-2026-07-30.md`
