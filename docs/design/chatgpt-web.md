# ChatGPT Web 能力设计

功能域：ChatGPT Web 账号池与内建 Provider、文本/图片代理、图片任务与图片库、临时对话、在线搜索与管理页。对应正式合同见[配置参考](../configuration.md)、[功能说明](../features.md)与[运维与发布](../operations.md)。

## 设计目标

- 无需官方 API Key：通过 ChatGPT 网页账号凭据提供文本对话、图片生成与联网搜索。
- 账号池是内建 Provider 的唯一模型权威；账号凭据只存 `state.database`，绝不进入 YAML、日志、错误或管理 API。
- 全部能力以 Admin 管理页为运维入口，组件未装配时明确 503，不降级为空状态。

## 账号池与内建 Provider

- 启用 `chatgpt_web.enabled`（启动期装配开关，修改需重启）后注入只读内建 Provider `chatgptweb`；禁止在 `providers` 中声明 `protocol: chatgptweb`。
- 模型来自账号池对 `/backend-api/models` 的枚举并集，并与 Provider 精确模型合成同一有效目录；`model_metadata` 只为同 ID 模型补充容量。同名来源保留全部候选，`priority` 默认 `10` 且不作为回退候选。
- 账号列表严格脱敏：只返回稳定本地 ID、脱敏邮箱、状态、结果计数、冷却与最近刷新状态；token、账号代理与原始 OAuth 错误绝不返回。
- 账号导出是唯一有意返回明文 token 的接口：二次确认、`Cache-Control: no-store`、不写 Web Storage、Blob 即用即销。OAuth 授权 URL、callback 与 session 仅存页面内存态，完成或取消后销毁。
- `provider_enabled` 只控制内建 Provider 是否参与路由（可热更新）；不影响账号刷新、图片或临时对话。

## 文本与图片代理

- `/v1/chat/completions`：纯文本与 OpenAI content-part 数组中的 `text` / `image_url`（仅 PNG/JPEG/GIF/WebP Base64 data URI，最多 4 张、合计 20 MiB、单图 ≤ 4000 万像素）；不下载远程 URL（无 SSRF 通道）；图片仅限 `user` 消息；`input_audio`、`file`、工具调用与其它 content part 返回 `invalid_request`。
- `/v1/responses`：同一文本执行器的无状态受限投影（字符串/message-array `input`、`instructions`、`reasoning.effort`、`input_text`、data-URI `input_image`、基础 buffered/SSE）；不保存会话；可兼容忽略的字段在 `ignored_features` 中可审计，改变语义的字段返回 `conversion_unsupported`。
- `/v1/images/generations` / `/v1/images/edits`：上游生图代理。图片字节只存本地文件系统，DuckDB 只存元数据与索引；interaction archive 对 data URI / `b64_json` 只存 MIME、字节数与 SHA-256 摘要。
- 韧性：`rate_limit` / TLS / 超时 / 上游故障生成 60 秒生图冷却；`invalid_token` 先触发一次 OAuth 刷新，仅尚未创建 conversation 时重投一次，已有 conversation 永不盲重投。文本 token 为稳定本地估计，不可当上游账单。

## 图片任务与图片库

- **图片任务**：`owner_id` 必填，是任务查询与恢复的隔离边界，切换 owner 清空旧列表与轮询；已有会话的失败任务走恢复轮询（`extra_timeout_secs=30`，不重新提交生成），仅 bootstrap 阶段失败的未建会话任务可重新提交。
- **图片库**：列表、标签、删除（不可恢复）；图片内容经 Admin 鉴权同源只读端点 `GET /api/chatgpt/images/content?path=&thumb=` 读取，路径严格校验、no-store，不暴露通用 `/files/**`。

## 临时对话

- 会话、消息、图片附件与上游续聊锚点由服务端 DuckDB 专用表持久化（owner 仅来自已认证 Admin principal）；页面不落消息正文、上游会话 ID 或 token，全程 no-store。
- API 覆盖创建、历史分页、详情、turns 长轮询事件、取消与永久删除。
- 附件限 PNG/JPEG/GIF/WebP，最多 4 张、合计 20 MiB；保留期 `retention_days` 默认 30 天；达到 `max_conversations` 拒绝新建，不静默删历史。
- 进程重启时未完成流持久化为 `interrupted`、会话进入 `recovery_required`（历史可读、不得在原上游分支继续发送，需新建会话）；不自动重发。
- 逐轮联网搜索开关：启用时不接受图片/文件附件；research / deep_research 专用模型不进入选择器。

## 在线搜索

- **请求边界**：Chat/Responses 仅接受唯一的 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11` 工具（或 `web_search_options`）；只取最后一条纯文本 user 消息为 query；图片、文件、function、混合工具与工具循环一律 `conversion_unsupported`；`web_search_options` 调优字段仅记受限降级。
- **上游执行**：prepare → `force_use_search=true` conversation → 有界 poll；全链路共用账号级 TLS client；结果仅投影受限答案、实际模型与去重来源（标题/URL/摘要），均有大小上限；429/网络/超时/上游失败触发模型级冷却，首个 401 单飞刷新后仅重试一次。
- **响应形态**：Chat 非流式追加来源与 OpenAI `url_citation` annotations；流式在搜索完成后发送单个完整 delta 与 `[DONE]`（兼容 SSE，不是增量搜索流）；Responses 返回 `web_search_call` 与带 citations 的 `output_text`。
- **`POST /v1/search`**：非流式简化入口，请求体仅 `model` 与纯文本 `query`；只保留 ChatGPT Web 候选，静态 Provider 同名模型不参与；无可用搜索能力时返回明确错误，不降级为普通文本生成。
- **管理页「功能集 → 在线搜索」**：经 typed Proxy command 调用；成功结果写 owner-scoped DuckDB 历史（`chatgpt_web_search_history`），每管理员最多 200 条、保留 30 天，失败不写入；未启用 Admin 登录时使用稳定本地 `admin` 作用域。`POST /v1/search` 与协议内单次搜索保持无状态，不写历史。

## 管理页信息架构

「Provider / 客户端 Key / 使用统计 / 系统信息」页签不变；「账号池」按 ChatGPT Web 与 Codex OAuth 分组，「功能集」含临时对话、在线搜索、图片任务、图片库。只消费既有 `/admin/api/chatgpt/**` 管理 API；组件未装配时返回 503，页面显示不可用状态而非空数据。

## 演进记录

- 2026-07-26：内建 Provider 自动发现方案 → 归档 `docs/archive/chatgptweb-builtin-provider-auto-discovery-design-2026-07-26.md`
- 2026-07-26：管理页收口设计 → 归档 `docs/archive/chatgpt-web-admin-closure-design-2026-07-26.md`
- 2026-07-27：用量统计接入设计 → 归档 `docs/archive/chatgptweb-usage-accounting-design-2026-07-27.md`
- 2026-07-27：临时对话设计 → 归档 `docs/archive/chatgpt-temporary-chat-design-2026-07-27.md`
- 2026-08-03：强制联网搜索设计 → 归档 `docs/archive/chatgpt-web-search-design-2026-08-03.md`
