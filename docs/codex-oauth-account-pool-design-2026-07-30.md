# Codex OAuth 账号池收口（2026-07-30）

## 范围

本实现新增独立 `codexoauth` 内建 Provider，只代理原生 `POST /v1/responses` 的 HTTP JSON/SSE P0。它不实现 ChatGPT 网页会话、消息树转换、临时对话、网页插件、WebSocket/realtime 或 `responses/compact`。

## 边界

```text
Admin / Proxy HTTP adapter
  -> proxyapi Biz (编排与 retry 策略)
  -> typed EventHub
  -> codexaccountpool Block (凭据、调度、冷却、refresh、模型快照)
  -> codexupstream Block (Codex HTTP/SSE 与模型枚举技术执行)
```

- `codexaccountpool` 是单一持久化凭据与模型快照资源 owner，使用 `codex_oauth_accounts`；不复用 `chatgpt_accounts` 或 ChatGPT OAuth session。
- `codexupstream` 执行 `https://chatgpt.com/backend-api/codex/responses` 及账号作用域的 `/models` 枚举，但不保存账号状态。
- `proxyapi` 通过 owner 的 typed command 获取一次性 request credential，HTTP handler 不持有 EventHub、token、store 或上游 client。
- 每个账号的 `proxy` 同时用于授权码换令牌、refresh token 与 Codex 上游请求；管理读模型从不返回该值。

## P0 行为

1. 轮询获得状态为 `normal`、且不在对应模型冷却期的账号。
2. 成功、401、429、超时、网络、协议和上游失败均回写账号结果。
3. 401 对同一账号按本地 ID 单飞 refresh；refresh 成功且尚未对客户端输出时仅重试一次。
4. 429 和瞬态错误写模型级冷却；`Retry-After` 不超过 3600 秒。429 仅在尚未输出 SSE 时改用未尝试账号。
5. 非流式请求强制请求上游 SSE，读取 `response.completed.response` 后返回原生对象；若上游返回 JSON Response 对象，则直接保留。
6. 流式请求在 EventHub 中使用 `Start/Pull/Cancel` 的有界 DTO；任何客户端写入失败、超时或 teardown 都取消上游 body。
7. Proxy 的发现编排按账号读取 `GET /backend-api/codex/models`，使用该账号代理、Bearer token 和 ChatGPT account header；仅持久化受限模型投影。快照 6 小时有效，失败按账号独立指数退避，且只有有有效快照的账号可被该模型调度。

## 配置与运维

- `codex_oauth.enabled`、`refresh_account_interval_minute` 在启动期决定 Block 生命周期，修改后重启。
- `models` 是可选精确 allowlist；留空时有效目录由健康账号的模型快照并集生成，设置时仅发布命中 allowlist 的已发现模型。静态 Provider 的同名模型优先。
- 定时 refresh 只处理已有可解析到期时间、且将在 5 分钟内过期的正常账号；无 expiry 的导入凭据由实际 401 驱动刷新。
- Admin 的 `/api/codex/**` 和「账号池 / Codex OAuth」页面支持脱敏列表、模型缓存状态、JSON 导入、批量 refresh/delete 与 PKCE OAuth。图片任务、图片库和历史对话归入「功能集」。callback、token、account ID、proxy 不会回显或写入 Web Storage。
