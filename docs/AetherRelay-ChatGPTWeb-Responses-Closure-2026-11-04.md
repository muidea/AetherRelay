# ChatGPT Web Responses 适配收口记录

日期：2026-11-04（UTC+08）。本次承接旧 Chat/Text Completions 入口清理后的 Responses 深入核对；实现范围为 GPTWeb 受限适配，未扩展上游原生能力。

## 交付边界

| 核对问题 | 实现与验收 |
| --- | --- |
| 搜索提前分流，跳过公共校验 | 公共 allowlist 先于 executor 选择；普通文本、搜索、HTTP/SSE 都在执行前拒绝不支持的语义，回归核对上游调用次数为零 |
| 参数被静默删除或默认格式被拒绝 | 普通 text 格式及无工具/同步执行默认值可用；采样、输出 token 限制、Structured Outputs、旧 response_format、未知控制字段明确拒绝；metadata 校验回显 |
| 搜索部分正文冒充成功 | `aetherrelaycompletion.Search` 穿过 client、Block、Biz 和 HTTP；稳定快照不终止、超时不成功，明确区分正常结束、模型截断与失败 |
| SSE 不完整 | 共享 response envelope 和事件 writer，固定标识/创建时间，递增 sequence_number，完整 output item/content part/text 生命周期，终态互斥 |
| 引用不符合 Responses | annotations 平铺字段，索引针对最终正文；官方 Go SDK 验证 URL、标题与正文 |
| 能力与用量说明不足 | `/v1/models` 发布 `capabilities.chatgptweb.responses`；HTTP/SSE 用量头和管理页详情说明可见文本估算，不承诺上游计费准确性 |

## 搜索结果合同

- 正常完成：有效已知终态、非空有界正文、长度与摘要相符，返回 `completed`。
- 模型截断：`max_tokens` / `length`，正文证据一致，Responses 返回 `incomplete` 和 `incomplete_details.reason=max_output_tokens`。用量结算为 `text_model_length`，不计成功、不据此冷却账号或重投。
- 无证据、证据与正文不符、未知结束原因、预算超限或超时：失败。Responses 在可写时保留部分正文；流式发送 `response.failed`，非流式返回非 2xx 的 failed response。
- 非法类型的结束证据不会回退为成功；`stop` 不能覆盖进行中或失败状态。只有来源而无可见正文时，不生成部分答案。
- `/v1/search` 与 Admin 消费者要求完整答案，不能把截断结果作为成功历史保存。
- 普通文本原有的严格完成合同保持：模型长度终止仍按 `text_model_length` 失败处理，本次 `incomplete` 投影仅用于有明确截断证据的搜索结果。

## 验收

- `chatgpt_responses_contract_test.go`：公共拒绝矩阵、执行前拒绝、默认值/metadata、SSE 生命周期与标识、搜索终态/用量、官方 SDK 正文与引用。
- `search_completion_test.go`：已知终态、稳定快照直到正常完成/取消、超时保留部分结果、正文预算不静默截断。
- `chatgpt_search_test.go`：有明确截断证据、缺少证据或正文摘要不符时，不计账号成功、不触发冷却或重投。
- 现有搜索准备阶段恢复、账号反馈、HTTP 上下游取消、文本完成性、归档与统计测试继续作为回归门禁。
- 本次验证使用本地合成上游与真实 SDK，不产生外部模型调用。真实 GPTWeb 联调与部署验收另行执行。

### 本地验证结果

- `go test ./... -count=1 -timeout 180s`：62 个有测试包通过，48 个包无测试。
- 四个相关包（Proxy HTTP、Proxy Biz、Web client、Web Block Biz）的完整回归与 `-race` 检查通过；最终边界补充后再次核对受影响包。
- 官方 Go SDK 的 Responses JSON、SSE、正文及搜索引用用例通过。
- `go vet ./...` 通过；后续边界补充的相关包静态检查通过。
- `node --test web/admin/*.test.cjs`：96 项全部通过。
- `scripts/check-format.sh`、新增 Go 文件格式检查和 `git diff --check` 通过。

本轮仅修改代码、测试与文档，未提交、推送或部署。

正式行为以 [ChatGPT Web 设计](design/chatgpt-web.md)、[功能说明](features.md)及[客户端接入](integration.md)为准。
