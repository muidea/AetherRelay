# ChatGPT Web 用量统计接入设计（2026-07-27）

> 状态：implemented
>
> 范围：将 ChatGPT Web 相关调用正确写入 `aiproxyusage` 用量权威，使 Admin「使用统计」可观测
> 代理与管理台调试流量，并使 Prometheus 客户端用量镜像可观测代理 `chatgptweb` 流量。
> 本文是实现与验收依据；不覆盖账号池 success/fail 计数、临时会话正文表或图片任务详情内的
> `tokenusage` 投影（那些仍由各自 owner 维护）。

## 1. 决策摘要

用量权威唯一且已存在：`internal/pkg/aiproxyusage`（DuckDB `usage_events`），跨组件经
`usageruntime` EventHub 的 `Start` / `Complete` 合同写入。ChatGPT Web **不新建**平行用量表，
不扩展通用 document/KV API，也不让 Admin HTTP 或上游 client 直接写 DuckDB。

当前三条 ChatGPT Web 路径均未正确接入该权威：

| 路径 | 是否 `Start` | 是否正确 `Complete` | 问题 |
| --- | --- | --- | --- |
| 代理文本 `POST /v1/chat/completions` → `chatgptweb` | 是（`ServeHTTP.beginUsage`） | 否 | 成功路径不调用 `recordAndPrint`/`completeUsage`，`defer completePendingUsage` 把成功记成 `error` / HTTP 500 / `proxy_internal_error` |
| 代理图片 `POST /v1/images/generations\|edits` → `chatgptweb` | 是 | 否 | 同上；上游 `Usage` 未投影 |
| Admin 临时对话 `chatgpttemporarychat` | 否 | 否 | 完全不经 proxy；只有账号池 `RecordTextResult` |
| Admin 图片任务 `chatgptimagetask`（可选） | 否 | 否 | 任务表可有 `tokenusage.Usage`，与全局 usage 无关 |

本设计分阶段收口：先修代理路径误记账（数据正确性），再接入临时对话（Admin 调试流量可观测），
图片任务是否进全局 usage 作为可选阶段由产品确认。

文本 token 在 ChatGPT Web SSE 未解析真实 usage 前，使用
`chatgpttokenusage.EstimateChatTextUsage`，`CompleteRecord.Estimated = true`。不得把估计值表述为
上游账单。

实现前必须先补齐 proxy 本地执行端口：文本结果投影 `ActualModel` 和结构化失败，图片结果投影
有界 `Usage`。HTTP adapter 不直接依赖 `chatgptwebupstream` 事件类型；`proxyapi` Biz 负责把上游
owner 合同映射成自己的窄端口。

## 2. 目标与非目标

### 2.1 目标

1. 代理路径上所有已 `Start` 的 `chatgptweb` 请求在退出时有一次且仅一次正确 `Complete`。
2. 成功、上游失败、客户端取消、客户端写失败、进程中断等 outcome 与现有 proxy 合同一致，不再依赖
   `completePendingUsage` 作为正常成功路径。
3. usage 行可按 `provider=chatgptweb`、`model`、`api_key_id`、`outcome`、`estimated` 筛选与聚合。
4. Admin 临时对话每一轮（turn）写入独立 usage 事件；`api_key_id` 使用稳定 Admin 维度，不与
   客户端 Key 混淆。
5. usage / metrics label / 错误 envelope 不出现会话正文、access token、上游 `conversation_id`、
   `parent_message_id` 或原始 SSE。
6. 跨 Module 写入只经 `usageruntime` typed EventHub；`chatgpttemporarychat` / `chatgptimagetask`
   不直接依赖 `aiproxyusage` 实现或 DuckDB 连接。

### 2.2 非目标

1. 不修改 `aiproxyusage` schema 主键或新增第二套统计 authority。
2. 不把临时对话历史表、图片任务表改造成 usage 读模型。
3. 不在本轮实现上游 SSE 真实 token 解析（可后续增强；字段预留 `Estimated=false` 路径）。
4. 不回溯修复本设计落地前已被 `completePendingUsage` 误记为 error 的历史行。
5. 不把 research 专用模型用量单独建模（research 模型本就不暴露给目录/临时对话）。

## 3. 权威归属

| 数据 | 权威 owner | 写入路径 |
| --- | --- | --- |
| 请求级用量明细与聚合 | `usageruntime` / `aiproxyusage` | EventHub `Start`/`Complete` 或 proxy 持有的 EventHub-backed Store |
| 客户端 Key 身份 | `proxyapi` + `aiproxyclientauth` | 仅代理入站；Admin 路径不用客户端 Key |
| ChatGPT 账号 success/fail | `chatgptaccountpool` | `RecordTextResult` / `MarkImageResult`（与 usage 并行，不替代） |
| 临时会话正文与续聊锚点 | `chatgpttemporarychat` | 专用 DuckDB 表；不进 usage |
| 图片任务状态与任务内 Usage | `chatgptimagetask` | 任务 payload；可选再投影一条全局 usage |
| 上游 transport / SSE | `chatgptwebupstream` | 不持久化到 usage；只投影 ErrorClass、actual model 与有界 Usage |

## 4. 字段映射合同

### 4.1 通用 Complete 字段

所有 `chatgptweb` 路径共用：

| Complete 字段 | 代理文本 | 代理图片 | 临时对话 turn |
| --- | --- | --- | --- |
| `Provider` | `chatgptweb` | `chatgptweb` | `chatgptweb` |
| `Model` | 请求 model；若上游明确返回 actual model 则优先 actual | 请求 model | 会话固定 model；若上游明确返回 actual model 则优先 actual |
| `UpstreamProtocol` | `chatgptweb` | `chatgptweb` | `chatgptweb` |
| `UpstreamEndpoint` | TransportPlan 值（如 `chatgptweb`） | `chatgptweb_images` 或矩阵值 | 合成标签 `chatgptweb_temporary_chat` |
| `ConversionMode` | `native` | `native` | `native` |
| `Stream` | 请求 `stream` | `false` | `true`（内部流式 pull） |
| `Estimated` | 文本估计时为 `true` | 有真实 Usage 则为 `false` | 文本估计时为 `true` |
| `InputTokens` / `OutputTokens` | 估计或真实 | 来自上游 Usage 或 0 | 估计 |
| `HTTPStatus` | 实际写出状态；流式已提交 200 后失败仍保留 200；尚未提交响应且客户端已断开时使用 499 约定值 | 实际写出状态 | 记录启动 turn 请求的实际响应：接受后固定 `202`；接受前账号不可用 `503`；启动上游失败 `502` |
| `Outcome` | 与 §4.5 结构化映射一致 | 与 §4.5 一致 | `success` / `client_canceled` / `upstream_failed` / `process_interrupted`；禁止成功写成 `error`+`proxy_internal_error` |

### 4.2 Start 字段

| Start 字段 | 代理路径 | 临时对话 |
| --- | --- | --- |
| `EventID` | 现有 `newRequestID()`（不可用客户端 `X-Request-ID`） | 每 turn 新 UUID；**不得**用 `conversation_id`/`turn_id` 当全局主键 |
| `RoundID` | interaction archive round | `0`（临时对话不进 interaction archive） |
| `APIKeyID` | 客户端 Key ID | 见 §4.3 |
| `Operation` | `OperationForPath`（`chat_completions` / image ops） | `chat_completions` |
| `Route` | `RouteLabel(r)` | 固定 `admin_temporary_chat` |
| `ClientEndpoint` | 规范化入站 path | 合成 `/admin/chatgpt/temporary-chat`（非真实对外 path，仅标签） |
| `ClientProtocol` | `openai` / 入站协议 | `admin` |
| `Provider` / `Model` | 解析到 plan 后可在 Start 补写或仅在 Complete 写；本设计要求 **Complete 必填** Provider/Model | Start 即写 `chatgptweb` + 会话 model |

### 4.3 Admin `api_key_id` 维度

临时对话（及若启用的 Admin 图片任务）使用：

```text
admin:<owner_id>
```

其中 `owner_id` 为服务端认证 Admin principal（与临时对话 owner 相同，通常为管理员用户名）。
不得使用浏览器提交值，不得使用 ChatGPT 账号 ID，不得留空导致并入 `default`。

筛选与文档中应说明：使用统计里出现的 `admin:*` 表示管理台调试流量，不是客户端 API Key。

### 4.4 Outcome 与取消

与现网 proxy 对齐，至少覆盖：

- `success`
- `client_canceled`（客户端断开或 Admin 取消 turn）
- `client_write`（代理已开始回写后客户端连接写失败）
- `upstream_failed`（上游终态失败、invalid_token、rate_limit 等归类后的业务失败）
- `process_interrupted` / 启动恢复路径已有 `OutcomeProcessInterrupted`（仅 usage 恢复遗留 started 行）

临时对话：用户点取消 → `client_canceled`；进程 teardown 将 streaming 标 interrupted →
`process_interrupted`，与 usage 启动恢复语义一致。`CancelTurn` 只设置取消标志并取消上游，不能直接
Complete；已注册 runtime 后由 `runTurn` worker 唯一结算，Teardown 设置 stopping/cancel 并等待 worker。

不确定上游是否已消费消息时，会话状态机仍为 `recovery_required`；usage 仍应 Complete，避免
永久 `started`。

### 4.5 结构化失败合同

`proxyapi` owner 定义自己的 `FailureKind` / typed error（可放在 text/image 共用的 focused package），
至少包含：

- `client_canceled`
- `client_write`
- `provider_unavailable`
- `invalid_token`
- `rate_limit`
- `content_policy`
- `tls`
- `timeout`
- `upstream`
- `internal`

建议的最小本地合同：

```go
type Failure struct {
    Kind  FailureKind
    Cause error
}

type TextResult struct {
    ConversationID string
    ActualModel     string
    Text            string
}

type ImageResult struct {
    Created int64
    Data    []Data
    Usage   *tokenusage.Usage `json:"-"`
}
```

`Failure` 实现 `error` / `Unwrap`，调用方使用 `errors.As` 分类。`proxyapi` Biz 将
`chatgptwebupstream.ErrorClass` 映射为上述本地类型；HTTP adapter 不导入上游 owner 合同，也不使用
错误字符串包含判断。文本与图片 Executor 即使返回错误，也应尽量同时返回已经累计的文本、actual
model 或 Usage 等有界结果。

| FailureKind | Outcome | ErrorCode | 计入 upstream error 指标 |
| --- | --- | --- | --- |
| 无错误 | `success` | 空 | 否 |
| `client_canceled` | `client_canceled` | `client_canceled` | 否 |
| `client_write` | `client_write` | `client_write` | 否 |
| `provider_unavailable` | `upstream_failed` | `provider_unavailable` | 否 |
| `invalid_token` | `upstream_failed` | `invalid_token` | 是 |
| `rate_limit` | `upstream_failed` | `rate_limit` | 是 |
| `content_policy` | `upstream_failed` | `content_policy` | 否 |
| `tls` | `upstream_failed` | `tls` | 是 |
| `timeout` | `upstream_failed` | `timeout` | 是 |
| `upstream` | `upstream_failed` | `upstream` | 是 |
| `internal` | `error` | `proxy_internal_error` | 否 |

`streamFail`（或等价的结算值）必须把 `Outcome` 与 `ErrorCode` 分开，不能继续用一个 Kind 同时表达
两者。客户端上下文取消优先归类为 `client_canceled`；response writer 返回写错误归类为
`client_write`；两者都不得误记为上游故障。

### 4.6 Token 估计与累计规则

文本（代理与临时对话）：

```text
promptParts = 本轮提交给上游的文本 parts
  - 代理：请求 messages 的 content
  - 临时对话：若首轮带 system prompt 则包含 system + 本轮 user；续聊仅本轮 user
completion = 最终 assistant 文本（取消/失败时用已累计增量）
Usage = EstimateChatTextUsage(promptParts, completion)
Estimated = true
```

图片：

- 有 `*tokenusage.Usage` → 映射 Prompt/Completion 或 Input/Output，`Estimated=false`
- 无 Usage → tokens 置 0，`Estimated=false`（只统计请求次数与 outcome）
- `n > 1` 时一次代理请求仍只产生一个 usage event，`proxyapi` Biz 累加每次实际发生的上游调用 Usage
- 前序图片调用成功、后续调用失败时，Executor 通过非零 `Result, error` 返回已经产生的累计 Usage；
  最终 event 记录失败 outcome，但不得丢失已发生的 token

## 5. 代码落点

| 位置 | 职责 |
| --- | --- |
| `proxyapi/pkg/chatgpttext` | 扩展本 owner 的文本结果与 typed failure；投影 `ActualModel`，错误时保留有界部分结果 |
| `proxyapi/pkg/chatgptimage` | 在内部结果增加 `Usage`（响应 JSON 不暴露），允许失败时携带已发生的累计 Usage |
| `proxyapi/pkg/<focused failure package>`（若 text/image 共用） | 定义 proxy owner 内部 `FailureKind` / `Failure`，只包含纯类型与映射无关 helper |
| `proxyapi/biz/chatgpt_text.go` | 将上游 actual model / ErrorClass 映射到 proxy 本地端口，不向 HTTP adapter 泄漏上游事件类型 |
| `proxyapi/biz/chatgpt_image.go` | 累加 `n` 次调用 Usage，包含部分成功后失败的已发生用量 |
| `proxyapi/service/proxy/chatgpt_web.go` | 代理文本所有出口显式结算；估计 token；填充 provider/model/plan 元数据；区分取消、客户端写失败与上游失败 |
| `proxyapi/service/proxy/chatgpt_images.go` | 代理图片所有出口显式结算；投影 executor 返回的累计 Usage |
| `proxyapi/service/proxy/handler.go` | 仅为复用结算与 metrics 做最小接线；让 outcome/error code 独立；**禁止**把成功路径继续交给 `completePendingUsage` |
| `chatgpttemporarychat/biz` | Setup 获取 EventHub-backed usage port；StartTurn 登记/早期失败结算，runTurn 唯一结算已注册 runtime，Cancel/Teardown 只驱动终态 |
| `chatgpttokenusage` | 复用估计函数；本轮原则上不改 API |
| `usageruntime` / `aiproxyusage` | 无合同变更；可加测试夹具帮助调用方 |
| `docs/operations.md` / `docs/configuration.md`（若有相关说明） | 记录 `chatgptweb` 与 `admin:*` 维度、估计 token 含义 |
| 本文 | 实现完成后状态改为 `implemented` 并记录验证结果 |

`adminapi` HTTP 层不写 usage。`chatgptwebupstream` 不写 usage；它只通过自己的 typed result 投影
ErrorClass、actual model 或有界 Usage，由调用方 owner 再映射。

## 6. 分阶段实施

### Phase 1 — 代理文本路径修正（P0）

**目标**：消除 chatgptweb 文本成功被记为 error 的缺陷。

1. 先扩展 `chatgpttext` 本地端口及 Biz adapter，保留 actual model、部分文本和 typed failure。
2. 在 `handleChatGPTWebChatCompletions` 所有出口（参数错误、executor 不可用、非流式成功/失败、
   流式成功/失败/中途错误）调用与
   其它上游路径相同的结算入口（优先复用 `recordAndPrint` / `recordAndPrintFail`，保证 metrics 一致）。
3. Complete 填 `provider=chatgptweb`、actual model（缺失时请求 model）、plan 的 upstream 字段、估计
   token、`estimated=true`。
4. 确保正常出口的显式 Complete 先于 `completePendingUsage`，使 defer 成为 no-op；Store Complete
   失败需记录健康与告警，遗留 started 行由启动恢复处理。
5. 流式：已写出 200 后仍按真实 outcome 结算并保留 HTTP 200；尚未提交响应即取消可使用 499；
   客户端取消和客户端写失败不得记 upstream 故障。
6. 单测：fixture 或 stub executor，断言 success、upstream fail、client cancel、client write 的
   Provider/Model/Estimated/Outcome/ErrorCode 均正确，实际模型优先于请求模型。

### Phase 2 — 代理图片路径修正（P0）

**目标**：图片同步 API 与文本同一正确性。

1. 扩展 `chatgptimage.Result` 与 Biz adapter，累加实际执行的所有上游 Usage；`Usage` 标记为
   `json:"-"` 或通过内部响应 DTO 与对外 JSON DTO 分离。
2. `handleImages` 在成功 encode 响应前 / 失败 writeAPIError 后显式 Complete。
3. 映射 `operation`（generations vs edits）与 TransportPlan endpoint。
4. 投影 executor 返回的 Usage；缺失则 tokens=0。`n > 1` 部分成功后失败仍记录已有累计 Usage。
5. 单测覆盖 generation/edit success、failure、`n > 1` 累计与部分失败。

### Phase 3 — Admin 临时对话接入（P1）

**目标**：管理台多轮调试流量进入使用统计。

1. `TemporaryChat` Setup：当 chatgpt_web + temporary_chat 启用时同步调用
   `usageevents.RequestStore`；失败直接 fail-fast。该 port 只保存 EventHub/source 与 typed mapping，不得
   返回或持有 `aiproxyusage` owner 的 DuckDB/store 实现。
2. `StartTurn` 固定采用以下顺序：

   ```text
   store.StartTurn
     -> usage.Start
     -> acquireTextAccount
     -> StartText upstream stream
     -> register turn runtime / worker
     -> Admin HTTP 202
   ```

3. `store.StartTurn` 失败时不创建 usage；`usage.Start` 失败时不访问账号池或上游，将本轮标记为
   `usage_unavailable` 并让 Admin 返回 503。
4. `usage.Start` 成功后，账号不可用以 HTTP 503 + `upstream_failed/provider_unavailable` Complete；
   启动 stream 失败以 HTTP 502 + `upstream_failed/<具体错误类>` Complete。
5. `event_id` 仅保存在调用栈或 turn 内存运行态，不写临时会话 DuckDB 表。进程崩溃遗留的 started
   行由现有 `RecoverInterrupted` 处理。
6. runtime 注册后，`runTurn` worker 是唯一 Complete 方：`CancelTurn` 只设置取消标志并取消上游；
   Teardown 设置 stopping/cancel、等待所有 worker 完成 usage 与会话终态写入后再关闭 store。
7. 已返回 Admin HTTP 202 的 turn，无论最终 success、upstream_failed、client_canceled 或
   process_interrupted，usage `HTTPStatus` 均为 202，最终业务状态只看 `Outcome`。
8. token 按 §4.6；outcome/error code 按 §4.4/§4.5。
9. 测试：fake usage Store；成功首轮/续轮、账号不可用、上游启动失败、取消、上游终态失败、
   teardown interrupt、取消/完成竞态、usage Start 失败不访问上游。

### Phase 4 — Admin 图片任务（P2，可选）

**仅当产品确认异步任务需要进入全局使用统计时实施。**

1. submit 时 Start（`api_key_id=admin:<server principal>`，`route=admin_image_task`）。usage 身份不得直接
   使用浏览器提交的图片任务 `owner_id`；任务 owner 与认证 Admin principal 是两个维度。
2. 任务终态 MarkSuccess/MarkError 时 Complete，tokens 来自任务内 Usage。
3. 若产品决定不接入：在本文 Phase 4 标记 `deferred`，任务详情继续只显示任务级 Usage。

## 7. 与现有机制的交互

### 7.1 `completePendingUsage`

保留作为**漏结算兜底**，不得作为 chatgptweb 正常成功路径。Phase 1/2 完成后，chatgptweb 成功请求
的 defer 必须因 `usageCompletion.done` 直接返回。

### 7.2 Metrics

proxy 路径必须通过 `recordAndPrintFail`（或等价统一入口）调用：

- `RecordClientUsage(api_key_id, prompt, completion)`
- `RecordRequestPlan(...)`
- `RecordTokens(...)`

临时对话 Phase 3 只要求 DuckDB usage 权威正确，不写 Prometheus 客户端用量镜像。原因是异步 turn
不是客户端 API Key 请求，强行复用 proxy 指标会混淆 HTTP 请求与业务终态。若后续确需指标，应新增
独立的、低基数的 Admin turn metrics 设计；label 可使用 `admin:*`，但禁止使用 conversation/turn id。

### 7.3 账号池计数

`RecordTextResult` / `MarkImageResult` **继续保留**，表示账号健康与配额行为，不替代
`aiproxyusage` 请求级统计。

### 7.4 历史数据

落地前误记的 chatgptweb 行保持原样。运维文档说明：以本设计落地版本为界，此前 `provider` 空且
`outcome=error`/`proxy_internal_error` 的短请求可能是误伤成功流量，不可直接用于对账。

## 8. 安全与隐私

1. usage 行禁止包含：消息正文、system prompt、access token、上游 conversation/message id、原始 SSE。
2. 日志最多：截断 event_id、api_key_id、provider、model、outcome、estimated、duration、error_class。
3. 临时对话 owner 与 `admin:` 前缀仅来自服务端 principal。
4. Admin 使用统计 UI 已有的脱敏与 no-store 行为保持不变；本设计不新增导出明文。

## 9. 验收标准

### Phase 1

1. 对 `chatgptweb` 模型发非流式/流式 chat completions 成功：usage 存在 `provider=chatgptweb`、
   正确 model、`outcome=success`、`estimated=true`、input/output tokens ≥ 0 且至少一侧反映估计逻辑。
2. 上游失败：`outcome` 为上游失败类，**不是** 默认 `proxy_internal_error`（除非本地 bug）。
3. 上游返回 actual model 时 usage 使用 actual model；未返回时使用请求 model。
4. 客户端取消流式：`client_canceled`；客户端写失败：`client_write`；二者均不计入 upstream error。
5. TLS、timeout、invalid_token、rate_limit 的 Outcome 均为 `upstream_failed`，ErrorCode 保留具体类别。
6. 同一 event 不会被 Complete 两次导致重复 tokens（调用方单结算 + Store 条件更新合同）。

### Phase 2

1. images generations/edits 成功与失败均有正确 Complete。
2. 有 Usage 时 tokens 非估计；无 Usage 时 tokens=0 且请求仍可统计。
3. `n > 1` 累计所有已发生上游调用的 Usage；部分成功后失败不丢失前序 Usage。

### Phase 3

1. 临时对话首轮与续轮各产生独立 usage 事件；`api_key_id` 为 `admin:<server owner>`。
2. 账号不可用和启动 stream 失败在返回 503/502 前均 Complete；usage Start 失败时不访问上游。
3. 已被 Admin 接受的 turn 均记录实际 `HTTPStatus=202`，最终状态由 Outcome 表达。
4. 取消、上游失败、teardown interrupt 均 Complete，无长期 `started` 泄漏（崩溃恢复除外并由
   RecoverInterrupted 收口）。
5. 取消/完成/teardown 竞态下仍只有一个运行态结算方，Store 中只有一条 completed event。
6. usage / 日志中无会话正文与上游锚点。
7. Admin 使用统计页按 provider=`chatgptweb` 或 api_key 前缀可筛到调试流量。

### 全量

- 相关包测试：`proxyapi`、`chatgpttemporarychat`、`aiproxyusage`（及 Phase 4 时 imagetask）
- `GOCACHE=/tmp/ai-proxy-gocache go test ./... -count=1`
- `make build`
- `scripts/check-format.sh`

## 10. 实施顺序清单（供逐项收口）

- [x] **P1.1** 扩展 `chatgpttext` 本地结果/typed failure，并在 Biz adapter 投影 actual model 与错误类
- [x] **P1.2** 梳理 `handleChatGPTWebChatCompletions` 全部出口，接入统一结算
- [x] **P1.3** 实现代理文本 Complete + 估计 token + plan 元数据 + Outcome/ErrorCode 独立映射
- [x] **P1.4** 代理文本单测（success / actual model / upstream fail / cancel / client write）
- [x] **P2.1** 扩展 `chatgptimage` 内部 Result，并在 Biz 累加 `n` 次及部分失败 Usage
- [x] **P2.2** 实现代理图片 Complete + Usage 投影
- [x] **P2.3** 代理图片单测（generation / edit / failure / n 聚合 / 部分失败）
- [x] **P3.1** temporarychat Setup 获取 EventHub-backed usage port
- [x] **P3.2** 固定 StartTurn 顺序、早期失败结算与 HTTPStatus 口径
- [x] **P3.3** runTurn 唯一结算，Cancel/Teardown 只驱动终态
- [x] **P3.4** temporarychat usage 单测（usage Start 失败、账号不可用、上游启动失败、成功 worker 结算与 race 检查）
- [x] **P4**（可选，deferred — 本轮不实施）imagetask 全局 usage
- [x] **Docs** 更新 `docs/operations.md`（chatgptweb 与 admin:* 维度、估计 token、历史误记说明）
- [x] **Close** 本文状态改为 `implemented`，记录最终字段与测试结果

## 11. 后续文档收口

实现完成后更新：

- `docs/operations.md`：使用统计中的 `chatgptweb` / `admin:*`、估计 token、历史数据边界
- 若 README 有用量简述：补一句 ChatGPT Web 已纳入同一 DuckDB 权威
- 本文：状态 `implemented` + 验证记录

不要求修改临时对话设计文档的会话合同；仅在其「可观测」相关处交叉引用本文即可。

## 实现记录（2026-07-27）

### 已完成

- **Phase 1**：`proxyapi/pkg/chatgptfail`、扩展 `chatgpttext`（ActualModel + typed Failure）；`handleChatGPTWebChatCompletions` 全出口显式 `recordAndPrintFail` 结算；估计 token、`estimated=true`；success / upstream_failed / client_canceled 单测。
- **Phase 2**：`chatgptimage.Result.Usage`（`json:"-"`）；Biz 累计 `n` 次调用 Usage，且第 `n` 次调用失败仍保留已发生 Usage；`handleImages` 显式 Complete，并用 `chatgptweb_images` 标记图片上游端点。
- **Phase 3**：`chatgpttemporarychat` Setup 注入 EventHub-backed usage Store；顺序 `store.StartTurn → usage.Start → acquire → StartText → worker`；`runTurn` 唯一 Complete；`api_key_id=admin:<owner>`；HTTPStatus 202（接受后）/ 503 / 502（接受前）。Setup 后续依赖失败会关闭已打开的临时会话 store，usage Complete 采用 5 秒超时并记录告警。
- **Phase 4**：deferred（Admin 图片任务不进全局 usage）。

### 验证

- `scripts/check-format.sh` 通过
- `GOCACHE=/tmp/ai-proxy-closure-gocache go vet ./...` 通过
- `GOCACHE=/tmp/ai-proxy-closure-gocache go test ./... -count=1` 通过
- `GOCACHE=/tmp/ai-proxy-closure-race-gocache go test -race ./internal/modules/application/chatgpttemporarychat/biz ./internal/modules/application/proxyapi/service/proxy -run 'Test(StartTurnSuccessStartsUsageThenWorkerCompletes|ChatGPTWebText|ChatGPTImage)' -count=1` 通过
- `make build` 通过
