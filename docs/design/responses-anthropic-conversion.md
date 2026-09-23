# OpenAI Responses 与 Anthropic Messages 双向转换设计

## 2026-09-23 隔离候选与分层验收

生产默认继续使用回退后的历史 system 合并映射。`test/001367–001369` 的小请求验证只证明协议可调用；指令冲突样本返回 AMBER 而非 SAPPHIRE，不能以 200/success 宣称语义通过，28–96 输入 Token 的零缓存也不作为缓存缺陷证据。

候选构造仅在 `conversion_cache_experiment_test.go` 中：`system` / `developer` 使用结构化 `input_text`，保留历史位置与文本；`developer` 是待验证的优先级映射，不宣称与原 system 语义等价。生产 builder 的历史消息投影参数固定为 nil；测试通过包内实例注入调用同一转换实现，无配置、环境变量或请求头能在生产选择候选。缓存键、工具集合、账号策略与统计口径均不改。

验收分层：

1. 普通离线测试验证真实 `/v1/messages` handler、候选输入前缀不变、工具配对、原数据不变；模拟终态不构成上游接受证据。
2. 显式 live 测试先重复普通请求、指令冲突、后续指令覆盖和工具历史场景。HTTP 200 必须同时具有 message_start/message_stop 且无错误事件；输出必须符合预期。失败立即停止，不自动回退或改写重试。
3. 前两层通过后，发送长合成前缀及四轮增量历史，分别记录输入、缓存读取和 known 标志。未知缓存、短样本、失败流不参与效果判定。零命中保留为已知零，不算“通过优化”；需对照 merged 与候选的匹配样本，不承诺固定命中率。

live 工具使用测试 executor：合成 Anthropic 请求先进入本地真实 handler，再将实际转换产物经指定 relay 的原生 `/v1/responses` 端口交给 Codex OAuth，最后由本地 handler 投影回 Anthropic。这是额外一跳的隔离实验，不是生产 claude-owner 会话回放；relay 的身份收敛和账号选择仍可能影响缓存。上线前必须按 run_id 核对最终出站归档及 provider/model，不将工具的成功退出视为自动上线批准。

示例（先建立 SSH loopback 隧道；Key 只通过环境注入，不写入命令示例或仓库）：

```bash
ssh -N -p 1212 -L 18080:127.0.0.1:8080 root@x600.muidea.com
# 另一终端，预先安全设置 AETHERRELAY_CACHE_API_KEY
AETHERRELAY_CACHE_LIVE=1 \
AETHERRELAY_CACHE_BASE_URL=http://127.0.0.1:18080 \
AETHERRELAY_CACHE_CANDIDATE=merged \
go test ./internal/modules/application/proxyapi/service/proxy -run '^TestLiveAnthropicCacheExperiment$' -count=1 -v
```

候选可分别指定 `system` 或 `developer`。未知模式直接拒绝；只允许 HTTPS 或本机 loopback，不跟随重定向，不打印 Key、响应原文或凭据。每次最多 12 个请求，单次 45 秒超时，只使用合成内容，长样本会产生实际 Token 用量。普通 `go test` 跳过 live 测试。

## 2026-09-23 历史 system 消息与缓存前缀

**线上验证失败，历史角色优化已撤回。** `001275/001276` 在 input 含 16 条 system 消息时被 Codex OAuth 返回 HTTP 400；此前 `001274` 使用合并指令映射成功。缺少当时的原始错误正文，不能断定是角色还是排列约束。标准 Responses 文档与模拟上游测试不构成 Codex OAuth 的接受证据。恢复历史 system 文本依次拼入 instructions 的旧映射，保留全部文本及工具配对，暂时接受前缀变化；不自动切换 developer/user，不对任意 400 改写重试。新的保序映射须单独完成真实端点验证后启用。

`claude-owner → gpt-6-luna` 的连续归档显示：预算 system 消息被拼入顶层 instructions 后，缓存读取多次停在 17920；指令不变时可恢复到约 97%。同一缓存键也存在不同工具/指令组合，不能将模型级汇总视为单一对话的缓存效果。

- 当前顶层 `system` 与历史 `messages[].role=system` 文本均按旧映射形成 `instructions`；保留原始请求用于排障。历史 system 为网关兼容扩展，不宣称 Anthropic 原生消息角色包含 system。
- 保留完整预算及其它文本，不识别特定 XML 标记后删除，不降为 user，不更改工具调用/结果配对。非文本 system 块继续显式拒绝。撤回映射后不宣称保留历史 system 的位置或稳定顶层指令。
- [OpenAI Responses 迁移文档](https://developers.openai.com/api/docs/guides/migrate-to-responses)支持用兼容的消息项保留历史系统指令，但本次 Codex OAuth 实测未通过；回归测试改为保护恢复后的映射，不以模拟端点成功替代线上验证。

错误观测：Codex HTTP complete/start/compact 的非 2xx 响应记录安全的 type/code/param/message，并识别字符串 error/detail；元数据保留 error_body_format、error_body_truncated、error_body_read_failed。仅完整正文归档开启时写入逐尝试 `upstream_response_NNN.error.body.txt`；默认是明确标记 redacted 的安全投影，显式不脱敏时保留最多 64 KiB 原文。未知结构/HTML 不在普通日志输出；读失败和截断不可伪装为完整正文。WS 握手和成功 HTTP 内的 SSE 错误仍沿用现有安全错误通道，不宣称此次覆盖其原始正文。
- 缓存键和账号路由规则保持不变。Debug 日志 `Codex conversion cache summary` 包含 request_id/model、instructions_digest、tools_digest、prompt_cache_key_digest、prompt_cache_key_source、input_items、history_system_messages，仅为转换侧结构摘要；不输出正文或原始身份，不作为路由键，也不推断分支身份。
- Live 验证限定 `claude-owner → gpt-6-luna`，按工具/指令摘要和已有客户端身份分组，对照连续调用；工具变更、新分支、上游缓存状态引起的未命中不得伪装为成功命中。

## 2026-09-23 模型能力配置核对

同批目录确认 `gpt-6-sol`：默认推理档位 `medium`，支持 `[low, medium, high, xhigh, max, ultra]`，上下文窗口 272000 / 最大 872000。本地配置、模板及 x600 配置同步补齐，未推测输出上限，运行结果待重新加载配置后验证。

`claude-owner/001005` 的 `gpt-6-luna` 请求携带 `thinking.type=adaptive` 和 `output_config.effort=max`，因线上缺少该精确模型的推理元数据而在本地拒绝。依据 Codex 0.155.1 模型目录缓存（2026-09-23T02:00:14Z），配置模板与 x600 配置补充 `reasoning_supported=true`、默认 `medium`、档位 `[low, medium, high, xhigh, max]`，以及目录声明的上下文窗口 272000 / 最大 872000；未推测输出上限。此次仅刷新配置文件，未重启服务，运行生效及上游成功仍需 live 验证。

## 2026-09-22 运行日志收口

流式 usage 按字段合并累计值：`message_delta` 中出现的输入、输出和缓存计数替换旧值，缺失字段保留；合并后重新计算含缓存输入总量。重复事件不累加，缓存计数晚到或修正时同步更新统计分母和归档报文，显式零仍是已知值。

- Codex 响应侧 reasoning 的处理独立于请求侧 thinking：按已有降级合同省略并记录 reasoning_output，保留正文、工具和 usage，不因客户端未声明 thinking 而拒绝整个响应。
- Codex 接入将 output_config.format 的 json_schema 映射为 Responses text.format，保留 schema，补齐 name 和 strict。独立 effort 必须按目标模型声明校验，不静默降级到其他值。
- 本地响应转换失败使用 conversion_response_error 并记录 conversion outcome，不记入上游故障率。字段拒绝携带字段位置；内容块数量上限仍保留。
- Anthropic 流已提交后遇到失败发送 error 事件，不发送成功 message_stop；归档保留该错误终态。
- 首事件超时保留504与重试提示，账号反馈为 availability-neutral，不触发 provider 熔断。账号级限流、额度耗尽及真实网络/上游失败继续按各自规则处理。

本文定义 AetherRelay 对 OpenAI Responses API 与 Anthropic Messages API 的双向协议转换边界。设计目标是提供可验证的纯文本兼容子集，并对无法保持语义的字段显式拒绝；不把两个协议包装成字段名称相似的“无损互换”。

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
| 无原生等价字段 | `stop_sequences` | Anthropic→Responses/Codex 有界本地输出控制，显式降级；不进入上游正文，真实 usage 不改写，详见 13.2.0 收口 |
| `reasoning` | `thinking` | 显式 `effort=none` 映射 `thinking.type=disabled`；其他值按方向适配器映射到固定目标 effort；不转换客户端具体 effort |
| `tools` | `tools` | Level 3 仅支持 function 定义，流式工具事件拒绝 |
| `tool_choice` | `tool_choice` | Level 3 映射 auto/required/none 与指定 function |
| `text.format` | 无通用等价字段 | 拒绝 JSON Schema/结构化输出转换 |
| `previous_response_id` | 无等价字段 | 拒绝；客户端应展开历史 |
| `background` / realtime | 无等价字段 | 拒绝 |

反向转换同理：Anthropic 的 `system`、纯文本 `messages` 和基础采样参数可以投影到 Responses；`thinking` 仅接受 `type=adaptive` 并映射到配置的 Responses effort。省略 `thinking` 时，若 exact model 的 `reasoning_efforts` 明确包含 `none`，转换器显式生成 `reasoning.effort=none`，避免目标模型的默认 reasoning 模式改变工具选择语义。`tool_use`、复杂 content block 和 provider-specific beta 字段不得静默折叠成 Responses 普通文本。

## 5. Reasoning 策略

### 5.1 默认拒绝，显式适配才降级

OpenAI Responses 使用：

```json
{"reasoning":{"effort":"low"}}
```

Anthropic 使用 `thinking` 配置和 thinking content block。两者的控制维度和返回结构不同，因此不能把 `effort` 直接改名为 `thinking`，也不能将 `budget_tokens` 反推为 `effort`。

Responses→Anthropic 仅对显式 `reasoning.effort=none` 做关闭语义映射；该请求不会携带 `output_config`。其余 effort 统一采用 capability 声明的 adaptive 目标值。DeepSeek Anthropic 上游在 thinking 开启时拒绝命名 `tool_choice`，命名工具调用必须显式选择 `none`。

### 5.2 能力声明

模型能力按 exact model 与上游 endpoint 选择固定模板。方向由 endpoint 推导：`messages` 对应 Responses→Anthropic，`responses` 对应 Anthropic→Responses：

```yaml
model_metadata:
  deepseek-v4-flash:
    reasoning_supported: true
    reasoning_default_effort: low
    reasoning_efforts: [none, low, high, max]
    conversion_capabilities:
      messages:
        profile: level3_reasoning
      responses:
        profile: level3_reasoning
```

`level3_reasoning` 自动展开 Level 3 的 text、streaming、tools 和方向匹配的 reasoning adapter，并使用模型 `reasoning_default_effort` 作为目标 effort。Responses→Anthropic 的 adapter 对显式 `none` 生成 `thinking: {"type":"disabled"}`，对其他 effort 生成 `thinking: {"type":"adaptive"}` 和默认 effort；Anthropic→Responses 的 adapter 生成固定的 `reasoning.effort`。客户端指定的其他 effort 不自动换算，Anthropic manual `thinking: {"type":"enabled","budget_tokens":...}` 也不转换。

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

在对应 model+upstream endpoint 的真实回归和灰度完成前，不应把转换能力描述为完整无损互转；当前公开合同仍明确排除图片、结构化输出、continuation 和流式工具事件。reasoning/thinking 仅是显式降级适配，不是无损互转。

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

`reasoning_supported` 只描述模型自身能力，不能代表跨协议转换能力。模型元数据按上游 endpoint 选择模板：

```yaml
conversion_capabilities:
  messages:
    profile: level2_reasoning
  responses:
    profile: level2_reasoning
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

没有统一持久化状态时，`previous_response_id` 不得伪装为 Anthropic 历史；Anthropic 历史也不得隐式变成 Responses session。无状态转换要求客户端提供完整历史；有状态转换必须由 AetherRelay 明确拥有 conversation state。

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
| endpoint conversion profile | exact model + upstream endpoint 固定模板和 `/v1/models` 投影已接入 | 继续做高级能力过滤和模板级灰度 |
| 双向文本 SSE | 状态机、首事件/空闲超时、双方向 Handler 级取消、EOF/截断、下游写失败、输出上限、多 text block、终止校验和失败唯一结算已实现 | 真实上游流式测试与归档证据 |
| 双向 function tools | 非流式 function 定义/call/result、request-local 闭合生命周期、角色约束、双向 HTTP round-trip、字段白名单、schema/参数/result 预算已实现 | 多轮跨请求 session 状态、并行工具和真实 Provider 评测 |
| reasoning 跨协议适配 | 仅允许显式 adapter；请求控制映射、推理输出省略、SSE 状态和降级审计已实现 | 绑定真实 Provider 灰度证据后再扩大模型声明 |
| 严格字段拒绝 | 顶层未知字段、并行工具控制、metadata/service tier、provider-specific tool 字段在转换前拒绝；stop_sequences 按 13.2.0 有界本地控制处理 | 若新增字段，必须先加入目标协议映射或显式降级合同和回归测试 |
| 转换观测 | archive、usage、Recent Calls 与 Prometheus 已记录有界的 mode/level/protocol/status/duration/degraded/estimated/feature；首次完成门闩防止 conversion 指标重复结算 | 仍需绑定真实 Provider 灰度证据和告警阈值 |

在这些项目完成前，现有 `supported_endpoints` 不得因为存在模型级 reasoning 能力而自动增加跨协议端点。

## 23. 正式配置 schema

当前实现以 `exact model + upstream endpoint` 作为唯一转换能力键，不绑定 Provider。Provider 只声明连接、认证、模型和原生 endpoints；所有提供同一 exact model 和 endpoint 的候选共享同一固定模板。切换 Provider endpoint 会立即重新匹配模板，不保存发布门闩或 dormant 状态。

```yaml
model_metadata:
  model-id:
    context_window_tokens: 1000000
    max_output_tokens: 128000
    reasoning_supported: true
    reasoning_default_effort: low
    reasoning_efforts: [none, low, high, max]
    conversion_capabilities:
      messages:
        profile: level2_reasoning
      responses:
        profile: level2_reasoning

providers:
  anthropic:
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    models: [model-id]
    endpoints: [messages]
```

字段规则：

- endpoint 只允许 `messages`、`responses`；
- profile 只允许 `level1`、`level2`、`level2_reasoning`、`level3`、`level3_reasoning`；
- Level 1 自动展开非流式 text；Level 2 增加 text SSE；Level 3 增加非流式 function tools；
- `_reasoning` 模板要求模型支持 reasoning 且声明 `reasoning_default_effort`，adapter 按 endpoint 自动选择；
- `reasoning_default_effort` 必须属于同一模型的 `reasoning_efforts`；
- 未知字段默认拒绝，避免配置拼写错误导致能力误发布。

固定 profile 展开合同：

| Profile | Level | text | streaming | tools | reasoning | 其他高级能力 |
| --- | ---: | --- | --- | --- | --- | --- |
| `level1` | 1 | true | false | false | false | false |
| `level2` | 2 | true | true | false | false | false |
| `level2_reasoning` | 2 | true | true | false | true | false |
| `level3` | 3 | true | true | true | false | false |
| `level3_reasoning` | 3 | true | true | true | true | false |

“其他高级能力”包括 `images`、`documents`、`structured_output` 和 `continuation`，当前所有 profile 均固定为 false；流式 tools 也不因 `streaming=true` 与 `tools=true` 同时出现而开放。`_reasoning` profile 对外发布 `reasoning_mode: degrade`，并按 endpoint 自动展开：

| Upstream endpoint | 转换方向 | Reasoning adapter | 目标 effort |
| --- | --- | --- | --- |
| `messages` | `responses_to_anthropic` | `responses_to_anthropic_adaptive` | 模型 `reasoning_default_effort` |
| `responses` | `anthropic_to_responses` | `anthropic_to_responses_effort` | 模型 `reasoning_default_effort` |

profile 表示该 model+endpoint 已完成对应语义等级验证，不是功能开关的集合。后续若要开放图片、structured output、continuation 或流式 tools，必须新增 profile 或提升 capability schema 版本，不能改变现有 profile 的既有含义。

Provider 规则：

- Provider 不保存转换配置；
- 候选 exact model 命中 metadata，且当前 endpoint 存在模板时才形成转换候选；
- 同一 model+endpoint 在不同 Provider 上得到相同能力；Provider 优先级、健康度和 fallback 只影响候选选择。

`level` 是 profile 展开后的只读兼容等级，不是业务请求字段，也不是 reasoning 强度。业务请求不携带 `level`；AetherRelay 根据请求实际需要的能力筛选候选。未完成 model+endpoint 验证时不配置 profile，等同 `level=0`。

建议等级语义如下：

| Level | 语义 |
| --- | --- |
| `0` | 未声明、未验证或不支持转换 |
| `1` | 纯文本、system/instructions、token 限制、基础非流式 |
| `2` | Level 1 加纯文本 SSE、首事件、终止事件和 usage |
| `3` | Level 2 加已验证的 function tools 非流式；图片仍需独立安全合同，不得仅凭 level=3 发布 |

`level` 与 `reasoning.effort` 独立：前者描述转换成熟度，后者描述模型推理策略。不得因为模型支持某个 reasoning effort 就自动提高 conversion level。

### 23.1 模板完善与跟踪

新增 profile、提高等级或开放新布尔能力时必须同时完成：

1. 更新配置层 profile 枚举和展开逻辑，并提升 `capabilities_schema_version`（改变既有 profile 语义时必须提升）。
2. 为两个 endpoint 分别补充配置解析、方向推导和非法手写字段拒绝测试。
3. 补充请求预检、非流式转换、SSE 状态机、tools/reasoning 降级及错误边界测试。
4. 补充 `/v1/models` 投影测试，确认只发布当前候选 endpoint 实际匹配的方向和布尔能力。
5. 更新 `config.example.yaml`、配置文档、集成文档与本映射表。
6. 使用真实 model+endpoint 完成功能矩阵验证；证据记录请求字段摘要、事件类型、终止状态、usage 和耗时，不记录密钥或完整正文。

跟踪时以 `model ID + upstream endpoint + profile + capabilities_schema_version` 为稳定标识，不使用 Provider 名称。

当前实现的有效门闩为：

```text
exact model endpoint template
    AND candidate currently exposes that upstream endpoint
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
5. conversion 候选按 exact model 与候选当前 upstream endpoint 查找固定 profile。
6. 展开 profile 并过滤任一必需能力不满足的候选。
7. 对剩余候选执行健康度、优先级和 fallback 排序。
8. 没有候选时返回 conversion_unsupported 或 endpoint_unsupported。
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
capabilities_schema_version: 2
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

Level 3 function tools 还施加独立预算：工具 schema 最大 256 KiB、schema 嵌套深度 32、单个 tool 参数/结果最大 1 MiB、工具定义最多 128 个；消息与内容块的独立预算及结构超限错误按 13.5.0 执行（协议内容块 512，消息/输入项与 system 各 256），不能把工具参数 JSON 对象计作协议内容块。已有工具专用预算拒绝继续使用 `conversion_unsupported`；所有限制均在访问上游前执行，不依赖通用请求体上限。请求结束时仍未解析的 call、未知或重复 result，以及与 Anthropic message role 不匹配的 tool block 同样必须在访问上游前拒绝。

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

`ignored_features` 在显式 reasoning adapter 下允许记录被省略的 `reasoning`、`thinking`、`reasoning_output`、`thinking_output` 字段名，但不得记录字段值；未配置 adapter 时，这些字段必须进入 `unsupported_features` 或错误响应。已识别的 Krill/Codex Responses output item 私有字段仅以 `internal_chat_message_metadata_passthrough`、`output_metadata` 两个有界名称记录并省略字段值，不得折叠进 Anthropic 文本或工具输入。

Prometheus 使用以下低基数指标，不把错误文本、请求正文或任意 feature 值作为 label：

```text
aetherrelay_conversion_requests_total
aetherrelay_conversion_duration_seconds_sum
aetherrelay_conversion_duration_seconds_count
aetherrelay_conversion_features_total
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

## 2026-09-20 运行验证收口合同

- `claude-owner/001203` 的工具结果包含布尔 `is_error: false`。转换必须接受省略、false、true；true 的错误状态以目标 `function_call_output.output` 中的 JSON 字符串 `{ "error": true, "output": "..." }` 表达，不向 Codex 对象传递 Anthropic 字段。字符串与纯文本块结果转换为文本，未知块拒绝而不是作为协议 JSON 偷渡。
- 转换失败必须给出有界特性与安全字段路径，不回显工具参数、结果或调用 ID；本地拒绝不产生虚假的上游请求记录。
- Responses `keepalive` 是非业务心跳，不推进完成状态、不生成下游业务块；未知事件仍拒绝。转换异常不得误记为客户端写入失败。
- 最终 Codex 出站正文在上游 owner 完成身份注入后观测，仅 `archive_interactions && archive_full_content` 时携带并归档。沿用 header 保真开关和正文附件摘要策略，不改变线上请求。逐次 HTTP 尝试分别归档，保留最终快照用于现有排障入口。
- 验收覆盖首轮、并行工具调用、工具成功/失败结果续轮、心跳、终止事件、正文开关及逐 attempt 归档；线上全链路结论必须等待重新部署验证。

## 30. 灰度、熔断与回滚

### 2026-09-22 无会话身份的缓存身份兜底（13.7.0）

未声明 `X-Claude-Code-Session-Id` 等会话信号的客户端，此前 `prompt_cache_key` 落到逐请求的 requestScope 随机值：键每轮变化，上游前缀缓存必然零命中，与提示词形态无关（本地子测试实测两轮得到两个不同 UUID）。现改由**对话锚点**派生：`system` 段（Anthropic `system` 数组文本或 Responses `instructions`，整段参与哈希、不做字节截断）+ 首条消息的 SHA-256，按客户端 Key 与模型命名空间化。

- 同一对话逐轮追加 → 键不变；换对话（开场不同）或开场被改写 → 换键；两段都取不到 → 保留既有回退且非空。
- Claude Code（带会话头）与声明了会话/线程信号的 Codex 客户端派生完全不变，既有会话的上游缓存身份不重建。
- 线上近 12h 内 codexoauth 有 6 轮 `cached=0`，可能属这类流量；部署后可用同样的「无会话头」请求复验，预期从 0 变为公共头级命中，命中量取决于该客户端前缀是否逐轮稳定。
- 回归覆盖：`TestPromptCacheHashFallsBackToStablePrefix` 与 `conversion_prefix_cache_test.go` 的无会话子测试。

### 2026-09-22 前缀缓存可复用性度量（13.6.0）

线上核对（2026-09-22 rounds 524-528，`claude-owner` + `codexoauth` + `gpt-5.6-luna`，mode `anthropic_to_codex_responses · L2 degraded`）显示同一 Claude Code 会话的 `cached_input_tokens` 恒为 **17920**（=140×128，上游按 128 token 块计量），而 `input_tokens` 从 29552 涨到 67119：`cache_hit_rate = cached_input_tokens / input_tokens`，因此使用率从 60.6% 被稀释到 26.7%。该使用者入站 body 在同 session 内反复涨落（137 KB→131 KB→147 KB→150 KB→242 KB→282 KB→293 KB→139 KB→303 KB），即主线程与子代理等多分支共用 session id、交替发起，不是逐轮追加；上游前缀缓存只能复用与已缓存内容一致的前缀，分支交替于是退化为各分支共有的公共头。

本地合成度量把「网关是否逐轮引入变化」与「客户端形态的可复用长度」分开：同一合成会话跑过真实转换路径（含 `prompt_cache_key` 注入）后测量——逐轮追加时上一轮完整提示词仍是下一轮提示词的前缀（5589 → 7885 字节），`prompt_cache_key` 与 `instructions`/工具定义逐轮不变；同 session 多分支交替时，与另一分支最近一次请求只剩公共头（3310 字节，本分支上一轮为 5597 字节）；客户端不声明 `X-Claude-Code-Session-Id` 时键逐请求变化（`374bb27b…` → `8c433e6c…`），命中必然为 0。

- 结论：命中退化为公共头属**客户端提示词形态**，不是网关转换缺陷；网关侧唯一会打到零命中的情况是客户端未声明会话身份。
- 度量按逻辑提示词（`instructions` + 工具定义 + 逐条 `input` 项）比较，而非请求体原始字节：会话数组按字典序排在 `instructions`/`tools` 之前，追加一项即造成其后字节位移（原始字节前缀 2377 对逻辑前缀 5589 字节），该序列化行为由 `CP-REQ-036` 声明。
- 归档未开全文（`full_content_enabled=false`），线上侧没有逐字节实证；分支交替结论来自 body 大小涨落、命中台阶与诊断字段（`prompt_cache_key_source=generated`、`turn_state_source=session`、`account_attempt=1`）。
- 回归覆盖：`proxyapi/service/proxy/conversion_prefix_cache_test.go` 的三个子测试。本条为测试固化，无部署动作；`13.5.0` 的内容块放宽仍需部署后复验。

### 2026-09-22 内容块预算放宽（13.5.0）

线上 rounds 253/276（2026-09-22 10:46:37Z / 10:57:09Z）记录同一个 Claude CLI 会话（`X-Claude-Code-Session-Id=14fef19c…`、`claude-cli/2.1.278`）在 `messages[98].content` 处累计 259 个协议内容块，被 13.3.0 的 256 预算在 17ms 内本地拒绝：body 约 737 KB、`input/output/total_tokens` 全为 0、`outcome=limit_exceeded`、`error_code=conversion_limit_exceeded`、`retryable=false`，无上游尝试与账号惩罚。同一会话约 10.5 分钟后原样重试（body 相差 227 字节）复现同一数值，说明这是长工具会话的真实结构，不是计数错误。

- 消息内协议 `content` 数组的块累计上限由 256 提升到 512；Anthropic `tool_result.content` 嵌套数组继续计入，`tool_use.input` 与 Responses function arguments/output 等业务 JSON 继续不计入。
- 顶层 messages/input 项数 256、Anthropic `system` 数组块 256、整树深度 32 与节点 65536、工具 schema 256 KiB / 单项参数结果 1 MiB / 工具定义 128 全部保持不变；三者不再共用同一常量，`system` 与 Chat 路径的消息条数不再随内容块预算变化。
- 超限行为不变：HTTP 400、`code=conversion_limit_exceeded`、`retryable=false`，不访问上游、不重试、不惩罚账号，也不为通过校验裁剪历史或工具结果。
- 整树节点预算仍是外层上限：极大业务数组或极深结构会先报 `limit_kind=tree_nodes`，排障时按 `limit_kind` 区分内容块与整树资源超限。

回归覆盖：99 条消息累计 259 块放行（`messages` 与 `input` 两个方向）、512/513 边界、跨消息累计越界、嵌套 `tool_result.content`、`system` 256 通过 / 257 拒绝、Chat→Responses 消息条数 256 通过 / 257 拒绝，以及 handler 层「达到上限调用上游、越过上限不触上游」。新边界须部署后用真实会话复验。

### 2026-09-21 消息树预算收口（13.3.0）

线上 d5f3309 于北京时间 09:01:38 启动；09:09:32–09:09:52 的 001454/001455/001456 均为 gpt-5.6-luna、52 条消息，清理已知注解后消息树分别含 262/263/264 个对象，最大数组长度 52、深度 6。001456 含 143 个直接内容块、4 个工具结果内文本块、65 个工具参数对象。旧校验将所有对象共用 256 个 content blocks 额度，产生 messages exceeds 256 content blocks，又被错误转换覆盖为 unsupported_feature。这些请求无 stop_sequences、无上游尝试；不是模型不存在、超时或上游拒绝。远端文件首事件超时已核对为 180 秒，但这些本地失败不构成超时或停止序列功能的线上验收。

本轮规则适用于 Anthropic→Responses/Codex 及共享校验的 Responses→Anthropic：

- 顶层 messages/input 项数最多 256，与内容块数独立；消息外壳不再占用内容块额度。
- 消息内协议 content 数组的块累计最多 256；Anthropic tool_result.content 数组也计入，防止嵌套协议块绕过限制。system 数组另有 256 块上限。纯文本字符串仍受正文/现有工具字节限制，不伪造为对象数。（13.5.0 已将协议内容块上限提升到 512，其余数值不变，见上一节。）
- 不进入 tool_use.input 或 Responses function arguments/output 的业务结构计数内容块。业务数据中同名 type/content/input 键不能被当成协议字段；不截断或丢弃工具参数来绕过限制。
- 整棵 messages/input 树使用独立的深度 32 和节点 65536 预算。节点包含对象、数组及标量，防止移除业务数组 256 项限制后失去资源保护。递归预算在首次越界停止，actual 是当时已观察的深度/节点数，不声称是完整请求总量。工具参数/结果 1 MiB、schema 256 KiB/深度 32、工具定义 128 的原有专用预算不放宽。
- 上述消息数、协议块数、整树深度/节点预算超限返回 HTTP 400、code=conversion_limit_exceeded、retryable=false。OpenAI envelope 包含 param/limit_kind/actual/limit；Anthropic envelope 保持 invalid_request_error，并在 message 中保留相同安全事实。usage/metadata 的 error_code 同步，outcome=limit_exceeded；conversion_error_path 保留路径，原始错误说明保留数值，不往 unsupported_features 添加伪能力，不创建上游档案或触发账号惩罚。路径仅由适配器生成，不输出工具参数键值。

回归使用合成而非客户正文，重建 52 条消息、262/263/264 个对象的三种结构，确认调用上游且工具参数无损；另测消息/内容块边界、超过 256 项的合法业务数组、嵌套 tool_result、深度与节点资源上限，以及错误响应、usage 与归档一致性。新规则仍须部署后用真实会话复验；无需修改客户端或线上配置模板。后续相关行为变化须同提交更新本节、维护合同及测试。

### 2026-09-20 剩余问题收口（13.2.0）

部署 f3bc470 后截至 001416 的快照为 29 个完成请求：16 成功、4 个 model_not_found、9 个 stop_sequences 本地转换拒绝；该快照未出现新增 503/504。成功轮次的转换观测、真实 usage/cache 明细、身份稳定及 end_turn 已验证。以下代码变更仍须重新部署验证，不能用离线测试代替线上证据。

#### 停止序列

- 仅 Anthropic→Responses/Codex 路径新增本地输出控制。原生 Anthropic 和现有 Chat 路径不改写。允许省略或空数组；非空数组最多 16 个非空 UTF-8 字符串，每项最多 256 字节。null、类型错误、空字符串及超限在上游调用前拒绝，错误路径定位到字段/数组项。
- `stop_sequences` 不进入上游 Responses/Codex JSON。非空声明进入 ignored_features，并标记 conversion_degraded，表示上游原生停止能力被本地输出控制替代，而不是静默忽略。配置能力声明不能使上游自动支持此字段。
- 只匹配可见 text 内容，不扫描 reasoning、工具 JSON 参数、工具结果或元数据。非流式按 content 顺序处理；流式在当前 text block 内跨 delta 匹配，保留可能构成停止串的后缀，块结束时原样释放未命中后缀。不跨独立 content block 匹配。
- 首个完整匹配生效：按结束位置最早者选择，同一结束位置按请求数组顺序决定。匹配串及之后输出不交给客户端；返回 stop_reason=stop_sequence、stop_sequence=实际命中的串。命中之后不下发新的工具块，未命中时保持原终止原因。该匹配规则不依赖网络分片。
- 命中不伪造上游 completed，也不提前取消生成；继续校验/读取上游终态以取得真实计费 usage，完整保留缓存明细。上游生成可能继续消耗时间和费用，输出 token 数可以大于客户端可见文本。本地 matcher 后缀有界，SSE 输入累计最多 64 MiB，原有取消、超时和错误处理继续生效；命中后上游失败/缺少终态仍为失败，不能改成成功。
- 验收覆盖分片、Unicode、重叠停止串、空数组/非法项、未命中后缀、工具参数不误截断、命中后的工具不泄漏、真实 usage、响应归档、上游终态失败以及两种 provider 路径的流式/非流式接线。

#### 本地错误观测

APIError.Code 必须进入 usage 和 metadata，model_not_found、conversion_unsupported 不退化为 error/conversion；outcome 仍为粗粒度分类。已有显式的细分失败码（例如 first_event_timeout）优先保留。路由选择之前的失败仍记录实际 Operation、ClientEndpoint、ClientProtocol，不伪造未发生的上游请求或响应。可准确识别的顶层字段及已定位的嵌套字段记录 conversion_error_path。

#### 客户端与部署配置

现场 Claude Code 配置已设置 ANTHROPIC_MODEL、Opus/Haiku 映射和子代理默认模型，但只设置 Sonnet 的 SUPPORTED_CAPABILITIES，缺少 Sonnet 模型映射。应在客户端 settings.json 的 env 中补齐下面这一项（合并，不能替换整个文件）：

```json
"ANTHROPIC_DEFAULT_SONNET_MODEL": "gpt-5.6-sol"
```

能力声明不等于模型映射，主模型也不是对所有模型选择的全局改写；显式模型选择/恢复会话仍需复测。具体哪个内部功能发起 Sonnet/stop 请求尚未确定，不能仅凭 autoCompactEnabled/precomputeCompactionEnabled 将其归因于压缩。模型目录仍采用精确查找，不把未知 Claude 模型静默映射到 GPT。

线上原显式 stream_first_event_timeout_seconds=90 会覆盖默认 180；模板已为 180，无须重复调整默认值。代码不自动覆盖用户配置，不因 effort=max 自动放宽超时，也不关闭压缩。

2026-09-20 经用户要求已刷新配置：本地 `/home/rangh/.claude/settings.json` 的 env 补齐 `ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5.6-sol`；`root@x600.muidea.com:1212` 的 `/home/workspace/deploy/config/config.yaml` 将首事件超时从 90 调整为 180 秒，空闲超时保持 300 秒，其余字段不变。远端原配置备份为同目录 `config.yaml.bak-20260920-first-event-180`，写入后校验哈希一致。未重启或重新部署服务；文件更新不代表运行实例已采用新值，仍需用户重新部署并复验，Claude Code 也需重新启动后确认实际模型选择。

后续所有相关代码、配置或验证结论变更须同步刷新本节与 Codex 维护合同；上述客户端/部署配置及新停止序列行为均列为上线复验项。

### 2026-09-20 后续日志收口（13.1.0）

001381、001383 上游分别在 1231/1111 ms 返回 HTTP 200，但 90 秒内没有首个业务事件，代理返回 504；相同正文随后在 001382/001384 成功。001385 首事件等待 86991 ms，最终成功 end_turn。上游报告 safety buffering，但日志不足以把它确定为根因。

首事件默认/模板预算调整为 180 秒，沿用现有配置且保留显式 90/0 的含义；不新增按请求头扩展预算，不绕过心跳、首事件、空闲和输出后禁止重放边界。上游 200、下游 504、first_event_timeout 必须同时准确保留；首事件失败不能标成 idle_timeout。metadata.error_code 与用量、诊断同源。Headers 阶段先观测，终态按变化更新；相同 attempt 不重复写正文/打印相同响应，新的重试仍独立归档，迟到旧回调不得覆盖最后尝试。

此轮不改变 UA、身份、缓存或账号惩罚/重试规则。线上 1547001 的 end_turn 证据已成立，但 13.0/13.1 修复和新预算仍须部署后验证。

### 2026-09-20 转换观测补全（13.0.0）

现场 `claude-owner/001377–001380` 已观察到成功工具续轮，当时尚不能据此宣称整段任务已到 `end_turn`；后续 001385、001386 已确认真实 end_turn + message_stop（部署版本 1547001）。事件 `6800a4e050fafd3bb5ac30d30e66f3ec` 的管理页缺项来自观测未贯通，缓存零值也不足以证明真实未命中。本轮规则如下：

- Codex 的 Messages/Chat 有界转换等级为 2；记录本地请求/响应转换处理耗时，不包含上游等待和客户端写入。毫秒取整后 0 合法，不显示为空。重复设置传输计划不能清除已经发生的降级；请求与响应的省略项合并。
- 上游 owner 提供每次 HTTP 尝试的值类型观测，ProxyAPI 负责归档与用量结算。最终尝试的 status、实际 Content-Type、Content-Length、Transfer-Encoding 和响应头耗时贯通 EventHub、usage_events、metadata.json 与管理页。不能用下游 SSE Content-Type 补造上游缺失头；长度未知用存在性区分，明确 0 必须保留。最终尝试无响应时清除前一尝试的头统计，逐尝试档案仍保留。
- 总耗时覆盖整个请求，响应头耗时只指最终尝试，首事件耗时仍指首业务事件，不把 keepalive 算入。转换/上游统计不依赖交互归档开关。metadata.event_id 与实际用量 Event ID 对齐，request_id 仍独立。
- Codex Responses 非流式及 SSE 终态保留缓存读取/创建明细和字段存在性。管理缓存统计只聚合成功请求，并使用这些成功请求的上游原始总输入作为使用率分母；失败、超时、转换拒绝和未完成事件继续保留原始记录，但不参与缓存读取、创建、完整性或使用率聚合。Anthropic 上游报文的 input_tokens 不含缓存读取/创建，解析时先与 cache_read_input_tokens/cache_creation_input_tokens 相加再进入内部用量，因此“上游原始总输入”指该归一化后的输入，使用率恒在 0–100%；反向按 Anthropic 协议输出（原生归档响应、Responses→Anthropic 转换）时再扣除明确报告的缓存读写并另列两个缓存字段，两个方向对称，避免双计数。成功请求中的缺失字段按读取、创建分别剔除，使用率分母仅取读取已知样本；无有效样本展示未提供，历史数据不猜测回填。CSV 导出在原列尾部追加两项 `*_known`，避免导出后重新混淆未知与零。
- 默认 Claude cache identity 按身份语义基准 13.0.0 执行；不恢复源协议 metadata/cache_control，不改变 UA/Originator、Installation、Session/Thread 或 Turn-State 投影，不承诺实际命中。

本地验收覆盖工具结果续传至 end_turn、缓存缺失/显式零/非零、重试最终响应归属、无归档统计、数据库存在性及管理页显示。上线仍需重新验证真实 end_turn、跨轮 cache identity 和上游实际缓存命中，不能以离线测试替代运行证据。

转换能力默认关闭，只有 exact model 的当前 upstream endpoint 配置固定 profile 时才开启。Provider endpoint 热更新会原子重建有效目录，因此 `/v1/models` 与请求路由使用同一代模板匹配结果。灰度期间监控：

- conversion error rate；
- upstream 400/422 rate；
- p95/p99 header、first-event 和 total latency；
- stream truncation；
- tool call parse failure；
- estimated usage rate；
- client cancellation rate。

超过阈值时移除对应 model+endpoint profile 或将 Provider 切回原生 endpoint；其他提供相同 model+endpoint 的 Provider 会使用同一模板，因此模板回滚是模型端点级操作。若无 native 候选则明确返回 `conversion_unsupported`。配置回滚必须恢复旧 runtime snapshot、旧 `/v1/models` 能力输出和旧路由候选，不能只回滚 YAML 文件。
