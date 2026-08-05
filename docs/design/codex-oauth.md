# Codex OAuth 账号池设计

功能域：Codex CLI OAuth 账号池、模型发现、原生 Responses 代理与额度观察。对应正式合同见[配置参考](../configuration.md)与[功能说明](../features.md)。

## 设计目标

- 复用 Codex CLI 登录的 OAuth 账号，提供原生 `POST /v1/responses` 能力，无需 API Key。
- 与 ChatGPT Web 是两个**独立账号域**：不共享 refresh token、账号代理、模型发现、网页会话或临时对话。
- 账号凭据、代理与到期时间只存 `state.database`；管理 API 严格脱敏，绝不返回 token、account ID 或代理。

## 账号池与模型发现

- 进程始终注入只读内建 Provider `codexoauth`；`codexaccountpool` 是单一凭据与模型快照 owner（安全文档 scope `codex_oauth_accounts`）。
- 模型按账号从 `GET /backend-api/codex/models` 自动发现，快照 6 小时有效；失败按账号独立指数退避（30 秒 ~ 5 分钟），仅持有有效快照的账号可被调度；可路由模型是全部健康账号快照的并集，不提供 allowlist 筛选项。
- 每账号代理同时用于授权码换令牌、refresh、模型发现、用量读取与 Responses 请求，保证出口 IP 一致；管理读模型从不返回代理值。
- 导入凭据、刷新凭据或完成 OAuth 后立即提交一次模型同步；管理页可对选中账号或全部账号执行「同步模型」并轮询进度（有界进度任务，当前进程保留 30 分钟，持久化快照才是重启后权威）。
- 管理页可按稳定本地 ID 导出选中账号的完整凭据；导出固定返回可直接作为 `accounts` 重新导入的 JSON 数组，响应 `Cache-Control: no-store`，不进入列表、日志或浏览器持久化存储。该交互与 ChatGPT Web 账号导入/导出一致。

## 原生 Responses 代理

- `codexoauth` 只服务原生 `POST /v1/responses`（HTTP JSON/SSE P0）；请求与 SSE 事件不经过 ChatGPT Web 消息树转换；`/v1/chat/completions` 不能路由到该 Provider。
- 非流式请求在内部要求上游 SSE，并仅从 `response.completed` 事件返回原始 Response 对象；上游若返回原生 JSON Response 也接受。
- P0 不支持 realtime/WebSocket、`responses/compact`、网页会话或插件能力。

## 账号韧性

- 上游 `401`：按本地账号 ID 单飞 refresh，成功且尚未向客户端写出时仅重试一次；刷新永久失败或第二次仍被拒绝时账号标为异常。
- `429` / 瞬态错误：写模型级冷却（`Retry-After` 上限 3600 秒），尚未写出 SSE 时改用未尝试账号；上游已开始 SSE 输出后不切换账号，避免拼接两个不同响应。
- **额度观察**：仅上游明确 `usage_limit_reached` 才记录账号/模型级额度耗尽与上游提供的恢复时间，并驱动模型冷却；普通 429 不伪装为套餐额度。
- 用量窗口（`GET /backend-api/wham/usage`）只保存计划类型、`used_percent`、窗口长度、恢复时间与 `allowed` / `limit_reached`，快照 15 分钟过期、刷新失败保留最后成功值；它是额度窗口观测，不改变模型路由或冷却。
- `refresh_account_interval_minute: 0` 关闭临期刷新；正数只刷新有可解析到期时间且将在 5 分钟内失效的正常账号。没有到期元数据的导入凭据仍可在实际 401 时刷新。
- 管理型 Provider 与 Codex 自动模型同名时都进入候选链：管理型默认 `priority=100`，Codex 默认 `90`，可在安全的原生 Responses 失败场景回退。

## 演进记录

- 2026-07-30：Codex OAuth 账号池收口设计 → 归档 `docs/archive/codex-oauth-account-pool-design-2026-07-30.md`
