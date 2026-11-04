# ChatGPT Web 能力设计

功能域：ChatGPT Web 账号池与内建 Provider、文本/图片代理、图片任务与图片库、临时对话、在线搜索与管理页。对应正式合同见[配置参考](../configuration.md)、[功能说明](../features.md)与[运维与发布](../operations.md)。

收口记录：[浏览器访问一致性完善方案（2026-10-28）](../archive/chatgpt-web-upstream-closure-plan-2026-10-28.md)。本次实现固定新加坡环境、导航/API 请求头区分及账号设备身份持久化；UA/TLS 与独立 Cookie Jar 边界沿用现有行为。验证进度及暂缓候选见该记录。

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
- 设备身份缺失或空字符串时，首次持久化生成 UUIDv4；已有非法格式或非字符串值保持原样，并通过 `device_identity_error=invalid_device_identity` 显示账号本地异常。该账号停止文本/图片调度、任务恢复、发现和自动刷新，不影响其它 Web 账号或 Codex 号池启动。启用或更新凭据不解除设备身份限制；共享存储的加载、解密与写入故障仍明确报错。
- 账号导出是唯一有意返回明文 token 的接口：固定返回可直接作为 `accounts` 重新导入的 JSON 数组，二次确认、`Cache-Control: no-store`、不写 Web Storage、Blob 即用即销。导入同时支持纯 access token 和完整 OAuth 对象；完整对象保留 refresh/id token 与账号代理，不会退化为单 token。OAuth 授权 URL、callback 与 session 仅存页面内存态，完成或取消后销毁。
- 账号行重新认证时 OAuth session 绑定稳定本地 ID；通用 OAuth 入口按精确上游 `account_id`（缺失时仅按唯一邮箱）识别 token 轮换。更新保留本地元数据并清理旧凭据失败，不会合并不同 `account_id` 的同邮箱 workspace。新增或重新认证成功后立即为该本地 ID 提交后台账号信息刷新，OAuth 完成响应返回刷新进度 ID，管理页静默轮询并重载额度与状态；刷新任务无法启动或上游信息刷新失败不回滚已经成功写入的 OAuth 凭据。
- `provider_enabled` 只控制内建 Provider 是否参与路由（可热更新）；不影响账号刷新、图片或临时对话。

## 浏览器通讯环境与设备身份

- UA、Client Hints 和 TLS 档位继续使用固定 Windows Chrome 133 环境。语言以现有中文环境为基准；时间、文本/搜索/图片的 `timezone` 统一使用 `Asia/Singapore`，UTC+08:00，`timezone_offset_min=-480`。日期转换到当地时间，Unix 时间戳保持绝对时刻。
- 首页 bootstrap 模拟直接导航：导航 Priority/缓存语义，不携带 API Bearer、Origin、Referer 或 oai-* 头。JSON GET 保留认证与同源 Fetch Metadata，省略 Origin；JSON POST 保留 Origin 与 JSON Content-Type；生成 SSE 的专用 token、路由与头保持既有规则。
- 账号池在启动迁移和新账号保存时初始化密码学随机 UUIDv4，以 `web_device_id` 存入加密账号文档。事务提交成功后才发布；同主体凭据刷新和重启延续设备值，正常请求不按 token 重新生成。上游命令只接收不可变设备值；缺少或非法设备值明确失败，不随机降级。
- 每次独立执行尝试新建 Client、Session ID 和 Cookie Jar；同次链路保留 Cookie，安全重试按既有预算新建尝试并重新准备临时 token，同账号设备值延续。上游设置的设备 Cookie 保留在当前 Jar，不合成为额外头、不覆盖本地设备，也不跨操作共享。
- 普通凭据导出和整体账号 bundle 不包含设备身份。导入新账号生成本实例设备，更新已有账号保留目标设备；完整数据库备份恢复保留设备。模型快照失效、冷却、重试和账号状态策略维持既有合同。

## 文本与图片代理

- **文本代理**：`/v1/responses` 接受纯文本 `input`，或 message 数组中的 `input_text` / `input_image`（仅 PNG/JPEG/GIF/WebP Base64 data URI，最多 4 张、合计 20 MiB、单图 ≤4000 万像素）；不下载远程图片 URL，图片仅限 user 消息。
- `/v1/responses`：同一文本执行器的无状态受限投影（字符串/message-array `input`、`instructions`、`reasoning.effort`、`input_text`、data-URI `input_image`、data-URI `input_file`、buffered/SSE）。文件限 PDF/TXT/Markdown/CSV，整次请求最多 4 份、合计 20 MiB。公共字段校验先于普通文本/搜索分流；未知字段或无法执行的语义返回 `conversion_unsupported`，不保存 Responses 会话。
- `/v1/images/generations` / `/v1/images/edits`：上游生图代理。ChatGPT Web conversation 协议没有 OpenAI Images API 的原生 `size` / `response_format` 字段；旧实现把尺寸附加到 prompt，只能表达意图，不能保证像素尺寸。现在请求只接受 `auto` 或受限的正整数 `WIDTHxHEIGHT`，并在拿到认证后的上游栅格 bytes 后本地中心裁切/双线性缩放；明确 `WxH` 才保证精确像素，`auto` 保留上游尺寸，无法解码 bytes 时失败而不虚报成功。图片字节只存本地文件系统，DuckDB 只存元数据与索引；interaction archive 对 data URI / `b64_json` 只存 MIME、字节数与 SHA-256 摘要。
- ChatGPT Web 只返回 raster（PNG/JPEG/GIF 等可验证栅格）。它没有真正的 SVG/vector 输出协议；SVG 容器包裹 raster 也不是真矢量，因此显式 `svg`、`response_format: svg` 或“导出矢量文件”请求会在访问上游前明确拒绝。vector-like/raster illustration 等仅描述视觉风格的提示仍可生成栅格图。
- 韧性：`rate_limit` / TLS / 超时 / 上游故障生成 60 秒生图冷却；`invalid_token` 先触发一次 OAuth 刷新，仅尚未创建 conversation 时重投一次，已有 conversation 永不盲重投。文本 token 为稳定本地估计，不可当上游账单。
- HTTP 200 中的 SSE 明确错误同样属于失败：文本、搜索、图片统一识别 `error` / `error_code`，限额代码（包括 `usage_limit`）归入 `rate_limit`，保留有界原始码和提示，未知码归入 `upstream`；不检查消息正文中的同名字段。SSE 错误不会成为凭据失效判据。已有会话、图片引用、文本及输出观测保留为失败的部分结果；流式错误提示继续传递到代理日志。实际 HTTP 状态仍可为 200，不伪造上游 429。

### 文本完成性与正文预算

- 文本 parser 的生成完成证据与流任务 `Done` 分开：`Done` 仅表示任务结束，`aetherrelaycompletion.Text` 为各 owner 合同与本地 Executor 共用的不可变证据值，不携带正文或传输句柄。当前支持的成功终止证据为 `[DONE]`；即使收到该标记，也必须已有非空正文、会话和 assistant message 锚点，且之前未发生协议、快照或预算错误。支持终止事件后直接 EOF、没有最终空行的合法结束；普通 EOF 不是完成证据。
- 当前正文协议为 assistant 的 `text`／纯字符串 `multimodal_text` 完整快照，正文通道仅支持未指定或 `final`。同一回答绑定会话、消息及通道；重复快照不重复输出，前缀增长只发后缀。缩短、改写、回答身份变化、损坏 JSON、无法安全忽略的未知正文结构或非字符串正文 part 明确失败，不将整份新快照再次追加。仅明确支持的非正文控制／元数据事件可忽略；未验证的多阶段或结构化增量协议不静默降级。
- 累计 SSE 读取上限为 16 MiB，单行及聚合事件上限各为 1 MiB，候选正文与累计接受的追加正文上限各为 256 KiB。单行预算不包含 CR/LF；聚合事件按每条 `data` 载荷及一个拼接 LF 计数。读取边界最多探测一个额外字节以区分真实 EOF 和预算耗尽；终止证据在预算内时不继续排空连接。SSE 字节与摘要对应实际读取字节（含缓冲预读和必要的探测字节），并非仅已解析事件。
- 所有正文限制均在发出新增正文前检查，不按字节截断后返回成功。成功结果的长度、SHA-256、累计追加字节与各消费者重建的正文必须一致。协议失败时保留此前已接受的部分正文和执行证据；已经尝试发出字节不等于客户端已收到。
- 新失败分类为 `text_incomplete`（缺少完成证据，或消费者发现结果证据缺失／不一致）、`text_protocol`（协议、快照或结果证据不一致）、`text_budget`（本地读取／正文保护）。`finish_details.type=max_tokens/length` 明确返回 `text_model_length`，当前严格完成模式不将模型长度终止包装为正常成功正文；未知结束原因不冒充 `length`。这些失败不刷新令牌、不换账号重放，也不触发账号健康冷却；原有真实鉴权、上游限流和传输错误保持独立反馈。
- Responses 与临时对话分别校验完成证据。普通文本非流式失败返回类型化非 2xx 错误；Responses 流式失败发送 `response.failed`，不发送 `response.completed`。HTTP 200 不作为流式成功依据。搜索另用 `aetherrelaycompletion.Search` 记录 document 的正常终态、模型截断、正文长度与 SHA-256，并贯穿 owner、Biz 和 HTTP；零值、证据与正文不一致不能授权成功。
- 现有执行 trace 的 `attempts[].text_completion` 保存终止证据、稳定原因、限制类型与数值、SSE／正文／追加字节、摘要及 HTTP 是否已开始写出；同时填充已有事件数、wire bytes、模型与发送证据字段。SSE 摘要仅覆盖已读取字节；普通错误日志和错误响应只使用安全分类，不包含任意上游错误正文。详情仍遵循现有 trace／归档访问与清理合同。
- 临时对话直接消费 owner 的完成证据，单独核对正文并持久化失败／中断状态。生成已发送或结果未知的失败要求恢复，禁止直接正常续聊；只有明确未发送、无正文／锚点且未超既有预算时才允许准备阶段凭据刷新恢复。用量保留真实错误类别，不将需要恢复的上游失败一律写成进程中断。
- 传输完成不等于业务 JSON 合法。网关不修补、裁剪或重新生成业务正文，业务发布门禁由调用方执行。

### 文本调用的有限恢复与观测

- 普通文本的受限 Responses 与临时对话共用执行器，HTTP/SSE 每个请求最多两次执行；401 凭据刷新与替代账号共用这个预算，不叠加重试。
- 上游明确返回 `generation_not_sent=true`，且失败阶段为 `bootstrap`、`requirements_prepare`、`requirements_finalize` 时，准备阶段的 5xx/TLS/超时可尝试一次替代账号；403 仅在 bootstrap 阶段适用。429、内容策略、未知发送状态/阶段、已有输出、续接、取消和剩余预算不足均不切号。生成请求进入 HTTP `Do` 后即视为可能已提交，不能因结果为空推断可重放。
- 候选仍由账号池按原模型、能力和冷却裁决，排除已失败凭据和账号，最多检查 8 个候选，优先选择不同已配置代理端点；没有不同出口时最多使用一个其他账号，不改写账号代理。没有候选时保留最初的真实失败。403 不等同于凭据永久失效。
- `metadata.json` 的 `chatgpt_text_attempts` 最多记录两次实际执行的账号、阶段、真实 HTTP 状态、未发送证据及结果；用量仅结算一次，最终账号和上游状态与归档一致。没有真实上游响应时不编造 HTTP 状态；历史记录不回填。
- 文本准备阶段沿用请求 context，取消和超时传播到 bootstrap / requirements HTTP 请求。缓存用量未知以及失败请求不参与缓存命中率/SLO；普通估算 Token 仍保留，不能当作上游账单。

### 客户端参数适配

- 普通文本和搜索共用请求 allowlist，拒绝发生在账号选择和上游执行之前。未知顶层字段及已识别对象中的未知控制字段明确拒绝；`response_format`、非空 `max_output_tokens` / `temperature` / `top_p` / `service_tier`、严格 JSON Schema、函数工具、持久会话、后台执行均不支持，不按提示词模拟这些能力。
- 接受 `text.format={"type":"text"}`、`tools:[]`、`tool_choice:"none"`（无工具时）、`background:false`、`parallel_tool_calls:false`、`store:false`、`include:[]`、`truncation:"disabled"` 及支持字段的可空默认值。`store:true` 无存储语义，明确拒绝。`metadata` 按字符串映射校验并回显；`user` / `safety_identifier` 仅作为兼容字段忽略并记入 `ignored_features`，不承诺上游安全识别。
- `reasoning.effort` 必须同时满足模型声明及 Web 已实现映射：`low/medium/high` 保留，`xhigh/extended` 映射 Web `extended`；`none/minimal/max/ultra` 等没有 Web 映射的值拒绝。`reasoning.summary`、`text.verbosity` 的有效设置明确拒绝；搜索不能承载 effort，非空值也拒绝。
- 非流式与流式共用响应对象和创建时间；SSE 使用单调递增 `sequence_number`，完整发出 response / output item / content part / text 生命周期。搜索正文为轮询完成后的单个 delta，不承诺上游 token 级搜索流。失败与截断均不发送 `response.completed`。
- `GET /v1/models` 的 `capabilities.chatgptweb.responses` 发布 provider-scoped 边界（文本、受限图片/文件、流式、单查询搜索；函数工具、结构化输出、续接、后台和客户端输出 token 限制均为 false，`usage=estimated_text_only`），不继承同名其它 Provider 的原生能力。依赖严格 JSON 输出的客户端须选择明确支持的 Provider。
- GPTWeb JSON/SSE 响应头 `X-AetherRelay-Usage-Estimated: true`、`X-AetherRelay-Usage-Scope: text-only` 表明 usage 只估算可见文本；图片、文件、隐藏推理及上游实际计费不在估算内。管理页用量详情同步说明，记录仍带 `estimated=true`，不伪造缓存或 reasoning token 数。
- 代理保留 `conversion_unsupported`、`feature`、`unsupported_features`，本地参数拒绝不触发刷新或切号。此次 Responses 收口及回归门禁见[2026-11-04 Responses 收口记录](../AetherRelay-ChatGPTWeb-Responses-Closure-2026-11-04.md)。

## 图片任务与图片库

- **图片任务**：`api_key_id` 缺省时使用内建 `builtin-local`，显式值必须对应已存在客户端 Key；它是任务查询、取消、删除与恢复的隔离边界，切换 Key 清空旧列表与轮询。`size` 只接受 `auto` 或正整数 `WIDTHxHEIGHT`（最大边 8192、总像素 4000 万），`quality` 仍按上游能力传递。ChatGPT Web 没有原生尺寸字段，任务完成后由本地栅格规范化保证明确 `WxH` 的实际结果尺寸；详情和图片库展示实际宽、高、格式。SVG/vector 文件请求明确失败，不伪装成 SVG。所有任务开放详情，`queued` / `running` 可取消，终态记录可删除。取消先持久化 `cancelled`，再传播任务级 context 取消；状态机拒绝迟到进度、成功或失败覆盖该终态。该取消是协作式的，上游已经受理时不承诺停止执行或免除额度消耗。删除只处理任务记录，图片库资产由图片库独立管理。已有会话的失败任务走恢复轮询（`extra_timeout_secs=30`，不重新提交生成），仅 bootstrap 阶段失败的未建会话任务可重新提交，其它失败不开放通用重试。
- **图片库**：列表、标签、删除（不可恢复）；Admin 图片内容经会话鉴权同源只读端点 `GET /api/chatgpt/images/content?path=&thumb=` 读取。ChatGPT Web Images 的 `response_format=url` 使用独立的短期签名 `GET /images/{scope}/{path}`，由 Proxy 校验完整路径、到期时间和当前启用的 Client Key 后按作用域读取；凭据在生图期间失效时默认返回 `authentication_failed`，显式发送 `X-AetherRelay-Allow-Image-Format-Fallback: b64_json` 的扩展客户端可接收内联 `b64_json`，实际格式同时写入响应正文 `response_format` 和 `X-AetherRelay-Image-Response-Format` 响应头。多张内联图片共享整次 JSON 响应大小预算。删除 Client Key 会先持久化禁用并关闭新图片请求准入，再以按 Key 的清理流程等待已进入的图片请求和任务结束；失败后保持禁用并允许重复 DELETE 继续幂等清理。签名参数在交互归档中脱敏。两类读取均严格校验路径并返回 `no-store`，不暴露通用 `/files/**`。

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

- **请求边界**：Responses 仅接受唯一的 `web_search` / `web_search_preview` / `web_search_preview_2025_03_11` 工具；只接受字符串 input 或唯一一条纯文本 user 消息，查询最多 64 KiB，超限在账号选择前拒绝；非空 instructions、多轮上下文、图片、文件、function、混合工具与工具循环一律 `conversion_unsupported`。工具只接受 type 声明，域名过滤、位置、搜索深度、离线搜索等未实现选项明确拒绝。`tool_choice` 可省略（选择该受限 Web 搜索即强制搜索），或为 `required` / 精确匹配的工具对象；显式 `auto` 无法承诺模型自主选择，明确拒绝。`web_search_options` 明确拒绝。
- **上游执行与恢复边界**：prepare → `force_use_search=true` conversation → 有界 poll；每次搜索生成独立的随机 `parent_message_id`，prepare 与 conversation 仅共享本次请求的根，并在两步都发送 `history_and_training_disabled=true`，不复用固定根或历史网页会话。单次尝试中的 prepare、conversation 与 poll 共用账号级 TLS client；仅当 `search_prepare` 在尚未取得 conversation ID 时分类为 TLS/超时，才会短暂退避并重建 TLS client 重试一次。该重试仍失败后，最多切换一次到不同账号且不同已配置代理端点；conversation 已开始、SSE 或 poll 阶段绝不重投，避免重复搜索或答案。结果仅投影受限答案、实际模型与去重来源（标题/URL/摘要），均有大小上限；429/网络/超时/上游失败触发模型级冷却，首个 401 单飞刷新后仅重试一次。
- conversation ID 只表示已提交；搜索读取有界 SSE 至 `[DONE]`/EOF，识别 ID 之后的失败再决定是否进入 poll。SSE 明确错误不进入轮询；轮询收到 HTTP 429 或明确 JSON 错误立即失败，保留会话 ID 和已有结果并反馈冷却。瞬态 404/409/423/5xx 沿用既有有界轮询，不重投搜索。
- ChatGPT Web 是非公开网页逆向接口；上述字段只能阻止代理主动携带历史，不能覆盖上游账号级 Memory/Reference chat history 等服务端策略。若账号仍注入跨主题内容，应在 ChatGPT 账号侧关闭相关记忆能力，或将该账号标记为不保证严格隔离。
- 排查污染时，先以管理页会话 ID 对照 DuckDB 中当前会话的消息，再按 `var/interactions/{api_key_id}/{round_id}/` 的 request/response 与 `metadata.json` 核对实际发送的消息；若本地请求未携带异会话内容而上游仍返回跨主题文本，问题位于上游账号记忆/网页会话策略，不应回写或拼接到其它本地会话。
- **完成判定**：只接受已识别的 document 正常终态（`stop` / `completed` / `finished_successfully`）；正文连续不变、轮询普通 EOF 或超时有正文均不是完成依据。`max_tokens` / `length` 保留正文并返回 Responses `status=incomplete` / `response.incomplete`，用量记为 `text_model_length`，不计成功、不触发账号冷却或重投。未知结束原因、本地正文预算超限、超时明确失败；超限不按字节截断正文后返回成功。
- **响应形态**：Responses 返回 `web_search_call` 与带平铺 `url_citation` annotations 的 `output_text`，引用索引对应最终正文的 Unicode 字符位置；失败保留已接受的部分正文，SSE 以 `response.failed` 结束。简化 `/v1/search` 及 Admin 功能要求完整答案，截断返回非 2xx，不写成功搜索历史。
- **`POST /v1/search`**：非流式简化入口，请求体仅 `model` 与纯文本 `query`；只保留 ChatGPT Web 候选，管理型 Provider 同名模型不参与；无可用搜索能力时返回明确错误，不降级为普通文本生成。
- **管理页「功能集 → 在线搜索」**：经 typed Proxy command 调用；成功结果写 owner-scoped DuckDB 历史（`chatgpt_web_search_history`），每管理员最多 200 条、保留 30 天，失败不写入；未启用 Admin 登录时使用稳定本地 `admin` 作用域。`POST /v1/search` 与协议内单次搜索保持无状态，不写历史。

## 管理页信息架构

「Provider / 客户端 Key / 使用统计 / 系统信息」页签不变；「账号池」按 ChatGPT Web 与 Codex OAuth 分组，「功能集」含临时对话、在线搜索、图片任务、图片库。只消费既有 `/admin/api/chatgpt/**` 管理 API；没有账号时展示明确的空池状态。

账号信息与图片额度采用账号级到期调度：每 15 秒领取最多 25 个到期账号，以 5 路并发刷新；恢复时间已到、从未刷新和最近使用的账号依次优先，禁用与长期未使用账号降低频率。下一次刷新、失败时间和退避均随账号持久化，失败按 1 至 30 分钟重试。手工刷新与定时刷新共享单飞任务，图片路由无可用额度时只触发非阻塞刷新，避免大号池扫描阻塞请求。

## 演进记录

- 2026-07-26：内建 Provider 自动发现方案 → 归档 `docs/archive/chatgptweb-builtin-provider-auto-discovery-design-2026-07-26.md`
- 2026-07-26：管理页收口设计 → 归档 `docs/archive/chatgpt-web-admin-closure-design-2026-07-26.md`
- 2026-07-27：用量统计接入设计 → 归档 `docs/archive/chatgptweb-usage-accounting-design-2026-07-27.md`
- 2026-07-27：临时对话设计 → 归档 `docs/archive/chatgpt-temporary-chat-design-2026-07-27.md`
- 2026-08-03：强制联网搜索设计 → 归档 `docs/archive/chatgpt-web-search-design-2026-08-03.md`
