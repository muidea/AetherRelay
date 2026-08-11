# ChatGPT Web 能力设计

功能域：ChatGPT Web 账号池与内建 Provider、文本/图片代理、图片任务与图片库、临时对话、在线搜索与管理页。对应正式合同见[配置参考](../configuration.md)、[功能说明](../features.md)与[运维与发布](../operations.md)。

## 设计目标

- 无需官方 API Key：通过 ChatGPT 网页账号凭据提供文本对话、图片生成与联网搜索。
- 账号池是内建 Provider 的唯一模型权威；敏感账号凭据经统一主密钥加密后只存 `state.database`，绝不进入 YAML、日志、错误或管理 API。
- 全部能力以 Admin 管理页为运维入口，账号池与相关组件始终装配。

### Admin 工具集的内建作用域

- Usage runtime 启动时幂等创建服务端维护的客户端作用域 `builtin-local`。它只有元数据和 `all` Provider 访问范围，不保存 raw secret/hash，也不是外部客户端可以携带的 API Key。
- 临时对话、在线搜索、图片代理以及图片任务/图片库在未指定 `api_key_id` 时都使用 `builtin-local`；Admin principal 仍单独用于临时会话和搜索历史的 owner 隔离。Admin 也可以显式选择其它已存在的客户端 Key。
- 管理台会展示该内建条目及其模型目录，但禁止创建同名 Key、启停、权限修改、轮换和删除；公开 `/v1/*` 仍必须使用普通客户端密钥。

## 账号池与内建 Provider

- 进程始终注入只读内建 Provider `chatgptweb`；`config.yaml` 不声明任何 Provider。
- 模型来自账号池对 `/backend-api/models` 的枚举并集，并与 Provider 精确模型合成同一有效目录；`model_metadata` 为同 ID 模型补充容量与可选 reasoning 能力声明。同名来源保留全部候选，`priority` 默认 `10` 且不作为回退候选。
- 账号列表返回稳定本地 ID、邮箱、状态、结果计数、冷却与最近刷新状态；token、账号代理与原始 OAuth 错误绝不返回。
- 账号导出是唯一有意返回明文 token 的接口：固定返回可直接作为 `accounts` 重新导入的 JSON 数组，二次确认、`Cache-Control: no-store`、不写 Web Storage、Blob 即用即销。导入同时支持纯 access token 和完整 OAuth 对象；完整对象保留 refresh/id token 与账号代理，不会退化为单 token。OAuth 授权 URL、callback 与 session 仅存页面内存态，完成或取消后销毁。
- `provider_enabled` 只控制内建 Provider 是否参与路由（可热更新）；不影响账号刷新、图片或临时对话。

## 文本与图片代理

- `/v1/chat/completions`：纯文本与 OpenAI content-part 数组中的 `text` / `image_url`（仅 PNG/JPEG/GIF/WebP Base64 data URI，最多 4 张、合计 20 MiB、单图 ≤ 4000 万像素）；不下载远程 URL（无 SSRF 通道）；图片仅限 `user` 消息；`input_audio`、`file`、工具调用与其它 content part 返回 `invalid_request`。
- `/v1/responses`：同一文本执行器的无状态受限投影（字符串/message-array `input`、`instructions`、`reasoning.effort`、`input_text`、data-URI `input_image`、基础 buffered/SSE）；不保存会话；可兼容忽略的字段在 `ignored_features` 中可审计，改变语义的字段返回 `conversion_unsupported`。
- `/v1/images/generations` / `/v1/images/edits`：上游生图代理。ChatGPT Web conversation 协议没有 OpenAI Images API 的原生 `size` / `response_format` 字段；旧实现把尺寸附加到 prompt，只能表达意图，不能保证像素尺寸。现在请求只接受 `auto` 或受限的正整数 `WIDTHxHEIGHT`，并在拿到认证后的上游栅格 bytes 后本地中心裁切/双线性缩放；明确 `WxH` 才保证精确像素，`auto` 保留上游尺寸，无法解码 bytes 时失败而不虚报成功。图片字节只存本地文件系统，DuckDB 只存元数据与索引；interaction archive 对 data URI / `b64_json` 只存 MIME、字节数与 SHA-256 摘要。
- ChatGPT Web 只返回 raster（PNG/JPEG/GIF 等可验证栅格）。它没有真正的 SVG/vector 输出协议；SVG 容器包裹 raster 也不是真矢量，因此显式 `svg`、`response_format: svg` 或“导出矢量文件”请求会在访问上游前明确拒绝。vector-like/raster illustration 等仅描述视觉风格的提示仍可生成栅格图。
- 韧性：`rate_limit` / TLS / 超时 / 上游故障生成 60 秒生图冷却；`invalid_token` 先触发一次 OAuth 刷新，仅尚未创建 conversation 时重投一次，已有 conversation 永不盲重投。文本 token 为稳定本地估计，不可当上游账单。

## 图片任务与图片库

- **图片任务**：`api_key_id` 缺省时使用内建 `builtin-local`，显式值必须对应已存在客户端 Key；它是任务查询、取消、删除与恢复的隔离边界，切换 Key 清空旧列表与轮询。`size` 只接受 `auto` 或正整数 `WIDTHxHEIGHT`（最大边 8192、总像素 4000 万），`quality` 仍按上游能力传递。ChatGPT Web 没有原生尺寸字段，任务完成后由本地栅格规范化保证明确 `WxH` 的实际结果尺寸；详情和图片库展示实际宽、高、格式。SVG/vector 文件请求明确失败，不伪装成 SVG。所有任务开放详情，`queued` / `running` 可取消，终态记录可删除。取消先持久化 `cancelled`，再传播任务级 context 取消；状态机拒绝迟到进度、成功或失败覆盖该终态。该取消是协作式的，上游已经受理时不承诺停止执行或免除额度消耗。删除只处理任务记录，图片库资产由图片库独立管理。已有会话的失败任务走恢复轮询（`extra_timeout_secs=30`，不重新提交生成），仅 bootstrap 阶段失败的未建会话任务可重新提交，其它失败不开放通用重试。
- **图片库**：列表、标签、删除（不可恢复）；图片内容经 Admin 鉴权同源只读端点 `GET /api/chatgpt/images/content?path=&thumb=` 读取，路径严格校验、no-store，不暴露通用 `/files/**`。

## 临时对话

- 会话、消息、图片附件与上游续聊锚点由服务端 DuckDB 专用表持久化（owner 仅来自已认证 Admin principal）；页面不落消息正文、上游会话 ID 或 token，全程 no-store。
- 历史读取和写入始终以 `(owner_id, conversation_id)` 为复合作用域；构造上游文本请求时只读取当前会话的成功历史，失败/取消/中断轮次不会回放到下一轮。不同会话不会共享本地消息、附件或续聊锚点。
- 管理页为会话详情、历史分页、发送、取消和增量轮询维护独立的请求代际。切换、创建、删除或离开功能页会取消旧请求；响应返回后必须再次核对会话 ID 与代际，旧会话响应不得覆盖当前气泡、轮询状态或输入状态。
- ChatGPT Web 文本请求每轮使用独立的随机 `parent_message_id`，并发送 `history_and_training_disabled=true`；本地历史由 AetherRelay 显式重建，不依赖同一网页账号的隐式会话链。
- API 覆盖创建、历史分页、详情、turns 长轮询事件、取消与永久删除。
- 附件限 PNG/JPEG/GIF/WebP，最多 4 张、合计 20 MiB；保留期 `retention_days` 默认 30 天；达到 `max_conversations` 拒绝新建，不静默删历史。
- 进程重启时未完成流持久化为 `interrupted`、会话进入 `recovery_required`（历史可读、不得在原上游分支继续发送，需新建会话）；不自动重发。
- 逐轮联网搜索开关：启用时不接受图片/文件附件；research / deep_research 专用模型不进入选择器。

## 在线搜索

- **请求边界**：Chat/Responses 仅接受唯一的 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11` 工具（或 `web_search_options`）；只取最后一条纯文本 user 消息为 query；图片、文件、function、混合工具与工具循环一律 `conversion_unsupported`；`web_search_options` 调优字段仅记受限降级。
- **上游执行**：prepare → `force_use_search=true` conversation → 有界 poll；每次搜索生成独立的随机 `parent_message_id`，prepare 与 conversation 仅共享本次请求的根，并在两步都发送 `history_and_training_disabled=true`，不复用固定根或历史网页会话；全链路共用账号级 TLS client；结果仅投影受限答案、实际模型与去重来源（标题/URL/摘要），均有大小上限；429/网络/超时/上游失败触发模型级冷却，首个 401 单飞刷新后仅重试一次。
- ChatGPT Web 是非公开网页逆向接口；上述字段只能阻止代理主动携带历史，不能覆盖上游账号级 Memory/Reference chat history 等服务端策略。若账号仍注入跨主题内容，应在 ChatGPT 账号侧关闭相关记忆能力，或将该账号标记为不保证严格隔离。
- 排查污染时，先以管理页会话 ID 对照 DuckDB 中当前会话的消息，再按 `var/interactions/{api_key_id}/{round_id}/` 的 request/response 与 `metadata.json` 核对实际发送的消息；若本地请求未携带异会话内容而上游仍返回跨主题文本，问题位于上游账号记忆/网页会话策略，不应回写或拼接到其它本地会话。
- **响应形态**：Chat 非流式追加来源与 OpenAI `url_citation` annotations；流式在搜索完成后发送单个完整 delta 与 `[DONE]`（兼容 SSE，不是增量搜索流）；Responses 返回 `web_search_call` 与带 citations 的 `output_text`。
- **`POST /v1/search`**：非流式简化入口，请求体仅 `model` 与纯文本 `query`；只保留 ChatGPT Web 候选，管理型 Provider 同名模型不参与；无可用搜索能力时返回明确错误，不降级为普通文本生成。
- **管理页「功能集 → 在线搜索」**：经 typed Proxy command 调用；成功结果写 owner-scoped DuckDB 历史（`chatgpt_web_search_history`），每管理员最多 200 条、保留 30 天，失败不写入；未启用 Admin 登录时使用稳定本地 `admin` 作用域。`POST /v1/search` 与协议内单次搜索保持无状态，不写历史。

## 管理页信息架构

「Provider / 客户端 Key / 使用统计 / 系统信息」页签不变；「账号池」按 ChatGPT Web 与 Codex OAuth 分组，「功能集」含临时对话、在线搜索、图片任务、图片库。只消费既有 `/admin/api/chatgpt/**` 管理 API；没有账号时展示明确的空池状态。

## 演进记录

- 2026-07-26：内建 Provider 自动发现方案 → 归档 `docs/archive/chatgptweb-builtin-provider-auto-discovery-design-2026-07-26.md`
- 2026-07-26：管理页收口设计 → 归档 `docs/archive/chatgpt-web-admin-closure-design-2026-07-26.md`
- 2026-07-27：用量统计接入设计 → 归档 `docs/archive/chatgptweb-usage-accounting-design-2026-07-27.md`
- 2026-07-27：临时对话设计 → 归档 `docs/archive/chatgpt-temporary-chat-design-2026-07-27.md`
- 2026-08-03：强制联网搜索设计 → 归档 `docs/archive/chatgpt-web-search-design-2026-08-03.md`
