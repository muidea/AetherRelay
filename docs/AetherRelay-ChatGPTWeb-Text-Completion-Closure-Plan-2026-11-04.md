# AetherRelay ChatGPT Web 文本完成性收口方案

- 日期：2026-11-04（UTC+08）。
- 核对基线：`37ecbf6acc4e42a37484c461c246941ca492cb8f`。
- 状态：代码已实施，本地全量测试、相关竞态检查与静态检查通过；实现、测试与文档纳入本次提交，CI 与部署验收仍待执行。实际结果见第 12 节。
- 范围：ChatGPT Web 文本上游读取、协议与快照完整性、完成判定、跨层结果合同、Chat Completions／Responses HTTP/SSE 转换、临时对话消费者兼容、错误与重试、诊断及长响应预算。
- 评审修订：纳入协议事件静默丢弃、快照改写拼接、Responses 适配遗漏和临时对话消费者遗漏四项评审结果；下文已展开为实施要求与验收门禁。
- 触发场景：ThoughtFlow 的成果集规划收到 HTTP 200 和 `finish_reason=stop`，但正文存在 JSON 语法错误及未完成尾部。

## 1. 收口目标与责任边界

优先修复“部分输出被当成完整成功返回”，随后评估长响应预算对成功率的影响。单纯增加读取上限、兼容输出包装或增加重试，均不能替代完成性判定。

AetherRelay 负责如实表达传输状态、上游终止证据、执行状态和失败原因。ThoughtFlow 负责 JSON 语法、字段协议、来源、范围与业务发布门禁。传输完整不等于模型生成的 JSON 合法，HTTP 200 也不是业务校验通过的证明。

本方案不要求网关修补 JSON、补全括号、截取可解析的前半段、删除协议外业务字段，或通过额外生成掩盖失败。ChatGPT Web 不支持的 `response_format` 仍须如实报告；客户端默认 fallback 及结果校验继续由客户端管理，不新增配置开关作为本次修复前置条件。

本文是独立收口计划，不替代现有[ChatGPT Web 设计](design/chatgpt-web.md)、[代理核心合同](design/proxy-core.md)及[代码结构约束](structure.md)。实施完成后需同步对应的正式合同与自动化测试。

## 2. 基线缺陷证据与限制

| 编号 | 核对内容 | 证据层级与结果 |
| --- | --- | --- |
| E01 | 未收到完成标记，已有部分正文后遇普通 EOF | 使用本机实际 `ParseTextSSE` 和合成事件复现：返回 `error=nil`、`Done=false` 及部分正文 |
| E02 | 累计 SSE 触达 16 MiB 读取上限 | 使用实际解析函数及相同的 `io.LimitReader` 构造复现：同样返回 `error=nil`、`Done=false` 及部分正文 |
| E03 | 非流式 HTTP 完成原因 | 源码确认：适配器在执行返回成功后固定填写 `finish_reason=stop` |
| E04 | 最终正文超过上限 | 源码确认：`bounded(text, maxTextContentBytes)` 按字节截取；此分支尚未独立运行故障注入 |
| E05 | 客户端本次失败响应 | 私有响应长度、摘要与报错一致；正文中间缺少数组终止符，尾部字符串也未完成 |
| E06 | 远端部署与真实原始 SSE | 未核实远端提交，未取得本次原始上游 SSE，不能将本机缺陷直接认定为本次线上故障的唯一根因 |
| E07 | 损坏的正文事件后收到终止标记 | 临时 Go overlay 调用实际 parser：合法 `AB` 快照 → 缺少闭合符的最终 `ABC` 快照 → `[DONE]`，返回 `error=nil`、`Done=true`、正文仍为 `AB` |
| E08 | 非前缀快照被当作增量追加 | 临时 Go overlay 调用实际 parser：`AB → AC → [DONE]`，返回 `error=nil`、`Done=true`、最终正文 `AC`，但累计发出正文为 `ABAC` |
| E09 | Responses 独立适配器 | 源码确认：`chatgpt_responses.go` 复用文本 Executor，但独立生成非流式 `status=completed` 及流式 `response.completed`／`response.failed`，必须单独验收 |
| E10 | 临时对话直接消费 owner 结果 | 源码确认：`chatgpttemporarychat/biz/biz.go` 直接消费 `PullTextResult`，自行拼接、持久化、判定恢复和重试；不经过 proxyapi Executor |

本次客户端关联元数据：

```text
request_id=evt-20261104-112423-ccd890e4
response_bytes=34178
response_hash=sha256:6b71b6b7829704e5890262d9cdb00cf633765acc30f8a0e6b3b9dd810a867014
reported_finish_reason=stop
first_syntax_error_original_utf8_byte=17611
```

第 17,611 个字节对应 `recovery_cases` 数组缺少闭合 `]` 后的首个语法错误。尾部另有未完成字符串。不能仅根据响应大小确定缺失发生在模型、传输还是网关，也不能用读取上限问题解释或豁免中间独立的语法错误。

E01/E02 通过临时 Go overlay 调用实际解析器完成，没有修改网关源码、调用真实模型或操作生产账号。输出摘要如下：

```text
limited=false eof_error=false done=false partial_returned=true
limited=true  eof_error=false done=false partial_returned=true
```

临时日志为 `/tmp/thoughtflow-relay-sse-eof-diagnostic.log`，仅作本机排查线索，不作为长期 CI 证据。临时测试已移除，实施时必须补入正式回归。上述复现中的测试通过表示成功观察到了缺陷，不表示网关满足收口要求。

E07/E08 同样仅使用合成 SSE 和实际解析器，无真实模型调用、生产账号操作或仓库源码修改；临时 overlay 已移除。观察结果如下，实施时须转为正式反例回归，不能将本次测试通过计为修复验收通过：

```text
malformed_final_snapshot: done=true error=nil result="AB" emitted="AB"
revised_snapshot: done=true error=nil result="AC" emitted="ABAC"
```

E07/E08 证明仅补 EOF 和读取上限判定仍不足以保证正文完整；不构成 E05 线上故障根因已确定的证据。

## 3. 修改点总表

| 编号 | 优先级 | 待收口项 | 主要修改位置 |
| --- | --- | --- | --- |
| AR-TC01 | P1 | 上游完成判定与协议事件完整性 | `internal/modules/blocks/chatgptwebupstream/internal/client/text.go`：`parseTextSSEObserved`、`finishTextResult`、事件解码、`TextResult` |
| AR-TC02 | P1 | 累计 SSE、事件、正文与累计输出上限；快照一致性 | 同文件：`StreamText` 的读取包装、scanner、正文限制及快照差量逻辑；核对下游正文拼接点 |
| AR-TC03 | P1 | 完成状态跨层传递及全部消费者适配 | upstream `pkg/events/contract.go`、upstream `biz/`、proxyapi `pkg/chatgpttext/port.go`、proxyapi `biz/chatgpt_text.go`、`internal/modules/application/chatgpttemporarychat/biz/biz.go` 及相关持久化／结果合同 |
| AR-TC04 | P1 | 两类 API 的 HTTP/SSE 成功与失败映射 | `internal/modules/application/proxyapi/service/proxy/chatgpt_web.go`、同目录 `chatgpt_responses.go` 及错误映射辅助代码 |
| AR-TC05 | P1 回归门禁 | 已生成或结果未知时的重试约束及消费者兼容 | proxyapi `biz/chatgpt_text.go`、临时对话 `biz/biz.go` 及现有失败分类、账号反馈链路 |
| AR-TC06 | P2 | 可关联的失败诊断与指标 | 现有 trace、interaction archive、usage、metrics 接线 |
| AR-TC07 | P2 | 长响应预算与成功率评估 | 上游读取预算、正文预算及确定性快照基准 |

P1 为本次完成性收口的阻断项。AR-TC05 主要要求保留并验证已有保护，不能据此宣称现有准备阶段有限恢复整体失效。

评审中的前两项分别并入 TC01／TC02，均为 P1 阻断项；Responses 和临时对话覆盖缺口分别并入 TC04、TC03／TC05，作为共享实现与合同修改的必需兼容性验收，不能因评审时标为 P2 而从本次验收中省略。

## 4. AR-TC01：完成判定

基线 `finishTextResult` 只检查正文、会话 ID 与 assistant message ID，没有将终止证据作为成功前置条件。

实施要求：

1. 明确当前支持的上游协议中，哪些事件代表文本生成已经结束，例如 `[DONE]` 或经验证的消息终态。存在正文或锚点只能证明已经观察到生成，不能证明完成。
2. 普通 EOF、传输错误、取消、超时、预算耗尽与已确认完成分别处理，不能最终全部落入同一个成功返回。
3. 保留上游实际终止证据；确实无法确定时，明确标记完成状态未知并返回失败。
4. 在断流前存在部分正文时，返回部分状态及错误供诊断与执行判断使用，不把部分状态发布为完整 completion。
5. 收到终止证据仍须执行原有正文与锚点校验，不能仅新增一个完成标记检查便放弃已有要求。

成功判定必须覆盖有无最终空行、终止事件后关闭连接及协议认可的其他终态，避免只修一个样例而误拒合法完成。

### 4.1 协议事件解析与错误优先级

基线 `json.Unmarshal` 失败会直接跳过事件，`content.parts` 中非字符串内容也会被忽略。终止标记只能证明上游结束，不能证明中间正文已被正确解码。

- 对支持协议列明正文快照、终止、上游错误及可忽略元数据事件的识别规则。明确合法且不携带正文的元数据可以忽略，不能因其没有 `message` 就误判失败。
- JSON 损坏的 `data` 事件、正文事件字段类型错误，以及无法证明可安全忽略的未知正文结构，必须返回稳定的协议解析／能力错误。不能静默丢弃正文、拼接部分可识别内容后继续成功；合法多模态或其他结构须按已验证的协议规则处理，不能一概当作字符串。
- 已观察到的解析、正文一致性或预算错误具有失败效力：及时终止；即使输入后续包含 `[DONE]` 或消息终态，也不能把此前失败改成成功。
- 错误返回应保留此前已接受的正文、锚点及执行证据，记录安全的失败类别和事件位置，不将损坏载荷写入普通日志。
- 正式回归同时覆盖 E07、字段类型不兼容，以及合法元数据夹在正文事件之间的成功样例，防止修复后误拒正常事件。

## 5. AR-TC02：读取预算与正文完整性

当前累计 SSE 上限为 16 MiB、单行 scanner 上限为 1 MiB、正文上限为 256 KiB。这三种限制对应不同资源，不能用最终正文长度代替累计传输量。

实施要求：

1. 替换或封装当前 `io.LimitReader` 用法，使“预算耗尽”能够与上游真正关闭连接区分。可使用显式计数和有界超读探测等方式，最终必须有可验证的边界行为。
2. 在未取得完成证据时触达预算，返回稳定的预算错误；不能表现为普通 EOF 后继续成功。
3. 最终正文超限时明确失败，禁止通过 `bounded` 截取后继续返回完整成功。此要求针对正文调用点，不要求连带修改所有元数据的长度限制。
4. 单行或多行组成的 SSE 事件需要有界；超过限制必须可观察，不得跳过关键事件后继续报告完成。
5. 继续保持内存、传输量与总时限的边界，不能用无限读取替代当前缺陷。
6. 测试精确边界：限制以内、等于限制、超过限制一个字节，以及终止证据位于边界附近的情况。

上游发送的是不断增长的完整消息快照时，累计 SSE 可以远大于最终正文。约 30–40 KiB 的最终内容不能据此认定一定不会触达 16 MiB 传输上限。

### 5.1 快照身份、差量与累计输出

基线快照不是旧正文的前缀扩展时，parser 会把整份新快照作为增量发出；proxyapi 和临时对话随后按增量追加。仅检查最终快照大小或 `Done` 无法发现 E08。

- 在正文发出前确定当前回答的会话、assistant message 与协议支持的正文通道身份。后续身份变化必须按已验证的协议规则处理；不能把不同消息或通道的文本拼成同一回答。合法的非正文事件不应改变已绑定的回答身份。
- 本次采用追加式文本输出：相同快照不重复发出，前缀扩展只发出新增后缀。快照缩短、非前缀改写或无法解释的回答身份变化，返回稳定的正文一致性错误，不追加整份新快照，也不通过最终快照覆盖来掩盖已发出的错误字节。合法协议若包含多阶段输出，须先明确支持的选择／转换规则并补入测试，否则如实拒绝不支持的形式。
- 每次发出增量前校验候选正文与累计输出预算；不得先写出再发现超限。明确累计已发出 UTF-8 字节的计数与上限，核对 owner、Executor、HTTP/SSE 和临时对话的拼接路径，避免各快照均未超限而累计输出失控。
- 成功时必须满足：累计发出的正文逐字节等于最终接受的正文，且各消费者保存或返回的正文与之相同。失败时已发出的字节保持部分输出语义，不得发布成功终态；非流式不得把该部分正文作为正常成功结果。
- 验收覆盖重复快照、前缀增长、缩短至空文本、非前缀改写、assistant message／会话／正文通道变化，以及累计输出预算边界；不能只覆盖不断增长的快照。

## 6. AR-TC03：完成状态跨层传递

基线 client `TextResult` 已有 `Done`，但非流式 owner 事件与 proxyapi 本地结果合同未完整表达这一事实。流任务的 `Done` 还可能表示任务停止，必须区分“任务结束”与“生成成功完成”。

实施链路：

```text
upstream client
  → chatgptwebupstream owner Result
    → proxyapi Biz
      → chatgpttext Executor Result
        → Chat Completions / Responses HTTP / SSE adapter
    → chatgpttemporarychat Biz
      → 临时对话消息持久化 / 拉取结果 / usage 与账号反馈
```

最少需要表达：完成状态、稳定终止原因、是否存在部分输出、是否触发限制，以及已有的上游状态和生成执行证据。具体字段名和枚举在实施时统一，不将本文中的描述直接当作已发布接口。

合同由各 owner 维护，调用方使用 owner 的 typed Result，不复制平行 DTO。不得跨 owner 直接导入私有 client 或共享可变状态；沿用现有 EventHub 和本地 Executor 端口。

### 6.1 临时对话兼容性

临时对话直接消费 `PullTextResult`，不能用 Executor 接线测试替代该分支验收。实施前检索文本 owner 结果的全部消费者，并记录适配范围；至少落实：

- 任务 `Done` 不能单独作为成功依据，部分正文与锚点按既有失败／中断合同保存，最终持久化状态和对外拉取结果应表达真实失败。
- 对生成已发送或结果未知的缺失终止证据、协议错误、快照不一致及读取超限，核对 `requiresRecovery` 和既有恢复流程，避免新增错误类别落入默认分支而允许正常续聊；明确未发送的失败保留既有安全恢复语义。
- 核对 `safeTurnError`、`CompleteTurn`、usage 和账号结果反馈的新原因映射；本地保护触发不能被记为成功或错误归因于账号鉴权。
- 核对无正文时的令牌刷新重试分支；空正文不能代替 `GenerationNotSent`。已有锚点、已发送或执行结果未知时不自动重放，规则与第 8 节一致。
- 增加 owner → 临时对话 → 持久化／对外结果的确定性回归，覆盖成功、部分输出失败、无正文但已有执行证据，以及新错误类别的恢复与反馈。

## 7. AR-TC04：HTTP/SSE 映射

本节同时适用于 Chat Completions 和 Responses。两者共用 Executor，但响应载荷、终态和错误帧独立，必须分别适配及验收。

### 7.1 非流式

- 只有生成完成已确认、原有必要校验通过时，才发布正常 completion。
- 未完成、读取超限、断流和传输结果未知，按现有错误体系返回明确错误与合适的非成功 HTTP 状态。
- `finish_reason` 必须依据实际完成状态映射，取消无条件填写 `stop` 的做法。
- 实际已知的长度终止与未知完成原因分别表达，不能凭响应大小臆造原生模型终止原因。
- 部分正文可以保留在受控诊断中，不放进正常成功的 `choices[].message.content`。

### 7.2 流式

- 响应头尚未发送时，返回正常的类型化错误响应。
- 响应头已经发送后不能修改 HTTP 状态，但应遵循现有 SSE 错误合同结束；连接已不可写时记录异常终止。
- 异常终止不能输出正常成功结束标记，不能把断开连接计入正常完成。
- 客户端应能区分明确成功结束、终态错误和缺少正常终态的断流。
- usage、metrics、trace 和账号结果反馈应记录实际失败，不能仅根据已经发送的 HTTP 200 记成功。

### 7.3 两类 API 的映射与验收范围

| 接口与模式 | 已确认完成且校验通过 | 未完成、协议／快照错误、预算失败或结果未知 |
| --- | --- | --- |
| Chat Completions 非流式 | 返回完整正文，`finish_reason` 如实映射 | 类型化错误与非成功 HTTP 状态，不返回正常成功 `choices` |
| Chat Completions 流式 | 按 Chat SSE 合同输出终态及 `[DONE]` | 写头前返回类型化 HTTP 错误；写头后在可写时输出可识别的错误帧，不输出成功终态及正常成功结束标记 |
| Responses 非流式 | 返回完整正文；`status` 及结束原因按实际终态映射 | 类型化错误与非成功 HTTP 状态，不合成 `status=completed` |
| Responses 流式 | 仅在真实成功时输出对应的正文／条目结束事件与 `response.completed` | 写头前返回类型化 HTTP 错误；写头后沿用 `response.failed` 合同，不输出 `response.completed` 或把部分正文作为成功完成发布 |

实施时核对并同步 Chat SSE 错误帧的正式合同与消费者测试，不能将当前仅关闭连接的行为视为已实现类型化错误终态。协议确认的模型长度终止须按各 API 支持的终态表达；本地读取／正文限制不能冒充原生模型长度终止。四个组合均需覆盖完成、写头前失败、适用时的部分输出后失败，以及统计／归档一致性。

## 8. AR-TC05：错误、重试与账号反馈

实施前先核对已有 `GenerationNotSent`、`hasOutput` 与两次执行预算。保留已落实的保护，只补齐新完成性错误的贯穿与回归。

- 已发送生成、观察到正文或生成锚点、存在部分输出或结果未知时，不自动重放生成。
- 读取上限、缺少终止证据和正文超限不是令牌失效证据，不触发账号刷新或立即换账号再生成。
- `retryable` 不能单独作为自动重放许可；是否允许执行仍须有明确的阶段与未发送证据。
- 明确未发送的 bootstrap／准备阶段失败，可以在既有总预算及截止时间内有限恢复。
- `conversion_unsupported` 等参数或能力错误维持独立分类，不触发账号冷却、令牌刷新或原样循环重试。
- 本地预算错误、上游访问错误和账号鉴权错误的反馈分开，避免本地保护触发被错误归因于账号健康。
- 协议解析和快照一致性错误不是令牌失效证据；保留其稳定原因，不因账号刷新或换账号掩盖失败。上述约束同时覆盖 proxyapi 与临时对话的独立重试入口。

## 9. AR-TC06／07：诊断与长响应成功率

### 9.1 最小诊断字段

沿用现有 trace、归档和 metrics，不另建平行执行台账。补充或核实：

- 客户端和网关请求 ID、实际 Provider／模型、执行阶段。
- 累计 SSE 字节、最终正文字节、触发的预算种类及上限。
- 累计已发出正文字节、协议事件位置及安全的解析／快照一致性失败原因；正文摘要按实际接受或发出的对象标明，不能混淆最终快照与累计输出。
- 是否观察到终止证据、终止事件类型、完成状态及稳定失败原因。
- 是否已有输出、生成是否已发送、错误发生在响应头之前还是之后。
- 原始失败正文的字节数与摘要；确有需要时通过既有受控私有诊断保存失败内容。

普通日志不得包含用户正文、凭据、token 或任意上游错误页。诊断保留与容量遵循现有访问控制和清理合同。没有原生 token 统计时如实标记未知或估算，不能用估算反推请求是否执行。

### 9.2 长响应评估

1. 使用合成的逐渐增长完整快照构造长响应，测量累计传输与最终正文的比例。
2. 覆盖最终正文约 30–40 KiB、累计 SSE 超过现有 16 MiB 的场景；预算内必须逐字节完整通过，超预算必须明确失败。
3. 分别测量内存、耗时与传输量，再决定是否调整上限；不以简单增大常量作为完成性缺陷已修复的证据。
4. 记录成功率评估与正确性验收的区别。错误被准确报告不等于长生成已经更容易成功。
5. 若仍需降低大规划输出失败率，ThoughtFlow 可另行设计保留完整冻结输入、统一调用预算和跨片范围核对的分阶段规划；这不属于网关偷偷拆分、改写或补全用户输出的授权。

快照改写和身份变化按第 5.1 节做正确性验收，不将其混入正常增长快照的成功率基准；预算评估同时记录最终正文和累计已发出正文，避免 E08 被最终快照长度掩盖。

## 10. 必需回归矩阵

下表定义正式验收要求；本地测试映射及最终验证结果见第 12 节。它们不替代最终提交 CI 和部署验证。

| 编号 | 场景 | 必须断言 | 关联项 |
| --- | --- | --- | --- |
| T01 | 合法终止证据与完整正文 | 原文逐字节一致、完成状态贯穿到 HTTP | TC01、TC03、TC04 |
| T02 | 已有正文、锚点，但普通 EOF 且缺少终止证据 | 返回明确失败，保留部分状态 | TC01 |
| T03 | 16 MiB 累计读取预算耗尽 | 与普通 EOF 分类不同，不产生成功 completion | TC02、TC04 |
| T04 | 正文、单行或事件大小超限 | 明确对应限制，不静默裁剪或跳过后成功 | TC02 |
| T05 | 限制以内、等于限制、超出一个字节 | 边界确定，正常终止事件不被误判 | TC01、TC02 |
| T06 | 大量完整快照，最终正文 30–40 KiB | 预算内完整通过；超预算明确失败 | TC02、TC07 |
| T07 | 输出后断流、超时、取消 | 生成次数不增加，不刷新或换账号重放 | TC01、TC05 |
| T08 | 非流式未完成结果 | 无正常成功正文，无合成成功 `stop` | TC03、TC04 |
| T09 | 流式写出部分内容后失败 | 客户端可识别异常终止，不发送正常成功结束标记 | TC04 |
| T10 | EventHub／Biz／Executor 往返 | 完成状态、错误原因、部分输出与执行证据不丢失 | TC03 |
| T11 | 明确未发送的准备阶段失败 | 原有有限恢复仍生效，总次数和截止时间不增加 | TC05 |
| T12 | 参数能力错误、本地预算错误及鉴权错误 | 分类与账号反馈正确，参数错误不原样重试 | TC05 |
| T13 | 失败诊断 | 请求可关联、字节及摘要准确、普通日志无私有正文与凭据 | TC06 |
| T14 | 并发完成、取消及失败 | 无竞态、无悬挂任务，状态及部分输出不串请求 | TC01、TC03、TC05 |
| T15 | 完整传输但模型输出非法 JSON | 网关不修补正文或宣称业务通过，客户端保持严格拒绝 | 责任边界 |
| T16 | 合法部分正文 → 损坏的最终正文事件 → `[DONE]`；正文事件字段类型不兼容 | 稳定协议失败，保留此前部分状态；终止证据不覆盖失败，无成功终态、不重放 | TC01、TC03、TC04、TC05 |
| T17 | 合法元数据夹在正文事件之间 | 安全忽略支持的非正文事件，正文及终止判定正确，不误拒合法完成 | TC01 |
| T18 | 重复／增长／缩短／非前缀改写快照 | 重复不重复发出；增长只发后缀；缩短至空及改写明确失败，E08 不再产生成功 `ABAC` | TC01、TC02、TC04 |
| T19 | 会话、assistant message 或正文通道变化 | 按已验证规则处理；不支持时失败，不混接不同回答、不发布成功终态 | TC01、TC02 |
| T20 | 累计输出预算边界与各层拼接 | 发出前校验；成功时 parser 增量、Executor、HTTP/SSE、临时对话正文逐字节一致；累计输出不绕过预算 | TC02、TC03、TC04 |
| T21 | Chat Completions／Responses × 非流式／流式 | 四个组合分别覆盖成功与失败；错误 HTTP／SSE 形态、完成原因及 usage／归档符合第 7 节，无伪造成功终态 | TC03、TC04、TC05 |
| T22 | owner → 临时对话 → 消息持久化／拉取结果 | 部分正文与锚点保留，成功／失败／恢复状态一致；新增原因不落入错误默认处理，usage 与账号反馈正确 | TC03、TC05 |
| T23 | 临时对话无正文但已有锚点、已发送或结果未知；明确未发送的准备失败 | 前者生成次数不增加；后者保留既有有限恢复；空正文不作为安全重放依据 | TC03、TC05 |

除底层 parser 测试外，至少覆盖两类 API 的真实本地 HTTP handler、非流式与流式响应，以及 client → owner → Biz → Executor 和 owner → 临时对话 → 持久化／拉取结果两条合同接线。T08／T09 按第 7.3 节分别落实到两类 API，T10／T12／T14 同时覆盖适用的直接消费者分支；不能用一个接口或 Executor 的测试代替全部消费者验收。先使用 mock transport 和临时数据验证，不以真实外部模型调用替代确定性回归。

## 11. 实施顺序与完成门禁

1. 第一批：TC01／TC02，修复终止证据、协议解析、预算耗尽和快照一致性语义，固化 E01／E02／E07／E08 的正式反例回归，同时验证合法元数据与前缀增长快照不被误拒。
2. 第二批：TC03／TC04，使完成状态和错误正确穿透两类 API 的四种响应组合，并适配临时对话的持久化、拉取与恢复状态；验证正文一致且不会发布部分成功。
3. 第三批：TC05／TC06，核对 proxyapi 与临时对话的独立重试入口、账号反馈、诊断及指标的一致性。
4. 第四批：TC07，依据长响应基准评估预算调整，分别报告正确性和成功率。
5. 最终提交运行相关 Go、race、HTTP/SSE 测试及仓库完整检查，按最终 SHA 记录命令、结果和 CI。测试未运行、环境受限和失败必须单列。
6. 同步正式设计／运行时合同，记录新增错误语义、预算行为和接口影响。
7. 核实远端二进制提交与生效配置，再在获准的数据范围和调用预算内进行线上验证；本地工作区或一次成功不能代替部署证据。

全部 P1 完成并取得同一最终提交的验证证据后，才能关闭“部分输出被当作成功”的问题。真实 ThoughtFlow 归档恢复须由客户端独立验证；不能据此扩大为模型 JSON 质量或全项目无缺陷保证。

评审新增的 T16–T23 必须进入上述门禁，其中 E07／E08 反例、两类 API 的四组合及临时对话直接消费者验收不得省略。只有终止标记测试通过，或只有 proxyapi Executor 测试通过，均不足以关闭问题。

## 12. 实施与验收记录

### 12.1 实施范围与协议决策

已完成 TC01–TC06 的实现及 TC07 的确定性预算评估，实现、回归与本文纳入同一实施提交。基线仍为 `37ecbf6acc4e42a37484c461c246941ca492cb8f`；第 2 节 E01／E02／E07／E08 描述的是基线缺陷，现已增加正式反例回归。E06 的远端提交与真实 SSE 缺口仍然存在，不能据本次实现宣布 ThoughtFlow 事故唯一根因已确认。

- 成功终止证据限定为已测试的 `[DONE]`，同时要求正文及双锚点有效。支持终止事件没有最终空行；未验证的消息终态不直接授权成功。
- 正文支持 assistant 的 `text`／纯字符串 `multimodal_text` 完整快照，通道为未指定或 `final`。未知结构、损坏事件、缩短、改写、身份变化明确失败。已接受的部分正文与证据保留，不自动修补业务 JSON。
- 保留累计 SSE 16 MiB、单行和聚合事件各 1 MiB、正文和累计追加各 256 KiB。边界最多探测一个额外字节；SSE 字节和摘要包括实际预读及探测字节。追加字节统计表示 parser 接受的增量，不能证明客户端实际收到。
- 共享 `internal/pkg/aetherrelaycompletion` 仅承载不可变证据及校验规则。owner、Executor、HTTP 适配器和临时对话分别验证成功所需证据；各层成功正文长度、摘要和累计追加一致。
- owner 将最终结果保存在流终态槽位并关闭增量队列，消费者排空队列后取得终态，避免取消或队列背压使最终失败丢失。普通流总时限上界为 15 分钟。
- 明确区分 `text_incomplete`、`text_protocol`、`text_budget` 和 `text_model_length`；已确认的 `max_tokens/length` 终止当前按严格完成失败处理，不伪装为正常 `stop`。输入校验使用 `invalid_request`、HTTP 400 和明确未发送证据，不惩罚账号。
- 凭据刷新须有明确 `GenerationNotSent` 证据，保留既有两次执行总预算；新完成性错误不触发重放或账号健康冷却。临时对话独立保存失败、部分正文与恢复状态。
- 两类 API 的非流式失败返回非 2xx；Chat 写头后失败在可写时输出错误帧，无成功结束 chunk／`[DONE]`；Responses 使用 `response.failed`，无 `response.completed`。trace 沿用原有执行台账，记录安全原因、字节、摘要与预算，不把任意上游错误正文写入普通日志。

正式合同已同步至 [ChatGPT Web 设计](design/chatgpt-web.md#文本完成性与正文预算) 与 [代理核心合同](design/proxy-core.md)。

### 12.2 收口项与自动化证据

下表中的状态均指本地实现；“本次提交”指包含本文实施记录及第 12.3 节冻结源码的提交，可通过 Git 历史定位，不代表 CI 或部署验收通过。

| 收口项 | 当前状态 | 实施提交 | 测试覆盖与证据入口 |
| --- | --- | --- | --- |
| AR-TC01 | 已实施 | 本次提交 | T01／T02／T07／T16／T17：`TestTextCompletionProtocolAndSnapshots`、`TestTextReadCancelAndEmitErrorsKeepPartial`；E07 转为失败断言 |
| AR-TC02 | 已实施，保留现有上限 | 本次提交 | T03–T06／T18–T20：`TestTextExactBudgets`、`TestTextUTF8BudgetsAndConcurrentIsolation`、`TestLongSnapshotTransportBudget`；E08 不再追加为成功正文 |
| AR-TC03 | 已实施，覆盖两个消费者分支 | 本次提交 | T10／T14／T20／T22：`TestTextCompletionRealOwnerExecutorAndHTTP`、`TestTextOwnerConcurrentTerminalCancellationAndBackpressure`、`TestTemporaryTextCompletionPersistenceAndNoReplay` |
| AR-TC04 | 已实施，两类 API × 两种模式 | 本次提交 | T01／T08／T09／T15／T16／T18／T21：真实 HTTP 接线测试及 `TestTextCompletionBothAPIsRejectPartialAndUnknown`；验证成功正文、类型化失败以及 usage／归档／指标结果一致 |
| AR-TC05 | 已补强分类、反馈与回归 | 本次提交 | T07／T11／T12／T23：`TestTextRecoveryBeforeGenerationOnly`、`TestTextAlternativeSelectionBoundedAndAccountScoped`、`TestTextInvalidRequestKeepsUnsentEvidence`、临时对话持久化回归；核对生成次数、刷新次数、恢复和账号反馈 |
| AR-TC06 | 已沿用现有 trace／usage 接线补充证据 | 本次提交 | T13：`TestTextCompletionTraceKeepsEvidenceWithoutContent`、`TestTextFailureDiagnosticsExcludePrivateBodies`；真实 HTTP 测试验证每次请求一条 usage 结果；两类 API 四种模式逐一核对 usage、归档与指标一致 |
| AR-TC07 | 已完成确定性预算评估，未调整上限 | 本次提交 | T06：`TestLongSnapshotTransportBudget`、`BenchmarkTextGrowingSnapshots`；准确失败与成功率提升分别记录，见下文 |

测试文件：

- parser：`internal/modules/blocks/chatgptwebupstream/internal/client/text_completion_test.go`。
- client → EventHub owner → Biz／Executor → 本地真实 HTTP handler，以及并发取消／背压：`internal/modules/blocks/chatgptwebupstream/biz/text_completion_integration_test.go`。仅替换上游 HTTP transport 和账号选择，两类 API 的流式／非流式均覆盖。
- API 独立证据与正文一致性：`internal/modules/application/proxyapi/service/proxy/text_completion_test.go`。
- 准备阶段有限恢复：`internal/modules/application/proxyapi/biz/chatgpt_text_recovery_test.go`。
- owner → 临时对话 → 持久化／拉取状态、恢复及反馈：`internal/modules/application/chatgpttemporarychat/biz/text_completion_test.go`。
- 安全诊断：`internal/pkg/aetherrelaytrace/text_completion_test.go` 及 API 独立回归。

T01–T23 已对应上述确定性测试及既有恢复测试，不使用真实外部模型或生产账号。传输完整但业务 JSON 非法的测试要求网关原样透传；ThoughtFlow 的实际业务校验及归档恢复仍由其自身验收。

### 12.3 最终本地验证

最终检查已通过，以下结果对应同一冻结的工作区 Go 源码。冻结的变更 Go 文件集合及内容摘要为 `c6f55c0803f88f001abf7c29235cf958697a981ad662d2324a79d6fa754573eb`（路径、NUL、文件内容、NUL，按路径排序后计算 SHA-256），用于核对测试期间源码未变化；它不是 Git 提交 SHA。

| 命令／检查 | 结果 |
| --- | --- |
| `go test -p 1 ./...` | 通过，退出码 0 |
| 相关包 `go test -race -p 1`（完整命令见下） | 通过，退出码 0 |
| `go vet -p 1 ./...` | 通过，退出码 0 |
| 仓库格式检查、新增 Go 文件格式及 `git diff --check` | 通过；冻结源码摘要匹配 |

```sh
go test -race -p 1 ./internal/modules/blocks/chatgptwebupstream/... ./internal/modules/application/chatgpttemporarychat/... ./internal/modules/application/proxyapi/biz ./internal/modules/application/proxyapi/service/proxy ./internal/modules/application/proxyapi/pkg/chatgptfail ./internal/modules/application/proxyapi/pkg/chatgpttext ./internal/pkg/aetherrelaycompletion ./internal/pkg/aetherrelaytrace
```

管理页面既有 Node 回归（`node --test web/admin/accounts.test.cjs web/admin/models.test.cjs web/admin/usage.test.cjs web/admin/modeltrace.test.cjs`）为 81 项通过、0 项失败，日志为 `/tmp/ar-text-admin-tests.log`。本次未修改前端代码。

本机最终检查清单为 `/tmp/ar-text-final-verification.json`，输出为 `/tmp/ar-text-final-all.log`、`/tmp/ar-text-final-race.log`、`/tmp/ar-text-final-vet.log`。临时文件仅供本次核对；正式证据由仓库回归和后续 CI 重现。

长快照基准命令：

```sh
go test ./internal/modules/blocks/chatgptwebupstream/internal/client -run '^$' -bench '^BenchmarkTextGrowingSnapshots$' -benchtime=3x -benchmem
```

本机一次结果为 `948555400 ns/op`、`5.65 MB/s`、`42653536 B/op`、`16256 allocs/op`，夹具构造不计入计时。最终正文为 36 KiB、累计 SSE 约 5.1 MiB；同等正文的密集快照超过 16 MiB 时明确失败。`B/op` 是单次解析的累计分配量，不是峰值常驻内存；共享主机测量不构成吞吐保证或生产成功率结论。当前未取得放宽预算的峰值内存和真实负载依据，因此保留现有上限。

### 12.4 提交、CI 与部署验收

- 本次实施提交包含实现、回归及文档；本地检查对应第 12.3 节冻结源码摘要，尚无该提交的 CI 证据。
- 尚未发布或部署，未核对远端二进制提交和生效配置；线上验证须在既有发布授权、数据范围与预算内另行执行。
- 未取得 ThoughtFlow 本次事故的真实上游 SSE，未重新调用真实模型，也未验收其业务 JSON 质量或归档恢复。

本次交付为本地代码收口及可重现回归。最终提交 CI、远端生效和客户端事故验收仍按第 11 节门禁执行。
