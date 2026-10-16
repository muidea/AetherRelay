# Codex WS 优先与安全 HTTP 回退

本次收口覆盖 `codexoauth` 的 Responses、Messages、Chat HTTP/JSON/SSE 入口及原生 `GET /v1/responses` WebSocket。上游默认先尝试 Responses WS；客户端继续使用原来的协议。`compact` 保持 HTTP，不尝试 WS。无需新增配置项。

参考 OpenAI Codex 源码快照 [`6ae8b8aa7c608730d4315d59c5c081897ae02780`](https://github.com/openai/codex/tree/6ae8b8aa7c608730d4315d59c5c081897ae02780)：`codex-rs/core/src/client.rs`、`codex-rs/codex-api/src/endpoint/responses_websocket.rs` 及 `core/tests/suite/websocket_fallback.rs`。它是离线适配依据，不等同于生产端点验收。

## 回退边界

| 条件 | 行为 |
|---|---|
| 握手 404（非结构化 model_not_found）、405、426、501 | 同一账号转 HTTP |
| 握手其它 5xx、连接失败或握手超时，且请求预算未耗尽 | 同一账号转 HTTP |
| 400、401、403、429、结构化 model_not_found | 保留原错误分类和既有刷新／账号调度规则，不转 HTTP 掩盖拒绝 |
| 已发送或尝试写入 response.create，随后连接断开、读取超时或协议失败 | 禁止转 HTTP；执行结果不明时标为不可自动重试，禁止换号重放，即使尚无客户端输出 |
| 客户端取消、预算耗尽、无完整历史可用 | 不发起 HTTP 回退 |
| 明确的 token、模型、限流或请求拒绝 | 保留既有有界重试规则；已向客户端输出业务事件后仍禁止重放 |

HTTP 回退发生在发送生成请求之前，账号、凭据、代理、模型和请求语义不变。回退本身不登记账号失败或冷却；HTTP 的最终结果才参与账号反馈。一次握手失败只触发一次 HTTP 回退，不循环切换两种 transport。

## 两种入口的会话行为

HTTP/JSON/SSE 每个请求创建并关闭一个上游 WS，沿用现有请求租约及 timeout guard。WS JSON 帧转换为内部 SSE 事件后，复用现有完成对象归集、metadata 消费、协议投影与 token 观测。`response.done` 规范为 `response.completed`。Anthropic 与 Chat 投影显式兼容 `response.metadata`、`codex.response.metadata` 与 `codex.rate_limits`，仅作为观测信息，不作为业务输出或完成证据。本次不引入跨 HTTP 请求的后台连接池或自动历史增量压缩。

原生 WS 保留已有连接复用、单连接串行 response.create、消息／会话／空闲／存活上限以及账号绑定。首次上游握手允许转 HTTP；随后该连接保持 HTTP，避免每轮重复探测 WS。每次发送仍执行账号模型黑名单检查。账号选择保留 HTTP 可用账号的资格，不因旧 WS 不支持快照排除该账号。

原生 WS 转 HTTP 后，代理必须在消息预算内重建完整、有序、通过工具 call/output 配对校验的 input，并删除 previous_response_id。显式 previous_response_id 必须匹配本连接最近一次成功响应；陈旧或未知 ID、历史超限或不完整均拒绝执行。完整替换 transcript 不拼接旧历史。上游执行结果不明时关闭会话，不重新构造请求重放。

无 previous_response_id 且自身工具 call/output 完整配对的 input 属于完整替换，首轮也可直接用于 HTTP 回退；仅缺少配对 call 的工具 output 沿用原生 WS 的隐式历史续接。历史被预算淘汰或引用非本地响应 ID 后，后续原生 WS 成功只证明上游仍有历史，不能用新输出恢复本地完整标记；只有新的完整独立输入才能恢复重放资格。原生 WS 的账号迁移同样检查 response ID，不能借迁移绕过上述限制。

第二轮及以后，HTTP 回退在 Send 阶段收到明确 HTTP 429 拒绝，与原生 WS 在 Pull 阶段收到的 typed 限流共用最多两次的账号迁移预算。首轮、无完整历史、已输出或请求已取消时不迁移；HTTP 网络错误及原生 WS 写入错误不进入此路径。失败耗尽后保留 typed 限流记账；本地 error 帧携带安全 type/code/message/param、status、retryable 与 retry_after_seconds，不在已升级连接上伪造 HTTP Retry-After 头。

`generate=false` 只作为预热：HTTP 回退会话在本地返回零用量的 created/completed，不向 HTTP 上游发起生成，也不宣称完成上游缓存预热。输入仍由代理保留，下一次 create 根据本地预热响应 ID 重建完整 input。独立 HTTP 请求没有该续接状态，不把 generate=false 回退成真实生成。

连接与 reader 的 owner 仍是 codexupstream Block，后台读取使用注入的 BackgroundRoutine，关闭、取消和 Teardown 排空读任务。原生连接的账号租约维持现有连接生命周期；本次不改变空闲连接的并发计费方式。Realtime、app-server WS 和 response.interrupt 扩展不在本次收口范围。

握手阶段的 caller context 取消必须关闭底层连接，覆盖直接连接、HTTP/HTTPS 代理 CONNECT 与 Upgrade 响应头等待；不能依赖 30 秒握手超时兜底。成功握手移交会话前须解除并等待取消回调，后续 WSOpen 命令 context 结束不能关闭已建立会话。取消或停机排空时不得启动 HTTP 回退。

## 记录与验收

每个原生 response.create 独立建立 EventID、usage completion 和归档 round，共享原始连接 request ID 以便关联。每轮分别保存 request.json、response.ws.jsonl、账号、模型、用量、首事件与终态，避免一条长连接只结算第一轮。首轮沿用已开始的握手 usage event；未发送 create 就关闭的连接沿用请求兜底结算。

上游尝试归档区分 GET 握手、WS create 和 POST HTTP。`upstream.response*.json` 中的 `upstream_transport`、`transport_fallback_reason`、`websocket_request_sent` 标明实际路径，失败握手与最终 HTTP 响应分别保留。普通回退日志只写有界原因及握手状态；完整正文遵循已有归档开关。

离线测试覆盖 WS JSON/SSE、metadata 与 done、拒绝分类、握手回退、发送后断线、跨账号重试阻断、多轮工具历史重建、完整工具记录替换、历史淘汰后多轮续接、陈旧 ID 及其后续污染拒绝、Send 429 迁移与次数上限、逐次用量、预热不生成、关闭后取消 HTTP reader，以及握手取消/超时/停机、两类代理 CONNECT 取消与成功连接移交。

部署后的 live 验收：

1. 分别以 Responses、Messages、Chat 的流式及非流式请求确认上游 WS create、真实终态、模型及 token，客户端协议保持一致。
2. 在可控测试上游拒绝 Upgrade 后确认仅一次 GET 和一次同账号 POST；426 是握手失败观测，最终 HTTP 200 才是生成成功。不要修改生产账号以制造故障。
3. 连续执行原生 WS 工具调用及 output 续接，核对每个 create 独立 EventID、完整历史次序和用量合计；旧 response ID 应在 HTTP 发出前被拒绝。
4. 更新绑定账号模型黑名单，确认下一次 create 被拒绝且没有 HTTP 绕行；当前已准入执行允许完成。
5. 在测试上游收到 create 后主动断线，确认没有 POST 回退或换号重放；关闭客户端及停机后没有遗留读任务。

当前为本地实现与离线验证范围，生产能力及延迟收益需部署后核对，不据此声明 live 已通过。

## 17.5.2 附加事件兼容

x800 部署 `dec0efa` 后，上游正常 Upgrade 为 101，但 Messages 流在 `responsesapi.websocket_timing` 被本地转换器拒绝。两个指定事件都已生成 Edit 工具参数，尚未读到正式终态便取消上游。101 与已提交的客户端 HTTP 200 均不构成请求成功。

共享 `aetherrelaycodex.ObservationFeature` 精确识别 `response.metadata`、`codex.response.metadata`、`codex.rate_limits` 和 `responsesapi.websocket_timing`，不在 WS reader 中丢弃。Messages/Chat 投影省略并记录固定降级项，Responses SSE、原生 WS 和完整正文归档保留。内部新字段透明接纳；未知业务事件仍按转换能力拒绝。timing 的 `num_sampled_tokens_total`、`engine_total_prompt_tokens_total` 及 critical_path 均为观测数据，不能替代正式完成、输出证据或 usage。

本地验收矩阵包含三种 HTTP 入口的流式/非流式文本和工具输出、WS 成功与握手安全回退的完成/不完整/失败/断线、仅遥测和空完成拒绝、原生 WS 多轮透传及用量隔离、compact 仍使用 HTTP 并要求压缩项。失败时保留首输出时间与正耗时，不要求已取得 Token；Token/TPS 未知保持未知。已有 schema 和 CSV 列保持不变，不回填历史记录。新 attempt 清除旧观测，迟到握手不覆盖已结算失败。

live 除上述步骤外，必须抽查实际包含 timing 的请求：转换前归档继续出现正式 completed/incomplete 和 usage，Messages 有唯一 message_stop，Chat 有 finish_reason 和 [DONE]，原生接口保留 timing。缺终态仍失败且无自动重放，部分输出观测在缺 usage 时仍可见。详细事件与验收见 [x800 收口记录](x800-codex-conversion-closure-2026-10-16.md#9-1752-ws-timing-及全接口收口)。
