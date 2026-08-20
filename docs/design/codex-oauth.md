# Codex OAuth 账号池设计

功能域：Codex CLI OAuth 账号池、模型发现、原生 Responses 代理与额度观察。对应正式合同见[配置参考](../configuration.md)与[功能说明](../features.md)。

## 设计目标

- 复用 Codex CLI 登录的 OAuth 账号，提供原生 Responses HTTP/SSE、compact 与 WebSocket 能力，无需上游 API Key。
- 与 ChatGPT Web 是两个**独立账号域**：不共享 refresh token、账号代理、模型发现、网页会话或临时对话。
- 账号凭据、代理与到期时间只存 `state.database`；管理 API 直接显示邮箱，但不返回 token、account ID 或代理。

## 账号池与模型发现

- 进程始终注入只读内建 Provider `codexoauth`；`codexaccountpool` 是单一凭据与模型快照 owner（安全文档 scope `codex_oauth_accounts`）。
- 模型按账号从 ChatGPT 上游 `GET /backend-api/codex/models` 自动发现，快照 6 小时有效；该路径不作为 AetherRelay 入站端点。失败按账号独立指数退避（30 秒 ~ 5 分钟），仅持有有效快照的账号可被调度；可路由模型是全部健康账号快照的并集，不提供 allowlist 筛选项。
- 每账号代理同时用于授权码换令牌、refresh、模型发现、用量读取与 Responses 请求，保证出口 IP 一致；管理读模型从不返回代理值。
- OAuth 新增与重新认证使用同一 owner 合同但语义分流：账号行发起时 session 绑定稳定本地 ID并复用既有代理；通用入口按精确上游 `account_id`（缺失时仅按唯一邮箱）收敛轮换凭据。不同 `account_id` 的同邮箱 workspace 不合并，目标身份冲突明确失败。
- 导入凭据、刷新凭据或完成 OAuth 后立即提交一次模型同步和用量刷新；管理页的「刷新凭据」表示强制续期 OAuth 凭据，成功后自动同步模型与上游用量。「同步模型」和「刷新用量」仍可独立执行并轮询有界进度任务（当前进程保留 30 分钟，持久化快照才是重启后权威）。
- 自动触发的模型同步与用量刷新由管理页静默轮询，避免一次账号操作重复显示两条完成通知；只有用户手工启动的同步任务显示进度，成功完成信息自动隐藏，轮询错误保持可见。
- 管理页可按稳定本地 ID 导出选中账号的完整凭据；导出固定返回可直接作为 `accounts` 重新导入的 JSON 数组，响应 `Cache-Control: no-store`，不进入列表、日志或浏览器持久化存储。该交互与 ChatGPT Web 账号导入/导出一致。

## 原生 Responses 代理

- `codexoauth` 服务原生 `POST /v1/responses`、`POST /v1/responses/compact` 与 Responses WebSocket；请求与事件不经过 ChatGPT Web 消息树转换；`/v1/chat/completions` 和 `/v1/messages` 可通过受限协议适配路由到该 Provider。
- `codexoauth` 是只读内建 Provider，上游 Responses、模型发现和用量端点由实现固定，不参与管理型 Provider 的 protocol/base URL/endpoints 切换；如需切换上游接入端点，必须使用独立的管理型直连 Provider。
- 非流式请求在内部要求上游 SSE；优先返回 `response.completed` 的原始 Response 对象，也可从完整 `output_item.done` 重建标准文本和 function-call output。上游若返回原生 JSON Response 也接受。
- 流式响应透传标准 Responses 事件。若工具调用已收到完整 `function_call_arguments.done` 和 `response.output_item.done`，随后 clean EOF 可作为成功结束；代理不会伪造缺失的 `response.completed` data。
- `/v1/responses/compact` 的客户端形态仍保留，但 OAuth 上游统一改写为 streaming `/backend-api/codex/responses`：强制 `stream=true`、`store=false`，并在 input 末尾放置唯一 `compaction_trigger`。只有响应中确实出现 `compaction`/`compaction_summary` item 才学习为支持；旧 unary compact 端点的能力缓存会自动失效。
- `X-Codex-Beta-Features` 是会话 profile：客户端未声明时默认 `remote_compaction_v2`，显式非空集合保持原样，原生压缩请求则强制包含 v2。`X-Codex-Turn-State` 作为有界 opaque 值转发和回传，只记录其哈希对应的铸造账号；已知跨账号回放在 failover 出站前剥离。
- 账号指纹收敛默认关闭。管理员可逐账号显式选择 `device`、`session` 或 `full`；HTTP、SSE、compact 与 WebSocket 共用同一类型化 profile，header 与 `client_metadata` 使用同一组 ID，切换到 `off` 账号时不会继承上一 attempt 的身份。
- WebSocket 支持规范入口 `GET /v1/responses`；`GET /v1/responses/ws` 仅见于参考实现的 SDK 测试，不作为生产兼容入口。Realtime、网页会话或插件能力不在本合同范围。

## 账号韧性

- 上游 `401`：按本地账号 ID 单飞 refresh，成功且尚未向客户端写出时仅重试一次；刷新永久失败或第二次仍被拒绝时账号标为异常。
- `429` / 瞬态错误：写模型级冷却（`Retry-After` 上限 3600 秒），尚未写出 SSE 时改用未尝试账号；上游已开始 SSE 输出后不切换账号，避免拼接两个不同响应。
- **额度观察**：仅上游明确 `usage_limit_reached` 才记录账号/模型级额度耗尽与上游提供的恢复时间，并驱动模型冷却；普通 429 不伪装为套餐额度。
- 用量窗口（`GET /backend-api/wham/usage`）只保存计划类型、`used_percent`、窗口长度、恢复时间与 `allowed` / `limit_reached`，快照 15 分钟过期、刷新失败保留最后成功值；它是额度窗口观测，不改变模型路由或冷却。
- `refresh_account_interval_minute: 0` 关闭临期刷新；正数只刷新有可解析到期时间且将在 5 分钟内失效的正常账号。没有到期元数据的导入凭据仍可在实际 401 时刷新。
- 管理型 Provider 与 Codex 自动模型同名时都进入候选链：管理型默认 `priority=100`，Codex 默认 `90`，可在安全的原生 Responses 失败场景回退。

## 演进记录

- 2026-08-17：对齐 sub2api 的 native remote compaction v2、会话 beta、Turn-State 来源守卫和显式 opt-in 指纹收敛。
- 2026-07-30：Codex OAuth 账号池收口设计 → 归档 `docs/archive/codex-oauth-account-pool-design-2026-07-30.md`
