# 外部应用集成指南

本文说明业务应用如何接入 `ai-proxy`、发现模型能力、选择客户端协议，并正确处理流式响应、工具调用和错误。Provider 配置见[配置参考](configuration.md)，服务端点矩阵见[功能说明](features.md)，跨协议转换的内部设计见[Responses 与 Anthropic 双向转换](design/responses-anthropic-conversion.md)。

## 1. 接入地址与鉴权

默认监听端口以实例的 `config.yaml` 为准。假设网关地址为 `http://127.0.0.1:8080`：

```text
OpenAI SDK Base URL:    http://127.0.0.1:8080/v1
Anthropic SDK Base URL: http://127.0.0.1:8080
```

OpenAI 风格端点使用 Bearer 鉴权：

```http
Authorization: Bearer <client-api-key>
```

Anthropic Messages 端点使用 Anthropic 风格鉴权：

```http
X-API-Key: <client-api-key>
Anthropic-Version: 2023-06-01
```

`client-api-key` 由 ai-proxy 管理端创建。它不是 Provider 的上游 API Key，也不是 ChatGPT Web 或 Codex OAuth 凭据；网关不会把客户端 Key 作为上游凭据转发。

`codexoauth` 是账号池注入的只读内建 Provider，只提供固定 Codex OAuth 上游的原生 `/v1/responses`。业务端不能通过 Provider 配置切换其 protocol、base URL 或 endpoints；需要切换上游接入端点时，应使用独立的管理型直连 Provider。Codex OAuth 边界会把 Responses 标准字符串 `input` 等价展开为消息数组，并强制上游 `stream=true`、`store=false`；这些属于固定传输适配，不改变业务端发布的客户端端点。模型目录中的 `maxOutputTokens` 表示模型输出能力上限，不代表该固定 transport 接受客户端 `max_output_tokens` 参数；当前传入该字段会在上游调用前明确返回 400，不会静默省略。

Codex OAuth 的非流式聚合会从 SSE `output_item.done` 重建标准 `output`；若上游只提供文本 delta，则生成等价的 assistant/output_text item。流式工具调用已观察到固定上游在完整 `function_call_arguments.done` 后仅发送 `event: response.output_item.done` 并 clean EOF，未必提供 `response.completed` data。代理只在已收到 item done event 时将这种 EOF 结算为成功，不伪造缺失的 completion 对象；依赖严格 `response.completed` data 的应用应使用非流式工具调用。

### 1.1 SDK 配置

Python OpenAI SDK：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8080/v1",
    api_key="<client-api-key>",
)
```

Python Anthropic SDK：

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://127.0.0.1:8080",
    api_key="<client-api-key>",
)
```

SDK 只负责生成对应协议的请求。应用仍应读取 `/v1/models`，不能只凭 SDK 类型或模型名称推断能力。

## 2. 获取模型目录

应用启动后先获取模型目录：

```bash
curl -sS http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer <client-api-key>'
```

返回目录已经按当前客户端 API Key 的 Provider 访问范围裁剪。同一个实例中，不同 Key 调用 `/v1/models` 可能得到不同模型、端点和转换能力；应用不得缓存或复用其他 Key 的目录。请求一个全局存在但当前 Key 未授权任何候选的模型时，网关统一返回 `model_not_found`，不会泄露模型是否由其他 Provider 提供。

### 图片资产作用域

`POST /v1/images/generations` 与 `POST /v1/images/edits` 成功产生的图片，按认证凭据解析出的稳定 `api_key_id` 存储。应用只需继续使用自己的 API Key，不需要也不能在请求体中指定作用域。`b64_json` 响应会在返回前归档图片字节；上游仅返回 URL 且没有图片字节时，代理不会主动下载该 URL。Admin 图片任务和图片库要求显式选择已存在的客户端 `api_key_id`，任务、原图、缩略图、标签和读取/删除操作均严格限制在该 Key 内。删除客户端 Key 会同步清理其图片任务、图片资产和 `interactions/{api_key_id}/` 交互归档。

响应结构：

```json
{
  "object": "list",
  "data": [
    {
      "id": "model-id",
      "object": "model",
      "contextWindowTokens": 1000000,
      "maxOutputTokens": 29000,
      "supported_endpoints": [
        "/v1/responses",
        "/v1/messages"
      ],
      "capabilities": {
        "reasoning": {
          "supported": true,
          "default_effort": "low",
          "efforts": ["none", "low", "high", "max"]
        },
        "native": {
          "responses": {
            "tools": true,
            "images": false
          }
        },
        "conversions": {
          "responses_to_anthropic": {
            "level": 3,
            "text": true,
            "images": false,
            "documents": false,
            "reasoning": true,
            "reasoning_mode": "degrade",
            "tools": true,
            "structured_output": false,
            "streaming": true,
            "continuation": false
          }
        }
      }
    }
  ]
}
```

所有扩展字段均为可选字段。客户端解析 DTO 时必须允许字段缺失和未来新增字段。

### 2.1 字段语义

| 字段 | 语义 | 客户端要求 |
| --- | --- | --- |
| `id` | 请求体中使用的精确模型 ID | 大小写敏感，不做名称归一化 |
| `contextWindowTokens` | 已知的上下文窗口 | 缺失表示未知，不等于 `0` |
| `maxOutputTokens` | 已知的最大输出 token 数 | 缺失表示未知；有值时请求不得超过它 |
| `supported_endpoints` | 当前目录中至少一个已配置或已发现候选具备该客户端路径合同 | 必须按完整路径匹配；不代表实时健康 |
| `capabilities.reasoning` | 模型级 reasoning 能力和可选 effort | 只发送列入 `efforts` 的值 |
| `capabilities.native.responses` | 模型级原生 Responses 声明 | 不代表跨协议转换能力 |
| `capabilities.conversions` | 已验证并发布的方向化转换合同 | 按方向和具体功能逐项判断 |

`supported_endpoints` 是目录候选的并集。它综合 Provider 启停状态、模型匹配、原生端点、转换合同及账号池发现结果动态生成，不是静态配置的原样输出；运行时健康度和熔断在实际请求时另行过滤，因此目录列出的模型仍可能暂时返回 `provider_unavailable`。

`/v1/models` 不返回 Provider 名称，业务应用不能锁定某个 Provider，也不能假定同一模型、同一客户端端点每次都使用 native 路径。网关只会在当前 Key 获准的候选中路由和 fallback，绝不会因未授权 Provider 更健康或优先级更高而越权使用；应用必须以当前 Key 的公开能力合同构造请求。

### 2.2 Conversion Level

`level` 是固定 endpoint profile 展开后的只读兼容等级，不是业务请求字段，也不是 reasoning 强度：

| Level | 已开放的跨协议能力 |
| --- | --- |
| `0` 或未声明 | 不开放该方向的转换 |
| `1` | 非流式纯文本 |
| `2` | Level 1 + 纯文本 SSE |
| `3` | Level 2 + 非流式 function tools |

业务应用仍必须检查对应布尔能力。例如 function tools 要求 `level >= 3` 且 `tools: true`；纯文本流式要求 `level >= 2` 且 `streaming: true`。

管理配置使用固定 profile，其对外能力映射如下：

| Profile | 对外 Level | `text` | `streaming` | `tools` | `reasoning` / `reasoning_mode` |
| --- | ---: | --- | --- | --- | --- |
| `level1` | 1 | true | false | false | false / 省略 |
| `level2` | 2 | true | true | false | false / 省略 |
| `level2_reasoning` | 2 | true | true | false | true / `degrade` |
| `level3` | 3 | true | true | true | false / 省略 |
| `level3_reasoning` | 3 | true | true | true | true / `degrade` |

现有 profile 不开放 images、documents、structured output、continuation 或流式 tools。业务应用只消费 `/v1/models` 展开后的能力，不发送 profile 名，也不能在请求中选择或提升 Level。

方向名称从客户端协议指向上游协议：

| 客户端调用 | 可能使用的转换能力 |
| --- | --- |
| `POST /v1/responses` 路由到 Anthropic Messages 上游 | `responses_to_anthropic` |
| `POST /v1/messages` 路由到 OpenAI Responses 上游 | `anthropic_to_responses` |

转换能力只有在 exact model 的 metadata 为候选当前 upstream endpoint 配置固定 profile 时才会出现在目录中。能力不绑定 Provider；提供同一 exact model 和 endpoint 的所有候选共享模板。业务应用只依赖 `/v1/models` 的最终结果，不推断管理面配置。

## 3. 端点选择

推荐按以下顺序选择：

1. 用精确 `id` 找到模型记录；找不到则停止请求。
2. 列出应用实际需要的功能：文本、SSE、function tools、reasoning 或其他高级语义。
3. 优先选择应用原生实现的协议，并确认对应路径存在于 `supported_endpoints`。
4. 请求依赖跨协议能力时，检查对应 conversion 方向、`level` 和功能布尔值。
5. 无明确 capability 的高级字段按“不支持”处理，不通过模型名称或端点名称猜测。
6. query 参数只在 native 请求中保留；跨协议转换不会把客户端 query 带到另一协议的上游。

示例判断逻辑：

```text
需要 Responses 非流式文本
  └─ supported_endpoints 包含 /v1/responses

需要 Responses 纯文本 SSE，且请求可能被转换到 Messages
  └─ responses_to_anthropic.level >= 2
     且 responses_to_anthropic.streaming == true

需要 Messages function tools，且请求可能被转换到 Responses
  └─ anthropic_to_responses.level >= 3
     且 anthropic_to_responses.tools == true
     且 stream == false
```

Anthropic→Responses 转换中，省略 `thinking` 表示未启用 thinking。若目标模型的 `reasoning_efforts` 明确包含 `none`，代理会显式发送 `reasoning.effort=none`，避免目标模型的默认 reasoning 模式改变工具选择语义；显式 `thinking.type=adaptive` 仍按 capability 中固定的目标 effort 降级映射。反向 Responses→Anthropic 中，显式 `reasoning.effort=none` 映射为 `thinking.type=disabled`，其他 effort 映射为 adaptive + 配置的目标 effort。DeepSeek Anthropic 接入在 thinking 开启时拒绝命名 `tool_choice`，因此命名工具请求应显式使用 `reasoning.effort=none`。

对于只实现 OpenAI Chat Completions 的旧应用，仅选择 `supported_endpoints` 包含 `/v1/chat/completions` 的模型。Chat→Anthropic 是基础文本兼容路径；Anthropic 返回的 thinking 块会被识别并省略，不会作为 assistant 文本暴露。不要把 Provider 配置中的 `responses`、`messages` 等名称拼成 URL。

`grok-4.5` 在 Krill Provider 上已分别通过 Anthropic Messages 与 OpenAI Responses transport 的文本、SSE、非流式 function tools、工具结果闭环及 reasoning/thinking 降级验证。当前模型元数据发布 `none/low/high/max`，转换固定降级目标为 `low`，图片与流式工具保持关闭。Krill 原生 Responses 接受 `store=false`、`reasoning.effort=high` 和 `text.verbosity=high`；但 hosted `web_search` 即使要求 `tool_choice=required` 也未产生真实搜索工具事件，因此 `web_search=live` 应由应用端工具能力负责，不能依据该配置推断上游已支持实时搜索。

`gpt-5.6-luna`、`gpt-5.6-sol` 与 `gpt-5.6-terra` 的模型目录均发布 `272,000` context window、`128,000` max output tokens，以及一致的 reasoning 枚举 `none/low/medium/high/xhigh/max`，默认值均为 `medium`。其中 Luna 已完成双向 Level 3 验证，固定转换目标为 `medium`；该验证结论不自动扩展到尚未配置转换模板的 Sol 与 Terra。Responses output item 的私有 metadata 会被代理有界省略并记录 degraded feature，不向 Anthropic 内容泄漏内部元数据。

`gpt-5.5` 由内建 `codexoauth` 发布原生 `/v1/responses`，模型目录返回 `272,000` context window 和 `128,000` max output tokens。实测 `none/low/medium/high/xhigh` reasoning、非流式文本、文本 SSE、function call、tool result 闭环和流式工具事件均可用；默认 effort 为 `medium`。不要发送 `max` effort。图片能力保持关闭；客户端 `max_output_tokens` 受固定 Codex OAuth transport 限制，会在调用上游前返回明确的 400。

## 4. OpenAI Responses 集成

### 4.1 Level 1：非流式文本

```bash
curl -sS http://127.0.0.1:8080/v1/responses \
  -H 'Authorization: Bearer <client-api-key>' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "model-id",
    "input": "请简要总结这段内容",
    "max_output_tokens": 512,
    "stream": false
  }'
```

Python SDK：

```python
response = client.responses.create(
    model="model-id",
    input="请简要总结这段内容",
    max_output_tokens=512,
)
print(response.output_text)
```

跨协议 Level 1 保证文本、基本 usage 和有限终止原因映射，不保证保留 Provider 私有字段或所有 output item 类型。

### 4.2 Level 2：纯文本 SSE

```bash
curl -N http://127.0.0.1:8080/v1/responses \
  -H 'Authorization: Bearer <client-api-key>' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "model-id",
    "input": "逐步说明部署流程",
    "max_output_tokens": 1024,
    "stream": true
  }'
```

客户端应按 SSE 的 `event:` 和 `data:` 行解析事件，消费 `response.output_text.delta`，并以 `response.completed` 或 `response.incomplete` 作为终止状态。不要通过连接关闭推断成功。

Level 2 仅承诺纯文本流。携带 tools 的流式请求会在访问上游前返回 `conversion_unsupported`。

### 4.3 Level 3：非流式 function tools

首次请求：

```json
{
  "model": "model-id",
  "input": "查询上海今天的天气",
  "stream": false,
  "tools": [
    {
      "type": "function",
      "name": "get_weather",
      "description": "查询指定城市的天气",
      "parameters": {
        "type": "object",
        "properties": {
          "city": {"type": "string"}
        },
        "required": ["city"]
      }
    }
  ],
  "tool_choice": "auto"
}
```

模型可能返回：

```json
{
  "output": [
    {
      "type": "function_call",
      "call_id": "call_1",
      "name": "get_weather",
      "arguments": "{\"city\":\"上海\"}"
    }
  ]
}
```

执行工具后重新提交完整且闭合的历史。转换路径不支持 `previous_response_id` continuation：

```json
{
  "model": "model-id",
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": "查询上海今天的天气"
    },
    {
      "type": "function_call",
      "call_id": "call_1",
      "name": "get_weather",
      "arguments": "{\"city\":\"上海\"}"
    },
    {
      "type": "function_call_output",
      "call_id": "call_1",
      "output": "晴，28 摄氏度"
    }
  ],
  "tools": [
    {
      "type": "function",
      "name": "get_weather",
      "parameters": {"type": "object"}
    }
  ],
  "stream": false
}
```

`call_id` 必须稳定，call 与 result 必须成对闭合。悬空调用、未知或重复 result、非 JSON function 参数会在访问上游前被拒绝。

## 5. Anthropic Messages 集成

### 5.1 Level 1：非流式文本

```bash
curl -sS http://127.0.0.1:8080/v1/messages \
  -H 'X-API-Key: <client-api-key>' \
  -H 'Anthropic-Version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "model-id",
    "max_tokens": 512,
    "messages": [
      {"role": "user", "content": "请简要总结这段内容"}
    ]
  }'
```

Python SDK：

```python
message = client.messages.create(
    model="model-id",
    max_tokens=512,
    messages=[{"role": "user", "content": "请简要总结这段内容"}],
)
print(message.content[0].text)
```

### 5.2 Level 2：纯文本 SSE

请求增加 `"stream": true`。客户端消费 Anthropic `content_block_delta` 中的 `text_delta`，并等待 `message_stop`。转换流也会保持 Anthropic 事件 envelope，因此应使用 Anthropic SDK 或标准 SSE 解析器，而不是按 Responses 事件解析。

### 5.3 Level 3：非流式 function tools

```json
{
  "model": "model-id",
  "max_tokens": 1024,
  "stream": false,
  "messages": [
    {"role": "user", "content": "查询上海今天的天气"}
  ],
  "tools": [
    {
      "name": "get_weather",
      "description": "查询指定城市的天气",
      "input_schema": {
        "type": "object",
        "properties": {
          "city": {"type": "string"}
        },
        "required": ["city"]
      }
    }
  ]
}
```

收到 `tool_use` 后，下一次请求提交完整历史：

```json
{
  "model": "model-id",
  "max_tokens": 1024,
  "stream": false,
  "messages": [
    {"role": "user", "content": "查询上海今天的天气"},
    {
      "role": "assistant",
      "content": [
        {
          "type": "tool_use",
          "id": "call_1",
          "name": "get_weather",
          "input": {"city": "上海"}
        }
      ]
    },
    {
      "role": "user",
      "content": [
        {
          "type": "tool_result",
          "tool_use_id": "call_1",
          "content": "晴，28 摄氏度"
        }
      ]
    }
  ],
  "tools": [
    {
      "name": "get_weather",
      "input_schema": {"type": "object"}
    }
  ]
}
```

`tool_use` 只能位于 assistant message，`tool_result` 只能位于 user message。流式 function tools 以及 hosted/search/computer/MCP 等 Provider 专用工具不在转换合同内。

## 6. Reasoning 与 Thinking

### 6.1 原生模型能力

应用仅在 `capabilities.reasoning.supported` 为 `true` 时发送 reasoning，并从 `efforts` 中选择值：

```json
{
  "model": "model-id",
  "input": "分析这个并发问题",
  "reasoning": {"effort": "low"}
}
```

未显式指定时，可使用 `default_effort` 作为 UI 默认值或直接省略，让服务端采用默认策略。`none`、`low`、`medium`、`high`、`xhigh`、`max` 等值不是所有模型都支持；必须以当前模型返回的 `efforts` 为准。

### 6.2 跨协议降级

OpenAI `reasoning` 与 Anthropic `thinking` 不是同构字段。只有 conversion capability 同时声明 `reasoning: true` 和 `reasoning_mode: "degrade"` 时，网关才执行方向专用的有损适配：

- 客户端控制字段映射到 Provider 配置的固定目标 effort，而不是逐值换算；
- reasoning/thinking 输出块和 delta 被省略，不转换成普通文本；
- 调用观测标记 `conversion_degraded=true`；
- Anthropic manual thinking，即 `type: "enabled"` 配合 `budget_tokens`，不参与转换。

因此需要获取或展示完整推理内容的应用必须使用已明确验证的原生协议，不能使用跨协议降级路径。

## 7. Native 与 Conversion 的边界

以下能力目前不属于 Responses 与 Anthropic 的转换合同：

- 图片和 document content block；
- JSON Schema、Structured Outputs、`text.format` 和 `response_format`；
- `previous_response_id`、conversation continuation；
- 流式 function tools；
- parallel tool calls；
- hosted search、computer、MCP 等专用工具；
- manual thinking 及推理内容透传；
- `metadata`、`store`、`include`、`service_tier` 等无法等价表达的语义字段。

网关不会静默删除这些字段。转换路径收到无法表示的语义时，会在访问上游前返回 `conversion_unsupported`。

`capabilities.native.responses.tools/images` 仅表示模型级 native Responses 声明；`capabilities.conversions` 仅表示跨协议合同。`supported_endpoints` 包含 `/v1/responses` 也不等于 JSON Schema 一定可用。

由于模型目录不暴露 Provider 级候选详情，应用若依赖 JSON Schema 等高级 native 语义，应只使用实例另行明确发布的原生能力合同。在目录没有对应 capability 字段时应保守判定为不支持。

## 8. 错误合同与重试

OpenAI 风格端点返回：

```json
{
  "error": {
    "code": "conversion_unsupported",
    "message": "feature cannot be represented by this conversion path",
    "type": "invalid_request_error",
    "model": "model-id",
    "client_endpoint": "/v1/responses",
    "client_protocol": "openai",
    "feature": "parallel_tool_calls",
    "unsupported_features": ["parallel_tool_calls"]
  }
}
```

Anthropic Messages 端点返回 Anthropic-compatible envelope：

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "conversion_unsupported: feature cannot be represented by this conversion path"
  }
}
```

稳定错误码及建议：

| 错误码或状态 | 含义 | 应用行为 |
| --- | --- | --- |
| `authentication_failed` / 401 | 客户端鉴权失败 | 检查或刷新 client API Key，不要替换 Provider Key |
| `model_required` / `invalid_request` / `request_too_large` | 请求不合法 | 修正请求，不自动重试 |
| `model_not_found` / `endpoint_unsupported` | 当前目录或端点不匹配 | 立即刷新 `/v1/models` 后重新选择 |
| `conversion_unsupported` | 请求语义超出转换合同 | 移除字段、改用明确的 native 合同或更换模型；不要原样重试 |
| `route_contract_invalid` | 服务端路由合同错误 | 停止重试并告警运维 |
| `provider_unavailable` | 当前无健康候选 | 有界指数退避，并刷新模型目录 |
| `upstream_unavailable` | 上游连接、超时或协议失败 | 有界指数退避；保证业务操作幂等 |
| 429 | 上游或网关限流 | 尊重 `Retry-After`，否则有界指数退避 |
| 5xx | 临时服务故障 | 仅对幂等或可去重请求做有界重试 |

其他稳定错误码包括 `proxy_internal_error` 和 `usage_store_unavailable`。应用应优先读取机器可解析的 `error.code`；Anthropic envelope 不单独暴露 `code` 时，可按 HTTP 状态和 `error.type` 处理，message 仅用于诊断。

## 9. 流式客户端要求

- 使用标准 SSE 解析器，不按 TCP 分片或逐行 JSON 假设事件边界；
- 忽略 SSE comment 和空事件，按客户端端点对应的事件类型解析；
- 只有收到协议终止事件才判定成功；EOF 前无终止事件视为截断；
- 客户端取消时主动关闭连接；网关会同步取消上游请求；
- 首事件或事件间空闲可能触发超时，不能把已收到 `200` 响应头等同于请求完成；
- 网关写出首个 SSE 事件后不会切换 Provider；
- 普通 HTTP 候选可在连接失败、首事件前失败、408、429 或 5xx 时回退；ChatGPT Web/Codex 等可能创建上游会话的内置执行器只在能够证明未产生副作用的专用路径上回退；
- 流中失败后，只有业务请求可幂等或有去重键时才能从头重试；
- 不要对同一次流式工具执行进行无条件重放。

## 10. 目录缓存与刷新

模型目录会随 Provider 启停、模型匹配、端点变更和账号池发现结果变化。Provider 切换上游协议或 endpoint 时，网关按 `exact model + upstream endpoint` 重新匹配固定模板；未匹配方向不会进入业务目录。推荐策略：

- 进程启动时必须获取一次，获取失败时不要盲发模型请求；
- 使用短时缓存，建议由应用按自身流量设置 30 至 300 秒 TTL；
- `model_not_found`、`endpoint_unsupported` 或 `provider_unavailable` 时触发一次即时刷新；
- 保留上一个成功目录作为短暂降级缓存，但不得永久使用；
- 刷新使用 singleflight 或同类机制，避免并发错误造成刷新风暴；
- 模型 capacity 字段缺失时不自行填入其他模型的窗口值。

Admin 系统信息中的“开放 API 端点”是实例级路由清单，不代表每个模型都支持该路径。普通业务应用只应依赖数据面的 `/v1/models`；Admin API 需要管理面认证，不应暴露给业务客户端。

自定义 Provider 支持整体配置迁移：`POST /admin/api/providers/export` 导出安全配置（默认不包含 API Key），请求体设置 `{"include_api_keys":true}` 才导出完整凭据；`POST /admin/api/providers/import` 导入 `ai-proxy.provider-bundle` v1 文件，默认按 `merge` 合并同名 Provider，也支持 `skip` 和 `replace`。所有新建或导入的 Provider 都必须包含 API Key；安全导出仅能更新目标实例已有 Provider 并保留其密钥，不能用于新增无密钥 Provider。完整导出需要 Admin 权限、二次确认并使用 `Cache-Control: no-store`。Provider bundle 只包含 Provider 自身的协议、Base URL、模型、端点、路由优先级和启用状态，不包含健康度、模型元数据、转换能力、用量或账号池信息。

账号池迁移仅提供整体账号池接口：`POST /admin/api/account-pool-bundle/export` 与 `POST /admin/api/account-pool-bundle/import`。整体包使用 `ai-proxy.account-pool-bundle`、`schema_version: 2`，一个 `accounts[]` 元素包含可选的 `chatgpt_web` 与 `codex_cli` 槽位。ChatGPT Web 和 Codex 仍可通过管理页分别导入各自凭据；统一账号列表工具栏提供始终可见的“账号池迁移 ▾”分组入口，集中放置整体导入与导出，但不提供槽位单独导出；整体导出响应包含敏感凭据并设置 `Cache-Control: no-store`，要求两个 Store 都可用。整体导入会先完成整包预检，校验失败或发现重复 `account_ref`、重复槽位凭据、同一邮箱被多个 `account_ref` 使用时返回 `409`，并在 `conflicts` 中给出账号引用、槽位和安全原因，不会写入任一 Store；只包含一种槽位的 bundle 只要求对应 Store 可用。预检通过后先按槽位 `account_id` 匹配目标账号；未提供 `account_id` 时按唯一邮箱回退匹配，不会因为目标摘要来自另一个上游账号而误报冲突。同邮箱但明确不同上游 `account_id` 默认返回 `409`，只有在 bundle 顶层显式设置 `"replace": true` 时才允许替换目标槽位；一个 bundle 中的多个账号不能指向同一个已有槽位。跨 Store 无法事务回滚，若一侧成功、另一侧失败，响应的 `partial_success` 为 `true`。同一账号的跨槽位归组优先使用规范化邮箱，不能使用不同上游 `account_id` 直接强行合并。

两类迁移导出均遵循统一下载名 `ai-proxy-{artifact}-bundle-v{schema}-{profile}-{YYYYMMDDTHHMMSSZ}.json`：Provider 分别生成 `ai-proxy-provider-bundle-v1-safe-...` 或 `ai-proxy-provider-bundle-v1-complete-...`，账号池整体迁移生成 `ai-proxy-account-pool-bundle-v2-complete-...`。时间戳来自服务端返回的 `exported_at`，管理页下载名与 HTTP `Content-Disposition` 一致。文件名只用于识别和整理，导入始终按 JSON 内的 `format` 与 `schema_version` 校验，详见[管理面迁移 Bundle 文件命名](design/bundle-file-naming.md)。

## 11. 推荐启动流程

```text
加载 ai-proxy 地址和 client API Key
  -> GET /v1/models
  -> 按 exact model ID 建立本地目录
  -> 校验 endpoint + 所需 capability
  -> 构造对应协议请求
  -> 处理协议 envelope、SSE 终止和 usage
  -> 按错误分类决定修正、刷新目录或有界重试
```

集成上线前至少确认：

- Base URL 没有重复或遗漏 `/v1`；
- 使用 client API Key，而非 Provider Key；
- 模型 ID 保持原始大小写；
- 请求端点存在于该模型的 `supported_endpoints`；
- conversion 方向、level 和功能布尔值满足实际请求；
- `max_output_tokens` 或 `max_tokens` 未超过已声明上限；
- 未把 JSON Schema、图片、continuation 或流式 tools 发送到转换路径；
- tool call/result 的 ID、角色和历史闭合；
- SSE 客户端验证终止事件并能识别截断；
- 重试有上限、退避和幂等保护；
- 模型目录会定期及按错误触发刷新。
