# 核心代理与路由设计

功能域：入站端点、模型路由、协议转换与统一流式。对应正式合同见[配置参考](../configuration.md)与[功能说明](../features.md)；本设计说明最终机制与关键合同。演进历史：早期设计（2026-07-15 Provider Capability Contract）曾以单一 RouteOwner 为路由目标，后演进为按 `priority` 排序的有序候选链与安全回退，见[演进记录](#演进记录)。

## 设计目标

- 客户端只认标准 OpenAI / Anthropic 接口，请求形态不反向定义上游能力。
- 路由只按请求体中的 exact `model` 决定；客户端永远不需要知道服务商是谁。
- 上游失败在"响应尚未提交"的安全窗口内可自动切换，提交后绝不切换。

## 入站端点白名单

Client Protocol 只由 method + path 决定，不从 header 或 body 推断。其它 `/v1/*` 一律 404。

- **OpenAI**：`POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/completions`、`POST /v1/embeddings`，`GET|POST /v1/models`
- **Anthropic**：`POST /v1/messages`
- **扩展**：`POST /v1/search`（ai-proxy 自有搜索端点）、`POST /v1/images/generations|edits`（ChatGPT Web 图片）

`GET/POST /v1/models` 本地合成，不访问上游；只返回模型、容量与 operations，不暴露 provider 名、base URL 或密钥。

## 字段语义

| 字段 | 语义 |
| --- | --- |
| `model_catalog` | 静态模型的权威：模型 ID exact 且严格区分大小写，声明容量与 operations |
| `operations` | 业务合同，当前枚举 `chat_completions`、`embeddings` |
| `protocol` | 仅 `openai` / `anthropic`；必须显式声明，不得按 provider 名推断 |
| `endpoint_capabilities` | 仅表示上游 direct endpoint 能力（`chat_completions` / `messages` / `responses` / `completions` / `embeddings`）；转换派生的可服务 path 不得写回该字段；必填、去重、稳定排序、不允许未知枚举 |

`chatgptweb` 与 `codexoauth` 是保留的内建 Provider ID，禁止写入 `providers`；分别由 `chatgpt_web.enabled` / `codex_oauth.enabled` 在运行时注入。

## 模型路由与候选链

- **两阶段解析**：启动期 `config.Load` 对每个 catalog 条目按 `models` pattern exact 匹配 enabled Provider，校验每个 `operations` 至少有一个候选按固定矩阵可服务，按 `priority` 降序写入 `ResolvedModelRoute.RouteOwners`；校验失败的模型不进 `/v1/models`。请求期 `ResolveTransportPlan` 在同一只读快照上生成 TransportPlan，不修改候选归属。
- **候选链**：一个 exact model 可对应多个 enabled Provider，按 `priority`（`-1000`~`1000`，默认 `100`）降序排列，同优先级按 Provider 名稳定排序。内建 Provider 参与同一候选链：静态默认 `100`，Codex OAuth 默认 `90`，ChatGPT Web 默认 `10` 且不作为回退候选。同名模型保留全部候选，不相互覆盖。
- **回退策略**：仅当客户端响应尚未提交时，对网络错误、`408`、`429`、`5xx` 与流式首事件探测失败，按候选 `fallback: true` 尝试下一项；一次已写出的 SSE/HTTP 响应绝不切换。图片任务一旦提交不回退。
- **健康度与熔断**：5 分钟有界样本窗口；少于 3 个样本为 `unknown`；连续 3 次可重试失败打开 30 秒熔断；路由跳过熔断 / `unhealthy` / `credential_error` 候选。健康度不替代账号池的真实可用性判断。
- **不变量**：不存在 `default_provider`、旧 `fallbacks` 列表、`X-AI-Provider`、`?provider=` 或 `provider/model` 前缀等任何请求侧覆盖手段；客户端认证头不转发上游。

## 转发矩阵

| 客户端 | 上游 protocol | 需要的 capability | 上游 path | 模式 |
| --- | --- | --- | --- | --- |
| `/v1/chat/completions` | openai | `chat_completions` | 同 path | native |
| `/v1/chat/completions` | anthropic | `messages` | `/v1/messages` | `openai_to_anthropic` |
| `/v1/messages` | anthropic | `messages` | 同 path | native |
| `/v1/messages` | openai | `chat_completions` | `/v1/chat/completions` | `anthropic_to_openai` |
| `/v1/responses` | openai | `responses` | 同 path | native |
| `/v1/completions` | openai | `completions` | 同 path | native |
| `/v1/embeddings` | openai | `embeddings` | 同 path | native |

矩阵外组合统一 `endpoint_unsupported`，不得经转换隐式获得；`responses` / `completions` / `embeddings` 不能靠 chat/messages 转换派生。

## 协议转换边界

- 跨协议最低保证为**基础文本**：system/user/assistant 文本、`max_tokens` / `temperature` / `top_p` / `stop` 映射、非流式与基础文本流式、usage 映射。
- tools / function calling、多模态、`response_format` / JSON schema、provider 私有 reasoning 等能力**不能转换派生**，在访问上游前返回 `conversion_unsupported`（typed error），绝不静默删改；若后续候选可原生保留该语义，可改用该候选。
- 转换错误保留上游 HTTP status，但输出客户端协议可解析的安全 envelope；流式期间发现不支持内容记 `outcome=conversion` 并终止流，不切换 provider、不伪造正常终止事件。
- 上游请求头按上游 protocol 的 allowlist 重建（而非全量复制后删减），客户端与 provider 认证/版本头完全隔离（如 `Anthropic-Version` 由代理固定生成）。

## Typed Error 与 Envelope

- 稳定 code：`model_required`、`model_not_found`、`operation_unsupported`、`endpoint_unsupported`、`conversion_unsupported`、`invalid_request`(400)、`authentication_failed`(401)、`request_too_large`(413)、`route_contract_invalid`、`proxy_internal_error`(500)、`provider_unavailable`(503)、`upstream_unavailable`(502，唯一访问上游的 code)。
- envelope 按入站协议输出：OpenAI 带独立 `code`；Anthropic 用 `{"type":"error"}` 且同一 code 前缀写入 message。
- 所有本地拒绝一律走 typed encoder（禁用 `http.Error`）；错误不泄露 API Key、Authorization 或带凭据 URL。

## 统一流式 SSE

- 文本生成统一 SSE 增量输出；标准推理端点仅在请求体 `"stream": true` 时进入流式生命周期，`Accept: text/event-stream` 不能隐式改变请求模式。
- `/v1/chat/completions` 返回 OpenAI Chat Completions SSE（必要时转换 Anthropic 上游事件）；`/v1/messages` 返回 Anthropic Messages SSE（必要时转换 OpenAI 上游事件）。
- 跨协议 SSE 事件统一转换，响应头与边界一致；转换只保证基础文本 delta。
- 首包写出后 HTTP 状态不可改写，真实结束态用 **outcome**（`success`、`client_canceled`、`idle_timeout`、`limit_exceeded`、`upstream_truncated`、`upstream_failed`、`capability_drift`、`incomplete`、`client_write`、`protocol`、`conversion`、`error`）统一写入 DuckDB / Prometheus / `metadata.json`；客户端取消不得计为上游故障。
- 浏览器客户端应使用 `fetch()` + `ReadableStream`（POST + 认证 Header），不使用只支持 GET 的原生 `EventSource`。

## 观测合同

usage / metadata / metrics / SLO 必须消费同一 TransportPlan 的观测上下文：`provider`、`model`、`route`、`outcome`、`conversion_mode` 与 `api_key_id` 一致落盘，保证用量、指标与归档可对账。

## 演进记录

- 2026-07-15：Provider Capability Contract 早期设计（单 RouteOwner、无候选链、operations 枚举含 embedding 预研）→ 归档 `docs/archive/provider-capability-contract-design-2026-07-15.md`
- 2026-07-23：统一流式 SSE 收口 → 归档 `docs/archive/unified-sse-streaming-design-2026-07-23.md`
- 后续：候选链、priority/fallback、健康度与熔断、内建 Provider 合成等能力在既有正式合同中收口，无独立设计文档。
