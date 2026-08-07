# OpenAI Responses 与 Anthropic Messages 双向转换设计

本文定义 ai-proxy 对 OpenAI Responses API 与 Anthropic Messages API 的双向协议转换边界。设计目标是提供可验证的纯文本兼容子集，并对无法保持语义的字段显式拒绝；不把两个协议包装成字段名称相似的“无损互换”。

参考规范：

- [OpenAI Responses API](https://developers.openai.com/api/reference/responses/overview)
- [Anthropic Messages API](https://platform.claude.com/docs/en/api/beta/messages)

## 1. 总体原则

1. Native 优先。若候选 Provider 原生支持客户端协议，永远优先使用 native 路径。
2. 有界转换。转换器只接受在目标协议中可表达且已测试的字段子集。
3. 不静默丢语义。无法表达的字段在访问上游前返回 `conversion_unsupported`。
4. 能力按方向声明。Responses→Anthropic 与 Anthropic→Responses 的支持集合分别维护，不能用一个 `reasoning_supported` 布尔值推导全部能力。
5. 流式与非流式分开验收。事件转换必须有独立状态机和终止事件校验。
6. 不保存原始敏感内容。转换日志只记录模式、忽略/拒绝字段名和耗时，不记录 Authorization、API Key、完整响应正文。

## 2. 转换模式

新增的逻辑模式名称建议为：

```text
responses_to_anthropic
anthropic_to_responses
```

它们与现有 `openai_to_anthropic`、`anthropic_to_openai` 分开。现有 Chat Completions 转换不能直接复用，因为 Responses 使用 `input/output[]` 与 Response 事件模型，而不是 `messages/choices` 模型。

## 3. 支持等级

### Level 1：纯文本兼容子集

双向均支持：

- `model`
- 纯字符串输入
- 纯文本 message-array
- `instructions` ↔ `system`
- `max_output_tokens` ↔ `max_tokens`
- `temperature`、`top_p`（目标模型接受时）
- 非流式纯文本响应

Level 1 不承诺保留 Responses output item 类型或 Anthropic content block 类型，只保证最终文本语义和基本 usage。Level 1 请求不包含流式能力声明；流式请求必须使用 Level 2。

### Level 2：纯文本流式

Level 2 在 Level 1 基础上增加双向纯文本 SSE：首事件探测、首事件/空闲超时、客户端取消、单行与累计输出上限、终止事件和 usage 结算。流式 function tool 事件尚未纳入 Level 2。

### Level 3：function tools（非流式）

Level 3 在 Level 2 基础上增加 function tool 定义、`function_call`/`tool_use`、`function_call_output`/`tool_result` 和稳定 call ID 生命周期的非流式转换。仅允许 JSON 参数与 function 工具；hosted/search/computer/MCP、图片、结构化输出和 continuation 仍必须拒绝。reasoning/thinking 只有配置了方向专用降级适配器时才开放，且不透传推理内容。

## 4. 请求字段映射

| Responses 字段 | Anthropic 字段 | 双向策略 |
| --- | --- | --- |
| `model` | `model` | 直接映射，并以目标 Provider 的实际模型 ID 为准 |
| `input` 字符串 | `messages` 中单个 user 文本 | Level 1 支持 |
| `input` message-array | `messages` | Level 1/2 为 system/user/assistant 纯文本；Level 3 可增加 function call/result item |
| `instructions` | `system` | 直接映射；非字符串拒绝 |
| `max_output_tokens` | `max_tokens` | 直接映射并重新执行目标模型上限校验 |
| `temperature` | `temperature` | 目标模型支持时映射 |
| `top_p` | `top_p` | 目标模型支持时映射 |
| `stream` | `stream` | 进入对应流式转换器 |
| `reasoning` | `thinking` | 仅按方向适配器映射到固定目标 effort；不转换客户端 effort |
| `tools` | `tools` | Level 3 仅支持 function 定义，流式工具事件拒绝 |
| `tool_choice` | `tool_choice` | Level 3 映射 auto/required/none 与指定 function |
| `text.format` | 无通用等价字段 | 拒绝 JSON Schema/结构化输出转换 |
| `previous_response_id` | 无等价字段 | 拒绝；客户端应展开历史 |
| `background` / realtime | 无等价字段 | 拒绝 |

反向转换同理：Anthropic 的 `system`、纯文本 `messages` 和基础采样参数可以投影到 Responses；`thinking` 仅接受 `type=adaptive` 并映射到配置的 Responses effort。`tool_use`、复杂 content block 和 provider-specific beta 字段不得静默折叠成 Responses 普通文本。

## 5. Reasoning 策略

### 5.1 默认拒绝，显式适配才降级

OpenAI Responses 使用：

```json
{"reasoning":{"effort":"low"}}
```

Anthropic 使用 `thinking` 配置和 thinking content block。两者的控制维度和返回结构不同，因此不能把 `effort` 直接改名为 `thinking`，也不能将 `budget_tokens` 反推为 `effort`。

### 5.2 能力声明

模型能力应按方向声明，例如：

```yaml
model_metadata:
  deepseek-v4-flash:
    reasoning_supported: true
    reasoning_efforts: [none, low, high, max]
    conversion_capabilities:
      responses_to_anthropic:
        level: 3
        text: true
        streaming: true
        tools: true
        reasoning: true
        reasoning_adapter: responses_to_anthropic_adaptive
        reasoning_target_effort: low
      anthropic_to_responses:
        level: 3
        text: true
        streaming: true
        tools: true
        reasoning: true
        reasoning_adapter: anthropic_to_responses_effort
        reasoning_target_effort: low
```

`reasoning=true` 必须同时有方向匹配的 adapter 和 `reasoning_target_effort`，目标 effort 必须出现在 `reasoning_efforts`。Responses→Anthropic 的 adapter 只生成 `thinking: {"type":"adaptive"}` 和配置的 `output_config.effort`；Anthropic→Responses 的 adapter 只生成配置的 `reasoning.effort`。客户端指定的 effort 不自动换算，Anthropic manual `thinking: {"type":"enabled","budget_tokens":...}` 也不转换。

没有适配器或适配器不匹配时，在上游请求创建前返回：

```json
{
  "error": {
    "code": "conversion_unsupported",
    "feature": "reasoning",
    "message": "reasoning cannot be represented by this conversion path"
  }
}
```

只有显式方向适配器才可以进行有损映射，并且必须在响应 metadata、Recent Calls 和 usage 记录 `conversion_degraded=true` 与有限字段名。推理块/delta 只被识别后省略，不得写入目标文本或日志正文。

## 6. 响应转换

### 6.1 非流式

Responses→Anthropic：

1. 读取 Response `output[]`；
2. 仅接受单一或可合并的文本 message item；
3. 遇到 reasoning 时，仅在已声明 adapter 下省略并记录 `reasoning_output`；tool call、file、computer 或未知 item 仍拒绝；
4. 输出 Anthropic `content[]` 文本块；
5. 映射终止原因和 usage，未知字段不伪造。

Anthropic→Responses：

1. 读取 Anthropic `content[]`；
2. 接受 text block；已声明 adapter 时省略 `thinking`/`redacted_thinking` block 并记录 `thinking_output`；
3. 生成 Responses message output item；
4. 映射 `stop_reason` 到有限的 Responses status/termination 表达；
5. 无法表达的 stop reason 保留为 bounded metadata，不伪造 `completed`。

### 6.2 流式

每个方向都必须维护独立状态机：

```text
收到开始事件
  → 创建目标协议响应
  → 转发文本 delta
  → 处理 usage/终止原因
  → 收到 completed/stop
  → 关闭并结算
```

必须处理：上游提前 EOF、重复终止事件、delta 超限、客户端取消、目标协议首事件写出失败。已写出目标 SSE 后不得切换候选 Provider。

## 7. Tools 设计

Level 3 只允许 function 工具：

- Responses function tool ↔ Anthropic tool definition；
- Responses function call item ↔ Anthropic `tool_use` block；
- Responses tool output ↔ Anthropic `tool_result` block；
- 保留稳定 call ID；
- 请求历史中的 call/result 必须成对闭合，悬空 call、未知/重复 result 在访问上游前拒绝；
- Anthropic `tool_use` 只允许出现在 assistant message，`tool_result` 只允许出现在 user message；
- 严格限制工具参数为 JSON；
- 不支持 hosted/search/computer/MCP 等 provider-specific 工具的跨协议转换。

如果任一工具字段、并行调用或 tool result 内容无法表示，应返回 `conversion_unsupported`，而不是只转发工具名称。

## 8. 路由与 `/v1/models`

`supported_endpoints` 只能在目标候选存在对应 conversion contract 时包含 `/v1/responses` 或 `/v1/messages`。模型级 `capabilities.reasoning` 不得单独证明跨协议转换能力。

建议在能力扩展中区分：

```json
{
  "capabilities": {
    "reasoning": {"supported": true, "efforts": ["low", "high"]},
    "conversions": {
      "responses_to_anthropic": {"level": 2, "reasoning": true, "reasoning_mode": "degrade", "tools": false},
      "anthropic_to_responses": {"level": 2, "reasoning": true, "reasoning_mode": "degrade", "tools": false}
    }
  }
}
```

## 9. 错误与观测合同

转换失败统一使用 `conversion_unsupported`，并返回：

- `feature`：字段或能力名称；
- `client_endpoint`；
- `upstream_protocol`；
- `conversion_mode`；
- 不包含原始字段值。

Interaction metadata 只记录：

- `conversion_mode`；
- `ignored_features`（含显式 reasoning adapter 省略的有限字段名）；
- `outcome`；
- `error_code`；
- 转换阶段耗时。

未声明 adapter 时，`reasoning`、tools、JSON Schema 等改变语义的字段不得进入 `ignored_features`。已声明 adapter 时，仅允许 `reasoning`、`thinking`、`reasoning_output`、`thinking_output` 这些有限名称用于说明已发生的降级，禁止保存字段值。

## 10. 测试矩阵

每个方向至少覆盖：

- 纯字符串 input/message；
- system/instructions；
- 多轮纯文本消息；
- max tokens 与上下文上限；
- 非流式成功；
- 流式文本成功；
- 上游 EOF、超时、客户端取消；
- reasoning 被拒绝；
- Level 1 tools 被拒绝，Level 3 function tools 成功、双向 HTTP call/result round-trip、悬空 call、未知/重复 result 和角色错置均覆盖；
- JSON Schema 被拒绝；
- `previous_response_id` 被拒绝；
- usage 与 stop reason 有界映射；
- `/v1/models` 只发布实际存在的 conversion capability。

## 11. 分阶段落地

### Phase 1：合同与拒绝

- 增加双向 conversion mode 常量；
- 增加能力声明结构；
- 先实现 Level 1 请求校验并建立后续等级门闩；
- 对 reasoning/tools/structured output 明确拒绝；
- 完成错误和观测合同。

### Phase 2：非流式文本

- 实现双向纯文本请求和响应映射；
- 完成 usage、终止原因和大小限制；
- 增加真实 Provider 集成测试。

### Phase 3：流式文本

- 实现双向 SSE 状态机；（本地协议闭环已完成，仍需真实 Provider 回归）
- 完成双方向 Handler 级取消、EOF、超限、首事件、心跳不续期、下游写失败和终止前 block 闭合测试；
- 在 `/v1/models` 发布通过实现门闩的 Level 1/2 conversion capability。

### Phase 4：基础工具调用（可选）

- 仅 function tools，当前已完成非流式定义/调用/结果映射；
- 独立能力开关和 provider 白名单；
- 通过评测后按 model/direction 显式声明发布，不得由模型原生 tools 能力自动开启。

在对应 Provider/model/direction 的真实回归和灰度完成前，不应把转换能力描述为完整无损互转；当前公开合同仍明确排除图片、结构化输出、continuation 和流式工具事件。reasoning/thinking 仅是显式降级适配，不是无损互转。

## 12. 统一交互中间表示

当转换从纯文本扩展到多模态、工具和流式事件时，不能继续采用字段对字段的直接改写。两种协议应先转换为统一的 `Interaction IR`，再由目标协议编码器生成请求或响应。

```text
Responses Request ──decode──> Interaction IR ──encode──> Anthropic Request
Anthropic Request ──decode──> Interaction IR ──encode──> Responses Request
```

IR 至少包含：

- model、system/instructions、token limits；
- text、image、document、reasoning content blocks；
- tools、tool calls、tool results 与稳定 call ID；
- reasoning policy、response format、stream policy；
- continuation state、usage 和 termination reason。

IR 必须保留来源协议和原始语义等级，不能用一个字符串字段承载不同协议的 reasoning 或终止原因。

## 13. 方向化能力协商

`reasoning_supported` 只描述模型自身能力，不能代表跨协议转换能力。模型元数据应支持按转换方向声明能力：

```yaml
conversion_capabilities:
  responses_to_anthropic:
    level: 2
    text: true
    images: false
    documents: false
    reasoning: true
    reasoning_adapter: responses_to_anthropic_adaptive
    reasoning_target_effort: low
    tools: false
    structured_output: false
    streaming: true
    continuation: false
  anthropic_to_responses:
    level: 2
    text: true
    images: false
    documents: false
    reasoning: true
    reasoning_adapter: anthropic_to_responses_effort
    reasoning_target_effort: low
    tools: false
    structured_output: false
    streaming: true
    continuation: false
```

请求规划必须使用“客户端协议 + 请求能力 + 候选 Provider 能力 + 转换方向能力”共同筛选候选。能力不匹配的候选应被跳过；所有候选均不匹配时返回 `conversion_unsupported`，不能伪装为 provider unavailable。

`/v1/models` 可在扩展字段中返回：

```json
{
  "capabilities": {
    "reasoning": {"supported": true, "efforts": ["low", "high"]},
    "conversions": {
      "responses_to_anthropic": {"level": 2, "reasoning": true, "reasoning_mode": "degrade", "tools": false},
      "anthropic_to_responses": {"level": 2, "reasoning": true, "reasoning_mode": "degrade", "tools": false}
    }
  }
}
```

## 14. Reasoning 适配器

reasoning 应由独立 provider/方向适配器处理，而不是由通用转换器改名：

```go
type ReasoningAdapter interface {
    ResponsesToAnthropic(ReasoningPolicy) (AnthropicThinking, error)
    AnthropicToResponses(AnthropicThinking) (ReasoningPolicy, error)
}
```

适配策略分为：

- `preserve`：目标协议存在已验证的等价语义；
- `degrade`：明确降低语义，并记录 `conversion_degraded`；
- `reject`：无法安全表示时返回 `conversion_unsupported`。

默认策略必须是 `reject`。只有 provider-specific 配置显式允许时，才可以使用 `degrade`。

## 15. 多模态与安全边界

IR 应将 Responses 的 `input_text`、`input_image`、`input_file` 与 Anthropic 的 text/image/document block 映射为统一内容块。音频、computer use 和没有目标协议等价物的内容默认拒绝。

所有二进制和远程资源必须经过：

- MIME 白名单；
- 请求及单文件大小上限；
- data URI 解码上限；
- 远程 URL SSRF 与重定向校验；
- 不在日志、usage 或 interaction metadata 中保存正文。

## 16. 工具生命周期

工具转换必须保留完整生命周期，而不仅是工具定义：

```text
assistant tool call
  → client executes tool
  → tool result
  → assistant continuation
```

第一阶段只允许 function tools，并要求保留工具名、JSON 参数、稳定 call ID、并行关系和错误结果。hosted tools、web search、computer use、MCP 和 provider-specific tools 必须单独声明能力，未声明时拒绝。

## 17. 流式事件 IR

双向 SSE 转换应先统一为事件 IR：

```text
StreamStarted
MessageStarted
TextDelta
ReasoningDelta
ToolCallStarted
ToolCallDelta
ToolResult
Usage
Completed
Failed
```

每个方向实现独立 encoder。必须覆盖首事件超时、中途 EOF、重复终止、delta 超限、tool JSON 分片、reasoning 分片和客户端取消。目标 SSE 已写出后禁止切换候选 Provider。

## 18. 多轮状态与结构化输出

没有统一持久化状态时，`previous_response_id` 不得伪装为 Anthropic 历史；Anthropic 历史也不得隐式变成 Responses session。无状态转换要求客户端提供完整历史；有状态转换必须由 ai-proxy 明确拥有 conversation state。

`text.format`、JSON object 和 JSON Schema 必须区分处理。只有目标协议和目标模型都声明等价结构化输出能力时才允许转换；strict schema、递归 schema 或无法表达的关键字必须返回 `conversion_unsupported`，不能把 schema 拼进 prompt 后宣称仍然结构化。

## 19. 错误、usage 与观测

转换层统一错误类别：

```text
invalid_request
conversion_unsupported
conversion_degraded
upstream_protocol_error
upstream_timeout
client_canceled
```

记录 `client_status`、`upstream_status`、`conversion_mode`、`conversion_degraded`、`ignored_features`、`unsupported_features` 和转换耗时。不得记录字段值、密钥或完整正文。

无法精确换算的 reasoning token、cached token、tool token 或 provider-specific usage 必须标记 `estimated=true`，不得伪造精确账单数字。

## 20. 增补测试矩阵

除 Level 1 基础测试外，还必须增加：

- IR 往返 round-trip 测试，验证可保留字段不变；
- 方向能力协商和候选过滤测试；
- reasoning preserve/degrade/reject 三种策略测试；
- image/document MIME、大小和 SSRF 测试；
- function tool 多轮、并行、错误 result 和稳定 call ID 测试；
- SSE 事件 IR 双向编码、断流、重复终止和取消测试；
- previous response/history 状态边界测试；
- JSON Schema 关键字和 strict 行为拒绝测试；
- usage 不可精确映射时的 estimated 标记测试；
- `/v1/models` 不发布未满足 conversion contract 的 endpoint 测试。

## 21. 更新后的落地顺序

1. 建立 Interaction IR 和方向化能力结构；默认仍只发布显式声明且通过实现门闩的等级。
2. 将现有 Chat Completions↔Anthropic 转换迁移到 IR，保持行为兼容。
3. 实现 Responses↔Anthropic 非流式纯文本转换。
4. 增加候选能力过滤、错误合同和 `/v1/models` conversion 能力发现。
5. 实现双向纯文本 SSE 状态机。（当前直接转换状态机、失败分类和唯一结算已完成；迁移到统一事件 IR 与真实 Provider 回归仍待完成。）
6. 增加安全多模态内容块。
7. 在 provider 白名单下增加 function tools。（当前完成非流式 function 合同，继续做真实多轮评测。）
8. reasoning/thinking 的 provider-specific 降级适配已落地；继续评估 structured output 和 continuation。

在每一阶段完成真实 Provider 集成测试和回归评测前，不得提升公开的 conversion level，也不得把有损路径标记为 native。reasoning adapter 即使开放，也只能标记为 `reasoning_mode: "degrade"`。

## 22. 当前实现差距

截至本文编写时，代码已具备 OpenAI Chat Completions↔Anthropic Messages 的部分转换能力、Responses↔Anthropic 的文本/SSE/function-tools 转换，以及 Responses 的 native、ChatGPT Web 和 Codex OAuth 路径；以下能力仍属于设计目标，不能在 `/v1/models` 中宣称已支持：

| 能力 | 当前状态 | 交付条件 |
| --- | --- | --- |
| Responses→Anthropic | Level 1 文本、Level 2 文本 SSE、Level 3 function tools 非流式；显式 adaptive reasoning 降级适配已实现 | 继续补齐图片、structured output、continuation；流式工具仍拒绝 |
| Anthropic→Responses | Level 1 文本、Level 2 文本 SSE、Level 3 function tools 非流式；显式 adaptive thinking 降级适配已实现 | 继续补齐图片、structured output、continuation；manual thinking 与流式工具仍拒绝 |
| Interaction IR | 未实现 | IR round-trip 和版本测试通过 |
| 方向化 conversion capability | Level 1/2/3 模型实现门闩、Provider 发布门闩和 `/v1/models` 投影已接入 | 两层声明同时通过后才进入候选；继续做自动灰度和高级能力过滤 |
| 双向文本 SSE | 状态机、首事件/空闲超时、双方向 Handler 级取消、EOF/截断、下游写失败、输出上限、多 text block、终止校验和失败唯一结算已实现 | 真实上游流式测试与归档证据 |
| 双向 function tools | 非流式 function 定义/call/result、request-local 闭合生命周期、角色约束、双向 HTTP round-trip、字段白名单、schema/参数/result 预算已实现 | 多轮跨请求 session 状态、并行工具和真实 Provider 评测 |
| reasoning 跨协议适配 | 仅允许显式 adapter；请求控制映射、推理输出省略、SSE 状态和降级审计已实现 | 绑定真实 Provider 灰度证据后再扩大模型声明 |
| 严格字段拒绝 | 顶层未知字段、并行工具控制、metadata/service tier、stop_sequences、provider-specific tool 字段在转换前拒绝 | 若新增字段，必须先加入目标协议等价映射和回归测试 |
| 转换观测 | archive、usage、Recent Calls 与 Prometheus 已记录有界的 mode/level/protocol/status/duration/degraded/estimated/feature；首次完成门闩防止 conversion 指标重复结算 | 仍需绑定真实 Provider 灰度证据和告警阈值 |

在这些项目完成前，现有 `supported_endpoints` 不得因为存在模型级 reasoning 能力而自动增加跨协议端点。

## 23. 正式配置 schema

当前实现使用两层门闩：exact model metadata 描述转换器对该模型的实现上限，Provider 的 `conversion_releases` 描述具体 `provider/model/direction` 是否完成真实验证并允许发布。两层必须同时通过；任一层未声明都等同关闭。

```yaml
model_metadata:
  model-id:
    context_window_tokens: 1000000
    max_output_tokens: 128000
    reasoning_supported: true
    reasoning_default_effort: low
    reasoning_efforts: [none, low, high, max]
    conversion_capabilities:
      responses_to_anthropic:
        level: 2
        text: true
        images: false
        documents: false
        reasoning: true
        reasoning_adapter: responses_to_anthropic_adaptive
        reasoning_target_effort: low
        tools: false
        structured_output: false
        streaming: true
        continuation: false
      anthropic_to_responses:
        level: 2
        text: true
        images: false
        documents: false
        reasoning: true
        reasoning_adapter: anthropic_to_responses_effort
        reasoning_target_effort: low
        tools: false
        structured_output: false
        streaming: true
        continuation: false

providers:
  anthropic:
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    models: [model-id]
    endpoints: [messages]
    conversion_releases:
      model-id:
        responses_to_anthropic:
          enabled: true
          verified: true
          evidence_id: provider-eval-2026-08-07
```

字段规则：

- `level` 只允许 `0`、`1`、`2`、`3`；未声明等同于 `0`；
- `level=0` 不发布转换能力；
- `level>=1` 必须声明 `text=true`；
- `reasoning=true` 必须同时有已验证的 reasoning adapter；
- `tools=true` 只允许在 function tools 合同完成后启用；
- `streaming=true` 必须通过双向 SSE 状态机测试；
- `default_effort` 必须属于同一模型的 `efforts`；
- 未知字段默认拒绝，避免配置拼写错误导致能力误发布。

Provider 发布记录规则：

- model key 必须是区分大小写的 exact model ID，且被该 Provider 的 `models` 匹配；
- direction 只允许 `responses_to_anthropic`、`anthropic_to_responses`；
- `enabled=true` 必须同时有 `verified=true`；`verified=true, enabled=false` 可保存验证证据但不发布；
- 方向必须与 Provider protocol/endpoints 兼容，并存在可实现的模型级 capability；
- `evidence_id` 是可选的有限审计引用，不得写入 API key、请求正文或其他秘密；
- 未声明 Provider 发布记录时，旧配置按关闭处理，不做隐式兼容开启。

`level` 是运维声明的“该转换方向经过验证的兼容等级”，不是业务请求字段，也不是 reasoning 强度。业务请求不携带 `level`；ai-proxy 根据请求实际使用的能力自动筛选候选：纯文本非流式要求 `level>=1`，纯文本流式要求 `level>=2`，function tools 或图片要求 `level>=3` 且对应能力为 true。未做真实验证时不配置，等同 `level=0`；只有完成对应 Provider/model/direction 的真实测试后才能提升等级。

建议等级语义如下：

| Level | 语义 |
| --- | --- |
| `0` | 未声明、未验证或不支持转换 |
| `1` | 纯文本、system/instructions、token 限制、基础非流式 |
| `2` | Level 1 加纯文本 SSE、首事件、终止事件和 usage |
| `3` | Level 2 加已验证的 function tools 非流式；图片仍需独立安全合同，不得仅凭 level=3 发布 |

`level` 与 `reasoning.effort` 独立：前者描述转换成熟度，后者描述模型推理策略。不得因为模型支持某个 reasoning effort 就自动提高 conversion level。

当前实现的有效门闩为：

```text
model implementation capability
    AND provider + exact model + direction verified release
    > published candidate

任一条件不满足
    > conversion_unsupported / 不出现在 supported_endpoints 与 capabilities.conversions
```

## 24. 候选过滤算法

请求规划必须在选择 Provider 前完成能力过滤：

```text
1. 解析客户端协议、endpoint 和请求字段。
2. 提取请求能力：text/images/tools/reasoning/structured_output/streaming/continuation。
3. 获取模型的 native 候选和 conversion 候选。
4. native 候选优先检查原生协议能力。
5. conversion 候选检查方向化 `conversion_capabilities`。
6. 检查具体 Provider 的 exact model/direction 发布记录同时满足 enabled 与 verified。
7. 过滤任一必需能力为 false/unknown 的候选。
8. 对剩余候选执行健康度、优先级和 fallback 排序。
9. 没有候选时返回 conversion_unsupported 或 endpoint_unsupported。
```

能力不匹配必须与 Provider 健康失败区分：

- `conversion_unsupported`：请求语义无法由候选表示，HTTP 400；
- `endpoint_unsupported`：模型没有对应客户端端点，HTTP 400/404；
- `provider_unavailable`：存在兼容候选但全部不健康，HTTP 503；
- `upstream_failed`：已访问兼容上游但请求失败，HTTP 502 或透传状态。

## 25. 版本与混部兼容

转换合同应具有独立版本：

```text
ir_version: 1
conversion_contract_version: 1
capabilities_schema_version: 1
```

版本升级规则：

- 新增可选字段可向后兼容；
- 改变字段语义必须提升版本；
- 不识别更高版本的节点不得宣称对应 conversion level；
- 混部期间只发布所有节点都支持的最低 level；
- 热更新失败必须保留旧配置和旧能力快照。

## 26. 安全与资源预算

转换器必须单独施加预算，不得只依赖入站 HTTP body 限制：

- 转换前后请求字节上限；
- 输入、输出和 schema token 预算；
- content block 数量和嵌套深度；
- 单个 tool call 参数大小和总数量；
- SSE 单行、单事件和累计输出上限；
- data URI 解码后大小上限；
- 远程资源连接、读取、重定向和私网地址限制。

转换日志只能保存字段名、模式、错误代码和计时，不保存请求正文、工具参数、thinking 内容、Authorization 或 API Key。

## 27. 超时与取消合同

必须独立记录和配置以下阶段：

```text
client_total_timeout
upstream_connect_timeout
upstream_header_timeout
upstream_body_idle_timeout
stream_first_event_timeout
conversion_processing_timeout
tool_result_wait_timeout
```

Level 3 function tools 还施加独立预算：工具 schema 最大 256 KiB、schema 嵌套深度 32、单个 tool 参数/结果最大 1 MiB、工具定义最多 128 个、输入/content block 最多 256 个；超限在访问上游前返回 `conversion_unsupported`，不依赖通用请求体上限。请求结束时仍未解析的 call、未知或重复 result，以及与 Anthropic message role 不匹配的 tool block 同样必须在访问上游前拒绝。

非流式转换若上游返回 `text/event-stream` 会立即以 `upstream_protocol_error` 结束，并关闭响应体；不会把 SSE 当作 JSON 缓冲等待 EOF。普通 JSON 响应读取同样受上游 body idle timeout 与客户端取消控制。

客户端取消时必须取消上游 context，并将 usage outcome 记录为 `client_canceled`；未写出客户端响应时观测状态使用 499。取消不得继续遍历 fallback、增加 upstream error 或降低 Provider 健康度，转换器也不得继续等待上游 EOF。响应头已返回但 body 空闲超时，应同时保留 `upstream_status` 与最终客户端状态，避免把 200 响应头误报为连接失败。下游写失败使用 `client_write`，不得归类为上游协议错误。

## 28. 统一观测字段

Recent Calls、interaction metadata 和 metrics 应使用一致的有界字段：

```text
conversion_mode
conversion_level
conversion_degraded
unsupported_features
ignored_features
client_protocol
upstream_protocol
upstream_status
conversion_duration_ms
estimated
```

`ignored_features` 在显式 reasoning adapter 下允许记录被省略的 `reasoning`、`thinking`、`reasoning_output`、`thinking_output` 字段名，但不得记录字段值；未配置 adapter 时，这些字段必须进入 `unsupported_features` 或错误响应。

Prometheus 使用以下低基数指标，不把错误文本、请求正文或任意 feature 值作为 label：

```text
ai_proxy_conversion_requests_total
ai_proxy_conversion_duration_seconds_sum
ai_proxy_conversion_duration_seconds_count
ai_proxy_conversion_features_total
```

主 conversion 指标只使用 provider、model、client/upstream protocol、mode、level、upstream status、degraded 和 estimated；feature 指标只接受固定白名单，未知值统一收敛为 `_other`，同一次结算中的重复 feature 只计一次。转换首事件前失败、流中失败和正常完成都通过 usage completion 门闩只记录一次 conversion observation。

Level 2 流式失败使用稳定分类：`client_canceled`、`idle_timeout`、`limit_exceeded`、`upstream_truncated` 和 `protocol`。SSE comment、空行与单独的 `event:` 行不构成有效首事件，也不得重置首事件或协议事件空闲计时器。

## 29. 真实 Provider 验证表

每个公开 conversion capability 必须绑定真实请求证据：

| Provider | 方向 | 最小验证 |
| --- | --- | --- |
| OpenAI Responses | native | 文本、流式、reasoning、usage |
| Anthropic Messages | native | 文本、流式、thinking、usage |
| DeepSeek Responses | native | `reasoning.effort` 枚举和长响应 |
| ChatGPT Web Responses | projection | 文本、受限 reasoning、SSE |
| Codex OAuth Responses | native relay | 文本、工具、SSE |

每条证据至少包含请求字段摘要、响应事件类型、最终状态、耗时、usage 和不支持字段；不得将 API key 或完整正文写入证据。

## 30. 灰度、熔断与回滚

转换能力默认关闭，按 `provider/model/direction` 通过 `conversion_releases` 灰度开启。Provider 管理热更新会原子重建有效目录，因此 `/v1/models` 与请求路由使用同一代发布状态。灰度期间监控：

- conversion error rate；
- upstream 400/422 rate；
- p95/p99 header、first-event 和 total latency；
- stream truncation；
- tool call parse failure；
- estimated usage rate；
- client cancellation rate。

超过阈值时关闭对应 Provider release，保留模型实现声明和 native 候选；若无 native 候选则明确返回 `conversion_unsupported`。当前支持 Admin 手工热回滚，自动阈值回滚仍待实现。配置回滚必须恢复旧 runtime snapshot、旧 `/v1/models` 能力输出和旧路由候选，不能只回滚 YAML 文件。
