# Codex 号池管理端上游账号能力检测

## 目标与范围

在统一账号池和 Codex 账号管理页，为指定 Codex 槽位提供手工触发的 Mid-turn Steering 检测，展示该上游账号在指定模型和探测配置下的认证、模型访问、WebSocket 与 Steering 观测结果。

本功能仅属于管理面。浏览器通过现有 Admin 会话启动检测和查询结果，实际探测由服务端在独立上游 WebSocket 上执行。结果不表示下游客户端或 AetherRelay 数据面支持 Steering。

不新增下游接口，不扩展 `/v1/responses`、`/v1/messages` 或 `/v1/chat/completions` 的行为，也不向下游开放 `response.steer`。不接入正常 Agent、工具调度或生成响应流，不以此检测模型权重真实性或判定“降智”。

参考协议：

- https://developers.openai.com/api/docs/guides/steering
- https://developers.openai.com/api/docs/guides/websocket-mode
- https://developers.openai.com/api/reference/resources/responses/websocket-events

参考源码：[openai/codex rust-v0.163.0](https://github.com/openai/codex/tree/83086188ae0614bd882818ff1aa23f3194962012)，核对 commit 为 `83086188ae0614bd882818ff1aa23f3194962012`。官方客户端的 turn/steer 是本地会话操作，普通模式将更新入队、排空父响应后走普通 response.create；不能把它等同于上游 response.steer。公共 API 文档与源码核对均不能代替 Codex OAuth 上游的真实验证。

当前版本 `17.7.0` 先检测原生 response.steer，无法完成时切换一次官方客户端式调用。只给出实际检测路径的一份最终能力结论，并明确 invocation_mode / capability_target；客户端式成功不宣称原生 response.steer 支持。

## 当前架构与接线

- Admin Handler/Biz 负责管理认证、请求校验和安全结果投影，通过 typed EventHub 合同调用编排能力。
- 沿用现有模型发现、用量刷新的边界，由 `proxyapi/biz` 编排任务；Admin 不直接访问账号存储或调用 codexupstream。
- `codexaccountpool` owner 负责指定账号准入、租约、凭据刷新和本地模型策略。凭据只在服务内部受控合同中传递，不进入管理响应。
- `codexupstream` owner 执行专用 ProbeEngine，复用实际端点、账号代理、TLS 和身份配置，负责连接、读写、取消及排空。探测不复用普通生成的逐轮状态机和单响应终态处理。
- EventCorrelator / ResultClassifier 为独立纯协议逻辑，方便迁移；本期不新增独立运行进程或读取运行中 DuckDB 的外部工具。

调用链：Admin 页面 → Admin Handler/Biz → proxyapi 管理探测编排 → 账号 owner 的指定槽位租约 → codexupstream 独立探测连接 → 管理结果投影。

现有 `internal/services/probe` 只覆盖普通 Provider 的 YAML 配置与 HTTP direct endpoint，不能直接代表运行中的 Codex 号池。现有下游 WS 入口为逐轮串行 create，本功能不修改该入口。

## 管理端交互与 API

1. 在统一账号池的 Codex 槽位及 Codex 账号页提供“检测上游能力”。同邮箱的不同槽位按槽位 ID 区分。
2. 管理员选择该槽位已发现的完整模型 ID；页面说明检测会产生真实上游请求并可能消耗额度。只允许手工触发，不随列表加载、模型同步或定时刷新发起。
3. 提交后显示任务 ID、进度、槽位和模型；同槽位同时最多一个任务，重复点击不重复生成。
4. 同一任务先执行原生检测，无法取得能力结论且具备继续条件时切换一次客户端式调用。展示实际检测方式及一份阶段、能力、执行结果、错误、时序和正式用量报告；发生切换时附原生尝试诊断，JSON 同步保存。
5. 初始本地准入拒绝时立即结束，页面显示“本地准入拒绝，未请求上游”及具体原因；租约复查拒绝且此前已刷新凭据时，明确显示已刷新但未发起 WS 探测，不继续排队等待恢复。未执行、超时或断线同样展示实际阶段与原因，不直接显示为“不支持”或“账号正常”。ChatGPT Web 槽位显示不适用，不影响同一账号下 Codex 槽位的结果。

管理 API 合同（已实现，路径相对现有 Admin base）：

- `POST /api/codex/accounts/{slot_id}/steering-probes`，正文仅包含指定 `model`，使用 `Idempotency-Key` 请求头，首次接收返回 `202` 与任务 ID。
- `GET /api/codex/accounts/{slot_id}/steering-probes/{probe_id}`，返回阶段或完整安全报告。
- `GET /api/codex/accounts/{slot_id}/steering-probes/latest`，查询该槽位尚在保留期内的最近任务；打开弹窗只查询，不自动启动检测。

页面每次明确的新检测生成一个幂等键，网络重试和重复点击沿用同一键。服务端原子登记任务；同一键、同一槽位/模型/服务端配置版本返回已有任务，结果保留期间即使任务已结束也不再次生成；同一键绑定不同参数返回 `409`。同槽位已有排队或运行任务时，其他键的新请求返回 `409`，不得把不同模型的请求复用为已有任务。队列或结果存储容量不足返回 `429`，不创建任务或发起上游请求。

已接收任务的本地准入拒绝通过终结报告表达，仍可 GET 查询。结果过期或服务重启后 GET 返回 `404`，页面提示“结果已过期或服务已重启，请重新检测”；内存存储不承诺区分这两种原因。

入口复用现有管理认证、写操作来源/CSRF 防护及正文大小限制，结果设置 `Cache-Control: no-store`。请求不能覆盖上游 URL、凭据、代理或请求头。接口只登记在管理路由，不登记为下游 API 或可路由模型能力。

## 17.6.3 诊断补充

2026-10-17 核对 x800 `dd923ab` 的任务 `33772607-80bb-4119-b6c5-818d40853d3e`：槽位 `d5e3f15b-9275-4f3a-9f5b-8cb3d1df0029` 的 `gpt-6-astra` 已获准、WS 为 101，发送 steer 后以 UNKNOWN/upstream_error 结束；旧报告未保留原始拒绝详情，无法据此确认具体原因或判定不支持。本次只补诊断，不改探测输入 profile 或能力判据，不回填旧任务。

- `create_sent`、`steer_sent` 保持“已尝试提交”的兼容语义，写操作可能已经抵达服务端；新增 `create_write_completed`、`steer_write_completed` 只在本地 WriteMessage 成功后为 true。初始 created 仅使 steering_state 进入 ready；尝试时为 writing、成功写入为 sent、写入失败为 write_failed。accepted、pending、committed 继续由上游关联事件决定，写入完成不代表接受。
- `upstream_error` 独立记录失败 `event_type`、`type`、`code`、`param`、`message`、`status` 和 `truncated`；文本分别最多 128/128/128/256/4096 字节，UTF-8 边界截断，控制字符过滤。数值 code 保留文本形式；缺失字段保持空。未知 code 按独立错误状态分类，状态也未知时仍归一化为 upstream_error，原始详情不丢失；缺少 code 时可用 error.type 分类，不能从 message 猜测不支持。握手状态独立保存在 upstream_http_status，帧内 status 不覆盖 101。错误详情只来自失败事件的错误字段或握手安全错误投影，不包含完整帧、请求头或输出正文。
- `timeline` 保存最近 64 条事件的递增 sequence、elapsed_ms、direction（send/receive/local）、type 和 state；发送记录 attempt/written/write_failed，接收记录事件类型，结束记录归一化结果。`timeline_truncated` 标记旧条目被淘汰，失败详情独立保留。诊断受既有任务容量和 30 分钟有效期约束，不进入下游归档/usage。
- 管理页区分提交尝试、写入结果、协议接受，展示错误详情及最近事件，并对上游文本做 HTML 转义。普通结束日志记录 probe_id、失败事件及有界 type/code/param/status、写入事实和事件数，不写错误 message、完整事件或生成正文。诊断快照、查询结果与上游引擎彼此隔离，不能共享可变切片或错误指针。
- 自动化覆盖状态/类型兼容、未知错误详情、关联冲突、successor 失败、失败后证据、实际 WS 顺序及清理、写入失败、截断与快照隔离、管理 API/页面展示；仍要求部署后管理员重新手工检测真实账号，不自动发起付费探测。

17.6.3 本地验证：全仓 `go test -p 1 ./... -count=1`、最终 `GOFLAGS=-p=1 bash scripts/check-codex-contract.sh`（含 race、43 项 Web 检查、vet 与凭据模式扫描）通过；`node --test web/admin/*.test.cjs` 共 58 项通过。补充的握手 504 诊断与并发快照通过定向 race。主程序构建成功，产物 `/tmp/aetherrelay-steering-diagnostics`；本轮未部署或重新触发真实探测。

## 17.6.4 明确 native turn 拒绝分类

管理员提供的两份 x800 报告（以下时间为 UTC）均已记录完整错误详情：

| 任务 | 账号/槽位 | 开始时间 | 结果 |
|---|---|---|---|
| `f9cb58e5-7419-426e-bf24-46fe044ae17f` | `athanmomiy@gmail.com` / `d5e3f15b-9275-4f3a-9f5b-8cb3d1df0029` | 2026-10-16T18:41:59Z | `unsupported_native_inflight_message` |
| `254c415a-57f2-4036-bf2c-22f7d88a4f24` | `muidea@gmail.com` / `b7f48e2b-c128-49c9-b8da-7af08566b450` | 2026-10-16T18:44:19Z | `unsupported_native_inflight_message` |

两次模型均为 `gpt-6-astra`、profile 为 `single_agent_no_tools_v1`、客户端版本为 `0.163.0`。认证、模型访问及 WS 101 成功；create/steer 本地写入完成；同连接收到 `codex.rate_limits`、`codex.response.metadata`、父 `response.created`、`response.in_progress` 和 `error`，没有 accepted/pending/successor。原报告保持 UNKNOWN/upstream_error，不回填；错误明确说明 experimental native turn lane 不接受运行中的 stateful WebSocket 消息，只建议启动独立 response.create。该建议不是 Steering 支持证据，也不能据此判断账号整体不可用或所有模型均不支持。

新增结构化错误码白名单：仅精确的 `unsupported_native_inflight_message`，且已有父 created、已尝试 steer、父响应尚未结束、未接受 steer 时，判为 `UNSUPPORTED_FOR_PROFILE`、`steering_state=failed`，`execution_outcome` 与 `error_code` 均保留该代码。只在 create 阶段、尚未提交或父响应已结束时收到该码仍为 UNKNOWN，保留具体错误；未知、近似代码或仅 message 包含拒绝文本均不能推断不支持。已有 accepted 后再收到明确拒绝也不改为不支持，只有 accepted 仍为 UNKNOWN，已有 successor 则保留 SUPPORTED；pending、完成或其他终结报告不再被后续错误覆盖。原有 `steering_not_supported` 遵循相同的已有接受证据优先规则。

管理页执行结果显示“上游当前通道不支持运行中更新”，原始错误代码、详情与时序仍可查看和下载。能力判定只作用于独立管理报告，不改变账号健康、冷却、模型黑名单或调度；不自动重连、重试、换号、回退 HTTP 或发送独立 response.create。新判定上线后需由管理员手工复测，预期在上述条件下直接结束为当前配置不支持；本地事件回放或 Mock WS 不代表已验证新版线上行为。

17.6.4 本地验收：定向协议分类、Mock WS 与管理编排回归通过；`GOFLAGS=-p=1 bash scripts/check-codex-contract.sh` 全部通过（含相关包 race、Web、全仓 vet、差异和凭据模式检查）；`node --test web/admin/*.test.cjs` 共 59 项通过。主程序构建成功，产物 `/tmp/aetherrelay-steering-native-inflight`。本轮未部署、未触发真实探测，线上新分类仍待手工验收。

## 17.7.0 原生优先、客户端式调用后备

任务 profile 为 `native_first_client_fallback_v3`。原生检测使用 single_agent_no_tools_v1 / upstream_response_steer / response_steer；仅发生切换时使用 codex_client_drain_no_tools_v2 / codex_client_steering / local_queue_drain_create。

切换规则（CP-PROBE-001/002）：

1. 首先按原生流程检测；已获关联后继 created 的 SUPPORTED（即使后继生成失败），或正式 ACCEPTED_PENDING，直接结束，不发起客户端式调用。
2. 原生已经通过认证与模型访问、父 created、初始 create 完成写入并尝试 steer，随后明确不支持或以 UNKNOWN 结束，才允许切换。包括 unsupported_native_inflight_message、steering_not_supported 以及协议错误、竞争窗口、未知错误、断线或单路径超时造成的证据不足。
3. 认证/模型拒绝、限流/额度受限、取消、配置错误、接收限额、未创建父响应或未发送 steer 不通过另一种调用方式重试。总预算到期、取消或接收总量耗尽时也不启动后备路径，保留原生结果。
4. 关闭原连接后，用同一目标账号、模型及固定参数建立独立合成 WS，执行一次客户端式调用。不在被拒绝的连接补发独立 create，不重放生产输入，不循环切换或换号。

客户端检查依据官方固定 commit 的普通、非 responses-lite 路径：

- [turn_processor.rs](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/app-server/src/request_processors/turn_processor.rs)：turn/steer 调用本地 thread.steer_turn。
- [turn_input.rs](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/turn_input.rs) 与 [input_queue.rs](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/input_queue.rs)：校验活动任务、更新本地待处理输入、通知排空。
- [turn.rs](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/turn.rs)：普通路径读取父终态，再将待处理输入纳入下一次请求；response.interrupt 只在 use_responses_lite 等条件下使用。
- [client.rs](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/client.rs)：用 previous_response_id 和增量 input 构造后续 response.create。

本检测只模拟固定的单 Agent、无工具普通模式；不冒充完整官方客户端，不探测 responses-lite、中断、自动压缩、多 Agent 或全部工具模式。没有根据模型名称启用模式，也不发送 response.interrupt。

客户端方式在父 created 后标记 input_queued=true / queued_local，读取父正式成功终态后发送一次带 previous_response_id 和合成增量 user input 的 response.create。只有续接完成写入后的关联后继 created 才判 SUPPORTED；本地排队或写入不能证明上游接受。后继若带父引用必须匹配；省略时依据唯一未决续接、单连接顺序和不同响应 ID 关联。

父失败、incomplete、取消、关联冲突或缺失终态不续接；正式输出需要 function_call_output、custom_tool_call_output 或 mcp_approval_response 时以 ACCEPTED_PENDING / waiting_for_required_input 终结，此处表示本地更新等待必要输入，不表示上游接受原生 steer。后继创建后的错误保留 SUPPORTED 并单列执行失败。

报告只返回当前/最终路径的 capability、phase、execution_outcome、错误、时序和正式用量；check_profile / capability_target / invocation_mode 表示实际检测范围。切换后根报告完全转为客户端方式，steer_sent/write_completed 为 false，input_queued/continuation_sent/write_completed 表示客户端阶段。不再返回 native_steering、client_steering 或 active_check 两套结果。

`fallback` 仅在实际切换时存在，包含 reason（native_protocol_unsupported 或 native_protocol_inconclusive）、原生阶段/执行结果/错误、父响应/Steering ID、写入状态、原生时序及正式用量，不含 capability。页面只显示最终路径的能力表，原生尝试诊断可展开；导出保留全部诊断。done 仅表示整个任务结束，切换期间始终为 false。

每条路径至多 60 秒，共用任务创建起 120 秒总预算及 8 MiB 接收限额，不因切换重置。原生完成时最多一次 create/steer；切换时总计最多三次 create/一次 steer，最多可能触发四段生成。后备路径失败后直接结束，不自动再切换。取消和配置变化关闭当前连接，保留已取得的提交证据及原生诊断。历史报告不回填，新版仍须管理员手工 live 验证。

## 指定账号与执行边界

1. 严格绑定目标槽位，不可用时返回该槽位的原因，禁止换号。现有 `PreferredID` 只表示优先且会回退，不能直接当作严格绑定使用。
2. 账号 owner 在任何探测出站之前，统一检查账号状态、模型黑名单、模型快照、冷却、额度与并发限制。初始准入拒绝即结束任务，不等待冷却或并发恢复，不通过刷新凭据、同步模型、清空策略或换号再次尝试，也不发送 OAuth、HTTP 或 WS 请求；不得留下账号租约。准入通过后才占用有界租约并执行后续步骤。排队任务在执行时重新准入，被拒绝时同样直接结束。
3. 使用目标账号实际生效的受信任 Codex 上游端点、代理、账号请求头和客户端身份配置。报告记录安全配置摘要，不向任意自定义端点转发凭据。
4. 使用 WS-only 模式，禁用普通链路的 HTTP 回退；握手失败报告 WS 阶段失败，不据此宣称整个账号不可用。
5. 本地准入通过后，必要的 Token 刷新通过已有 owner 完成，刷新失败结束本次任务。已发送 create 或 steer 后执行结果不明时，不自动重连重放、不换号、不重发已接受输入。
6. 使用独立合成会话，不携带生产历史、工具结果或 Turn-State，不污染生产粘性与续轮缓存。
7. 探测结论不修改健康状态。成功、失败、不支持、待输入或未知均只写入独立检测报告，不增加或清零账号普通成功/失败计数，不新增、延长或清除冷却，不修改账号健康、启停、模型目录、传输能力快照或黑名单，也不写入下游成功率、延迟 SLO、TPS 或 Provider 熔断统计。不得把探测结果传入普通生成的 RecordResult、RecordTransportCapability 等写回入口；即使探测明确收到认证错误或限流，也只在报告中展示，不以探测结论触发健康变更。

上述健康隔离约束的是探测结论写回，不替换已有凭据生命周期：准入通过后确有必要的 Token 刷新，以及真实上游额度头合并，仍由既有 owner 按原语义处理，可能更新凭据、刷新状态或额度及其派生状态。报告单列 `credential_refresh_performed` 和安全刷新结果，不把这类 owner 操作伪装成“检测结论修改健康”，也不承诺账号记录完全只读。后台独立刷新造成的状态变化不得归因为探测结论。

## 探测请求与事件状态机

原生路径使用固定配置 `single_agent_no_tools_v1`，必要时切换到 `codex_client_drain_no_tools_v2`；两条路径均为：真实生成、`store=false`、合成文本输入、不声明客户端工具或 hosted tools、不绑定 conversation、不启用自动压缩或多 Agent。不使用 `generate=false` 预热事件作为生成证据。管理员只能选择模型，不能从浏览器覆盖探测参数。

该版本固定 instructions 为 `This is a synthetic protocol capability probe. Follow the user request.`，初始 input 为 `Write a numbered list of the integers from 1 to 100, one per line.`，steer input 为 `Stop the list. Reply with exactly STEERING_PROBE_OK.`。必要的 create 参数、合成内容及其版本由服务端统一维护，不额外覆盖推理设置；改变这些条件须发布新的 profile 版本。是否输出 `STEERING_PROBE_OK` 只作为独立内容观察，不作为协议支持的判据。

create 将初始 input 编码为 `[{"role":"user","content":[{"type":"input_text","text":"…"}]}]`，复用身份加工并保持握手与请求体的合成会话身份一致，包括指纹收敛关闭的模式。steer 的 input 为字符串，使用独立编码器。

模型由管理员明确选择，不自动替换。报告保存 `probe_profile` 版本和实际请求参数，使结论限定为“账号 × 模型 × 配置 × 时间”。遇到参数错误不静默删参、修改推理设置或重试；按配置错误记录，能力保持 `UNKNOWN`。上游响应模型不同则单列观测，不当作权重或后端身份认证。

Steering 使用专用 typed 结构和编码器，实际出站 JSON 恰好包含 `type`、`previous_response_id`、`input` 三个字段。仅复用连接、代理、TLS 和身份配置，不调用普通 create 的请求体加工器，避免自动注入 `client_metadata` 等字段。create 与 steer 不共用普通生成的逐轮统计和终态状态机。

官方文档目前将 Steering 限定于支持的 GPT-6 模型和执行模式，不按名称前缀猜测实际支持。`steering_not_supported` 与有效 Steering 提交阶段的 `unsupported_native_inflight_message` 只证明此次模型、通道或配置不兼容，固定探测配置不代表所有生产参数组合。

原生路径事件处理（切换后的客户端方式按 17.7.0 的排队、排空和续接流程处理）：

1. 建立独立上游 WS，发送最小合法 `response.create`。
2. 初始 `response.created` 后立即在同一连接发送一次 `response.steer`，只包含 `type`、初始 ID 对应的 `previous_response_id` 和 `input`，不附加 model、stream_id、工具或其它 create 参数。
3. `response.steer.accepted` 只表示输入入队，记录 `steer.id` 和父 ID，不判定已提交。
4. 只有关联 successor 的 `response.created` 才确认 `SUPPORTED`，随后单独观测 successor 正式终态。采用单连接、单初始响应、单 steer 约束，结合父 ID、顺序和协议实际提供的字段关联；不能把任意第二个 created 当成功，也不能要求 successor 携带协议未保证提供的 Steering ID。
5. 原始 `response.incomplete(reason=steered)` 是合法中断；原始正常 completed 后仍可能产生 successor，不提前结束。明确的 `response_already_completed` 或 `response_not_active` 记录为竞争窗口，不判定模型不支持。
6. 收到关联的 `response.steer.pending` 后立即以 `ACCEPTED_PENDING`、`execution_outcome=waiting_for_required_input` 结束本次检测并关闭连接。本探测不执行真实工具、不伪造结果、不重发 accepted 输入，保留 required_input 安全摘要，不宣称已提交，也不继续等待到超时。
7. accepted 前的 failed 可能没有 `steer.id`，按同连接唯一未决提交、父 ID 和事件关联；accepted 后的 failed 应匹配既有 ID。无关或矛盾事件不能提升结论。
8. 附加事件不作为完成、输出或 usage；内部新增字段不应中断探测。无法解释的未知业务事件在提交前报告协议结果未知，不伪造通过；提交后保留已有 `SUPPORTED` 证据，单独记录执行阶段的协议错误。
9. 仅正式响应 usage 用于记录探测消耗，原始响应与 successor 分别统计；缺失保持未知，不用 timing token 补账。

## 分类与报告

| 能力状态 | 证据 |
|---|---|
| `SUPPORTED` | 已观察到关联 successor created；后续执行仍可能失败 |
| `UNSUPPORTED_FOR_PROFILE` | 未获 accepted 时，上游明确返回 steering_not_supported，或在有效提交阶段返回 unsupported_native_inflight_message；仅限本次配置 |
| `ACCEPTED_PENDING` | 已接受，仍等待 required input，尚未提交；本次检测已终结 |
| `UNKNOWN` | 认证、准入、参数、网络、超时或竞争窗口导致证据不足 |
| `NOT_APPLICABLE` | 所选槽位类型不在本次范围；不能只因 WS 不通而使用 |

另行返回 `admission_state`、`auth_state`、`model_access_state`、`websocket_state`、`phase`、`execution_outcome` 和安全错误代码；未到达的阶段为 `not_observed`。101 只证明握手成功，accepted 只证明入队。真实 `response.created` 可以证明本次生成已获准，不能证明模型权重或后端身份。

错误按实际发生阶段与上游结构化错误码分类：明确的凭据错误、模型访问拒绝、`429` 限流、网络/`5xx`、参数不兼容分别记录，不把所有 `403` 都归为 Token 无效，也不把这些失败归为 Steering 不支持。只有符合阶段和证据约束的明确 `steering_not_supported` 或 `unsupported_native_inflight_message` 可得出 `UNSUPPORTED_FOR_PROFILE`（详见 17.6.4）；其余缺少充分协议证据的情况保持 `UNKNOWN`，且均不以探测结论回写健康状态。

初始准入（提交预检或执行时获取租约）拒绝的终结报告固定为：`admission_state=rejected`、`phase=admission`、`capability=UNKNOWN`、`execution_outcome=not_started`、`upstream_request_sent=false`。保存目标槽位及 owner 的拒绝原因；实际执行槽位为空，`auth_state`、`model_access_state`、`websocket_state` 均为 `not_observed`，Steering/响应 ID 与用量均未取得。该报告表示本次没有检测上游，不能用已有快照填充成本次成功观测。管理 API 已接收的异步任务仍可通过 GET 查询该终结报告，不把本地拒绝改为另一次探测。

租约获取后、必要凭据刷新后，启动 WS 前再通过 owner 复查所有准入事实。复查携已持有的 `LeaseID`，在调度锁内校验它属于规范化目标槽位，仅从并发检查中扣除自身的一个占用，不新增租约、不返回凭据。未知、已释放或跨账号租约拒绝为 `probe_lease_unavailable`；`Acquire=true` 不能与 `LeaseID` 同时使用。仍须执行最新额度、冷却和模型策略检查，不能因持有租约绕过。复查拒绝立即终结并释放原租约，不启动 WS/create/steer；报告为 `admission_state=rejected`、`phase=admission`、`execution_outcome=not_started`、`capability=UNKNOWN`，其中 not_started 指 WS 探测未启动。保留已获租约的实际槽位；若此前已执行 OAuth 刷新，保留 `credential_refresh_performed`、刷新结果及 `upstream_request_sent=true`，页面显示“本地准入复查拒绝，已执行凭据刷新，未发起 WS 探测”。配置版本变化优先终结为 `account_changed` 并标记历史观测；任务取消、预算到期和 owner 检查错误分别为 `canceled`、`timeout`、`admission_unavailable`，不冒充账号配置变化。

丢失确认、超时或断线不能证明 steer 被拒绝。只有 accepted 而没有 pending 或关联 successor created 时，最终为 `UNKNOWN`，执行结果区分提交确认超时或断线。已明确 pending 的任务按待输入终结，不再降为超时未知；已观察到关联 successor created 的任务保留 `SUPPORTED`，后续失败、超时或取消只改变执行结果。协议能力不等于响应最终完成或模型质量证明。

JSON 至少包括任务 ID、目标/实际槽位 ID、账号标签、请求/响应模型、上游端点、实际 transport、探测配置版本及参数摘要、账号执行配置版本、创建/执行开始/结束时间、分阶段观测、父/successor/Steering ID、能力、执行结果、安全错误代码及正式用量。这些是单次观测，不是全账号健康保证。

管理页将“账号当前健康状态”与“最近检测结果”分开展示。检测成功不自动恢复异常账号，检测失败或不支持不自动标记账号异常；本地拒绝也不覆盖为上游认证失败。重新授权或代理等执行配置变化后，已有报告标记为历史观测，不显示为当前配置的检测结论。账号卡片仅提供入口，打开弹窗时从服务端查询最近报告及配置历史标记，不在卡片缓存未经重新核对的能力结论。

报告不包含 Token、认证头、刷新凭据、生产内容或原始异常全文。结果只在管理面可见，不发布到 `/v1/models`，不作为下游路由依据。

## 生命周期与存储

- 任务使用现有受管理的后台生命周期，不依赖浏览器保持连接；停机取消并排空。
- 初版单槽位最多一个排队或运行任务，全局最多两个运行任务，等待队列最多 32 个；原生取得支持或待输入结论时只执行一次 create/steer；必要切换后总计最多三个 create/一个 steer，不循环重试，后备使用独立会话。排队仅等待检测执行资源，不持有账号租约，不为本地账号冷却或满载等待恢复。
- 槽位 ID 在 Proxy 的登记、幂等比较、同槽位判重和查询前，以及 owner 的并发账本查询前，统一去除首尾空白；模型 ID 仍精确匹配。空白别名不能产生第二个同槽位任务或绕过并发。
- 提交时先执行无出站、无租约的本地准入预检，拒绝即形成终结报告；执行时由 owner 原子完成严格指定槽位准入和租约获取，仍被拒绝则直接结束。预检不能代替实际执行准入。
- 总时限从服务端创建任务起计 120 秒，包含排队、准入、凭据刷新、握手和事件读取，阶段切换不延长总预算。每条路径至多 60 秒，握手上限为 10 秒与当前路径剩余预算的较小值；排队超时不请求上游。
- 探测新建的凭据刷新受任务 deadline 和 owner 停机取消约束，刷新前校验租约与执行配置版本，提交刷新结果时再次校验版本，防止旧刷新覆盖重新授权后的账号。若复用已经独立运行的 owner 刷新，只限时等待，不取消该独立生命周期。
- 解压后的单条完整 WS 消息最多 1 MiB，包括切换在内累计接收最多 8 MiB；限制作用于完整消息而非单个传输分片。输出参数不能替代时间和接收上限。
- 排队任务采用执行时账号配置并记录版本。账号删除、重新授权、代理或其他关键执行配置在运行期间变化时，取消任务、关闭连接，记录 `account_changed`，不重放；保留已取得的能力证据。owner 维护执行配置版本，正常 Token 刷新不触发该版本变化或自我取消。
- 所有退出路径关闭连接、停止读写任务、释放租约；失败后保留已获取的证据。
- 编排初始化与上游引擎分别由现有受管理后台任务执行；初始化完成后由 200 毫秒短周期控制器读取快照，不占用后台 worker 等待另一个引擎。关闭命令等待引擎真正退出，再释放租约；清理失败保持任务活跃并重试，不提前释放该槽位。控制器稍晚观察到超时或停机时，保留引擎已记录的 pending、完成或失败终态。
- 初版任务与结果保留在有界进程内存，合计最多 200 条，接收任务时为结果预留容量；已完成结果从结束时间起保留 30 分钟，幂等记录同寿命。清理已过期记录后仍满容量则拒绝新任务，不为新任务提前淘汰结果或活跃任务。页面提示有效期及重启后需重新检测，不新增数据库 schema 或修改账号池 bundle。

## 测试验收

原生优先后备验收（CP-PROBE-001..003）：原生成功、已提交后生成失败、pending、认证拒绝、取消、接收限额时零客户端调用；明确拒绝和 UNKNOWN 后只切换一次；父终态前零续接、同连接增量 input 和父引用、合成身份隔离；父失败/错序/写入失败不误判；切换后的 pending/拒绝/取消直接结束；共享预算；快照/管理查询深拷贝；API/JSON/Web 只显示一份当前结果并保留原生诊断，切换中仍轮询。下列原生与管理边界继续保留。


1. Mock WS 覆盖 accepted → steered incomplete → successor created → completed、原始 completed 后的合法 successor、只有 accepted、pending、明确不支持、accepted 前无 ID 的 failed、accepted 后同 ID 的 failed、无关/矛盾 ID、竞争窗口、参数错误、认证失败、超时和断线。断言实际出站 steer 恰好三个规定字段，无普通请求加工器注入的字段。
2. pending 立即以待输入结束，不被后续超时覆盖；只有 accepted 时超时为未知。successor 创建后失败、超时或取消，能力与执行结果分别正确；原始终态不提前关闭等待，附加事件不误作终态，缺 usage 不估算，输出是否命中固定文本不改变协议能力结论。
3. 目标槽位被禁用、异常、缺少凭据、黑名单、模型快照失效、冷却、额度受限或满载等本地准入拒绝时，立即形成 admission 阶段终结报告，不触发凭据刷新、模型同步或任何上游请求，不等待恢复、换号或遗留租约；排队后重新准入也覆盖相同边界。准入通过后刷新失败则终结于刷新阶段，不发送 create。HTTP 回退禁用，发送后断线没有重放。
4. 管理鉴权、写操作防护、跨槽位查询、同键重复提交和参数冲突、不同键的槽位占用冲突、两个运行任务及 32 个等待任务上限、结果容量预留与幂等保留期。验证 120 秒包含排队和刷新、分片与压缩不能绕过消息大小限制、完成后 30 分钟过期、重启后查询 404、关键配置变化取消而正常 Token 刷新不自我取消，以及停机和所有失败路径的资源释放。
5. 对探测成功、认证错误、限流、不支持、pending、超时和断线分别验证：探测结论不写普通账号结果、传输能力或 Provider 健康，不增加或清零成功/失败计数，不新增、延长或清除已有冷却，不改账号健康、启停、模型目录或黑名单。检测不改变生产会话和下游指标；已有凭据刷新与额度 owner 的独立操作按原语义单独验证，避免把真实凭据生命周期变化误判为探测结论写回。既有 HTTP/WS 数据面合同回归通过。
6. 页面区分未观测、待输入、配置不支持、账号准入失败与上游执行失败，不显示“降智”。
7. live 由管理员手工选择槽位和模型触发，记录 revision、任务 ID、实际账号、配置及完整协议阶段。只有 Mock、普通 WS 成功或本地测试时，不声明已验证真实上游 Steering。

## 实施输出与当前状态

输出修改文件、接线和 owner 边界、管理 API/UI 行为、实际测试结果、注明合成性质的 JSON 示例，以及目标账号 live 证据与尚未验证的限制。

2026-10-16 已完成本地实现，接线如下：

| 落点 | 实施内容 |
|---|---|
| `codexaccountpool/pkg/events/steering.go`、`biz/steering.go`、`internal/store/steering.go` | typed 指定槽位准入、原子租约与受保护的必要刷新；不经过普通请求结果写回 |
| `codexupstream/pkg/steering`、`biz/steering.go` | 固定请求、纯事件关联器、WS-only 独立引擎、大小及时间限制、真实关闭确认 |
| `proxyapi/biz/codex_steering.go`、`pkg/events/steering.go` | 幂等、队列、报告容量与过期、配置版本监测、资源释放和停机排空 |
| `adminapi/biz/codex_steering.go`、`service/admin/codex_steering.go` | 管理面路由、认证与写操作防护、严格请求校验、安全报告投影 |
| `web/admin/index.html` | 两处 Codex 槽位入口、模型选择、阶段展示、最近任务恢复及 JSON 下载 |

新增协议、Mock WS、owner、编排、Admin HTTP 与 Web 行为测试，纳入 `scripts/check-codex-contract.sh`；验收结论区分本地自动化与真实上游观测。

17.6.0/17.6.1 本地验收结果（历史记录）：

- `go test -p 1 ./... -count=1`：通过。
- `GOFLAGS=-p=1 bash scripts/check-codex-contract.sh`：通过，包含 Codex 数据面回归、受影响包 race、Web 检查、vet、差异与凭据模式扫描；随后补充的协议分类、控制器终态及准入测试另行通过 race。
- `17.6.1` 评审修复回归：账号单并发的自有租约复查通过，未知/已释放/跨账号租约被拒绝，复查只扣除自身一个占用；空白槽位别名不能绕过并发、同槽位任务、幂等及查询隔离。刷新前后的额度/冷却/租约/停机拒绝均不启动 WS，并释放租约且不写健康；刷新事实、配置变化、owner 错误、取消与预算到期分类独立，受影响 owner/Proxy 包通过最终 `-race -count=1` 复验。
- `node --test web/admin/*.test.cjs`：56 项全部通过，包含陈旧能力展示及刷新后复查拒绝文案回归。
- 最终 `go vet ./...`、主程序构建与 `git diff --check`：通过；构建产物为 `/tmp/aetherrelay-steering-build`。

以下是**历史原生合成报告节选，仅说明字段和分类，不是目标账号的 live 证据**：

```json
{
  "probe_id": "synthetic-example",
  "target_slot_id": "synthetic-slot",
  "actual_slot_id": "synthetic-slot",
  "model": "gpt-test",
  "probe_profile": "single_agent_no_tools_v1",
  "transport": "websocket",
  "state": "finished",
  "done": true,
  "admission_state": "accepted",
  "auth_state": "accepted",
  "model_access_state": "accepted",
  "websocket_state": "connected",
  "upstream_http_status": 101,
  "upstream_request_sent": true,
  "create_sent": true,
  "steer_sent": true,
  "create_write_completed": true,
  "steer_write_completed": true,
  "timeline_truncated": false,
  "phase": "successor",
  "steering_state": "committed",
  "capability": "SUPPORTED",
  "execution_outcome": "completed",
  "parent_response_id": "resp_synthetic_parent",
  "steering_id": "steer_synthetic",
  "successor_response_id": "resp_synthetic_successor",
  "credential_refresh_performed": false,
  "historical": false
}
```

17.6.0/17.6.1 的上述结果为本地验收记录。后续 x800 `dd923ab` 已由管理员执行真实探测，具体任务见本文 17.6.3 诊断补充：结果为 UNKNOWN/upstream_error，尚不能确认支持或具体拒绝原因。随后管理员提供的两次含完整诊断的真实报告已明确观测 native turn 拒绝，详见本文 17.6.4；新分类尚待部署。复测须下载完整 JSON 并结合 `Codex steering probe created/finished`、`Codex steering probe upstream error` 日志按 `probe_id` 核对实际槽位、配置、错误详情、事件顺序和消耗。真实上游若返回明确不支持、pending 或 UNKNOWN，应保留原始分类，不将 Mock 成功推广为生产能力保证。


17.7.0 新报告合成节选（不是 live 结果）：

```json
{
  "probe_profile": "native_first_client_fallback_v3",
  "check_profile": "codex_client_drain_no_tools_v2",
  "capability_target": "codex_client_steering",
  "invocation_mode": "local_queue_drain_create",
  "capability": "SUPPORTED",
  "execution_outcome": "completed",
  "input_queued": true,
  "continuation_sent": true,
  "continuation_write_completed": true,
  "steer_sent": false,
  "done": true,
  "fallback": {
    "reason": "native_protocol_unsupported",
    "phase": "steering",
    "execution_outcome": "unsupported_native_inflight_message",
    "error_code": "unsupported_native_inflight_message",
    "steer_sent": true,
    "steer_write_completed": true
  }
}
```


17.7.0 原生优先后备的本地验收：受影响的 steering、codexupstream/biz、proxyapi/biz、Admin HTTP 包全部测试通过；`GOFLAGS=-p=1 bash scripts/check-codex-contract.sh` 最终完整通过（含 race、数据面回归、全仓 vet、Web、差异和凭据模式检查）；全部 Web 测试 61 项通过；主程序构建成功，产物 `/tmp/aetherrelay-steering-fallback`。一次完整检查中既有 TestCodexCompletionBudgetIncludesAdmissionAndRetries/client_cancel 出现时序相关失败；未修改该数据面调用链，随后单独连续 25 次及最终合同重跑均通过。未部署、未触发真实上游请求，新流程仍待管理员手工 live 验证。
