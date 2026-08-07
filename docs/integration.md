# 外部应用集成指南

本文说明外部应用如何通过 `ai-proxy` 发现模型能力并选择正确的请求端点。完整的 Provider 配置见[配置参考](configuration.md)，端点矩阵见[功能说明](features.md)。

## 1. 配置客户端入口

外部应用只需要把 API Base URL 指向网关，并使用管理页创建的客户端 API Key：

```text
OpenAI 风格客户端：    http://127.0.0.1:8080/v1
Anthropic 风格客户端： http://127.0.0.1:8080
```

认证方式：

- OpenAI 风格请求使用 `Authorization: Bearer <client-api-key>`；
- Anthropic 风格请求使用 `X-API-Key: <client-api-key>`。

客户端 Key 不等于 Provider 的上游 API Key，也不等于 ChatGPT Web / Codex OAuth 凭据。网关不会把客户端 Key 转发给上游。

## 2. 从 `/v1/models` 发现模型能力

先请求模型目录：

```bash
curl -sS http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer <client-api-key>'
```

响应中的每个模型可能包含以下字段：

```json
{
  "id": "claude-3-7",
  "object": "model",
  "contextWindowTokens": 200000,
  "maxOutputTokens": 8192,
  "supported_endpoints": [
    "/v1/messages",
    "/v1/chat/completions"
  ]
}
```

字段说明：

| 字段 | 语义 |
| --- | --- |
| `id` | 请求体中使用的 exact 模型 ID，大小写敏感 |
| `contextWindowTokens` | 已知上下文窗口；未知时省略 |
| `maxOutputTokens` | 已知最大输出；未知时省略 |
| `supported_endpoints` | 当前至少一个可用候选 Provider 能服务的客户端路径 |

`supported_endpoints` 是运行时派生结果，不是配置字段。它综合考虑：

1. Provider 是否启用；
2. Provider 是否匹配该模型；
3. Provider 声明的上游原生 `endpoints`；
4. 统一协议转换矩阵；
5. ChatGPT Web / Codex OAuth 账号池当前是否有可路由模型。

因此外部应用不应把 `chat_completions`、`messages` 等 Provider 配置值直接当作请求 URL；应使用返回的完整客户端路径。

## 3. 选择请求端点

应用应先在 `supported_endpoints` 中查找自己支持的路径，再发送请求。例如：

```text
模型支持 /v1/responses → POST /v1/responses
模型只支持 /v1/messages → POST /v1/messages
模型支持 /v1/chat/completions → POST /v1/chat/completions
```

典型请求：

```bash
curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <client-api-key>' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-3-7",
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

如果应用只实现 OpenAI Chat Completions，应筛选包含 `/v1/chat/completions` 的模型；不要仅凭模型名称猜测能力。

## 4. 端点能力的派生规则

当前主要规则如下：

| `supported_endpoints` | 说明 |
| --- | --- |
| `/v1/chat/completions` | OpenAI 原生 Chat Completions，或 Anthropic `messages` 的基础协议转换 |
| `/v1/messages` | Anthropic 原生 Messages，或 OpenAI `chat_completions` 的基础协议转换 |
| `/v1/responses` | OpenAI Responses、ChatGPT Web Responses 投影或 Codex OAuth 原生 Responses |
| `/v1/search` | ChatGPT Web `chat_completions` 派生的联网搜索入口 |
| `/v1/completions` | OpenAI 原生 Completions |
| `/v1/embeddings` | OpenAI 原生 Embeddings |
| `/v1/images/generations` | 图片生成能力 |
| `/v1/images/edits` | 图片编辑能力 |

协议转换按 `/v1/models` 返回的方向化 `capabilities.conversions` 和 `level` 开放：Level 1 为非流式纯文本，Level 2 增加纯文本 SSE，Level 3 增加非流式 function tools。该投影要求模型实现 capability 与至少一个具体 Provider 的 `provider/model/direction` 验证发布门闩同时通过；端点名称或模型级声明本身不能推出转换等级。

对于 `/v1/responses`，需要区分两类候选：

- 受限投影或跨协议转换只保证声明等级；未声明的 tools、并行工具控制、多模态、thinking/reasoning、JSON Schema、`response_format`、continuation、`metadata`/provider-specific 字段等改变结果语义的字段，网关会在访问上游前返回 `conversion_unsupported`，不得静默删除或改写。配置了方向专用 reasoning adapter 时，客户端 reasoning/thinking 控制映射到目标协议的固定 effort，但具体 effort/budget 不跨协议换算；上游 reasoning/thinking 输出块和 delta 只省略并标记 `conversion_degraded`，不转成普通文本。Level 3 的 tools 仅限非流式 function 定义/call/result，流式工具仍拒绝。
- OpenAI 原生 Responses 候选可以支持额外的原生能力（例如 `gpt-5.5` 的 JSON Schema Structured Outputs），但该能力必须由 provider/model 的独立 capability 验证和声明；不能仅凭 `responses` 端点标记推导。

因此，应用需要 JSON Schema 时，应选择已明确声明该能力的原生候选；不能因为模型同时出现在 `/v1/responses` 目录中，就假定所有候选都支持 JSON Schema。

## 5. `/v1/models` 与系统开放端点的关系

系统信息中的“开放 API 端点”是服务实例级路由清单，例如网关是否注册了 `POST /v1/messages`。它不代表每个模型都支持该路径。

如果应用同时能读取 Admin 系统信息，应使用以下交集判断最终入口：

```text
模型 supported_endpoints
∩
服务实例开放端点
```

通常外部数据面应用只需要使用 `/v1/models`；系统信息接口属于 Admin 管理面，需要 Admin 认证，不应暴露给普通业务客户端。

## 6. 缓存与错误处理建议

- 模型目录会在 Provider 启停、模型匹配、端点变更和账号池发现完成后刷新；应用应定期重新请求 `/v1/models`，不要永久缓存。
- 收到 `401` 时检查客户端 API Key，不要把它当作上游凭据失效处理。
- 收到端点不支持或模型不可用错误时，重新获取 `/v1/models`，然后重新选择模型与端点。
- 流式请求一旦开始返回数据，网关不会中途切换 Provider；应用应正确处理不完整流并按需重试。

## 7. 最小集成流程

```text
启动网关
   ↓
创建客户端 API Key
   ↓
GET /v1/models
   ↓
筛选目标模型 + supported_endpoints
   ↓
按完整客户端路径发送请求
   ↓
端点或模型变化时重新发现
```
