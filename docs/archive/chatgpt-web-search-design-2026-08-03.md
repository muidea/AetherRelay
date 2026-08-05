# ChatGPT Web 强制联网搜索（2026-08-03）

> 状态：implemented

## 范围

此能力将明确请求的 OpenAI `web_search` 工具映射为一次隔离的 ChatGPT Web 强制搜索会话。它不是独立搜索索引、RAG、浏览器自动化，也不是通用 function/tool calling 实现。

```text
OpenAI Chat / Responses / `POST /v1/search` 或 Admin 功能集 / 临时对话
  -> proxyapi 协议适配与账号治理
    -> chatgptaccountpool（选择、结果反馈、冷却、OAuth 刷新）
      -> chatgptwebupstream（prepare -> force_use_search -> poll）
```

不增加运行单元：上游 HTTP 协议属于 `chatgptwebupstream` Block；账号、凭据与冷却仍只由 `chatgptaccountpool` owner 管理；`proxyapi` 编排选择、401 刷新后仅重试一次和结果回写。

## 请求边界

- Chat Completions 仅接受唯一的 `web_search`、`web_search_preview` 或 `web_search_preview_2025_03_11` 工具；Chat 的 `web_search_options` 可作为搜索显式开关，但其调优字段只记录为受限降级，不会被伪造为已执行。
- Responses 仅接受上述单一搜索工具。
- `POST /v1/search` 是 ai-proxy 的非流式简化入口，请求体严格为 `model` 和纯文本 `query`；它返回 `search.result`、答案、来源与估算用量。该入口只保留 ChatGPT Web 候选，静态 Provider 的同名模型与路由优先级不参与选择。
- 只取最后一条纯文本 user 消息作为 query；图片、文件、function、混合工具、tool call、结构化输出和工具循环都会在上游请求前返回 `conversion_unsupported`。
- 请求 `model` 同时用于账号模型目录筛选和上游请求；没有硬编码搜索模型。

## 上游与韧性

上游依次执行 `POST /backend-api/f/conversation/prepare`、带 `force_use_search=true` 的 `POST /backend-api/f/conversation`，再有界轮询 `GET /backend-api/conversation/{id}`。prepare、requirements、SSE 与 poll 共用同一个按账号构造的浏览器 TLS client，因此账号 proxy 会覆盖整个搜索链路。

结果只投影为受限答案、实际模型和去重来源（标题、URL、摘要）。所有上游错误体、SSE、会话文档和来源数均有大小上限；不把原始会话或 token 交给协议层。

搜索与普通 ChatGPT 文本调用使用同一账号结果反馈：成功会更新可用性，`429` / 网络 / 超时 / 上游失败触发既有模型级冷却；首个 `401` 会单飞刷新 OAuth token 后安全重试一次。永久刷新失败或第二次 `401` 才使凭据失效。

## 响应与管理页

- Chat 非流式响应追加来源并提供 OpenAI `url_citation` annotations；流式响应在搜索完成后发送一个完整 delta 和 `[DONE]`，因此兼容 SSE 但不是增量上游流。
- Responses 返回 `web_search_call` 与带 citations 的 `output_text`；流式依次发送 created、in_progress/searching/completed、output item 和 completed 事件。
- Admin 临时对话提供逐轮“联网搜索”开关。它不会保存凭据或搜索会话，搜索轮不允许图片或文件附件，也不宣称支持深度研究、网页插件或持续工具循环。
- Admin「功能集」提供独立的「在线搜索」页面，经 Admin 的 typed Proxy command 调用同一强制搜索能力；管理端不接触客户端 Key、账号 token、账号代理或上游 HTTP client。成功结果会由 Proxy 写入 owner-scoped DuckDB 历史，刷新页面后可回看；列表与详情均通过 typed Proxy command 读取。每个管理员作用域最多 200 条、保留最多 30 天，失败搜索不写入；`/v1/search` 与协议内搜索保持无状态。
