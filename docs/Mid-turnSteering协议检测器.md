# Codex 号池管理端上游账号能力检测

## 目标与范围

在统一账号池和 Codex 账号管理页，为指定 Codex 槽位提供手工触发的 Mid-turn Steering 检测，展示该上游账号在指定模型和探测配置下的认证、模型访问、WebSocket 与 Steering 观测结果。

本功能仅属于管理面。浏览器通过现有 Admin 会话启动检测和查询结果，实际探测由服务端在独立上游 WebSocket 上执行。结果不表示下游客户端或 AetherRelay 数据面支持 Steering。

不新增下游接口，不扩展 `/v1/responses`、`/v1/messages` 或 `/v1/chat/completions` 的行为，也不向下游开放 `response.steer`。不接入正常 Agent、工具调度或生成响应流，不以此检测模型权重真实性或判定“降智”。

参考协议：

- https://developers.openai.com/api/docs/guides/steering
- https://developers.openai.com/api/docs/guides/websocket-mode
- https://developers.openai.com/api/reference/resources/responses/websocket-events

参考源码：https://github.com/openai/codex 。本期依据上述官方协议文档和仓库现有 owner 合同自行实现，未采用外部源码，不声明已核对某个 Codex 源码 commit；后续采用源码时记录具体 commit。公共 API 文档不能代替 Codex OAuth 上游的真实验证。

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
4. 结果分别展示认证、模型访问、WS 握手、Steering 提交、successor 执行结果、错误类别及检测时间，提供安全 JSON 报告。
5. 初始本地准入拒绝时立即结束，页面显示“本地准入拒绝，未请求上游”及具体原因；租约复查拒绝且此前已刷新凭据时，明确显示已刷新但未发起 WS 探测，不继续排队等待恢复。未执行、超时或断线同样展示实际阶段与原因，不直接显示为“不支持”或“账号正常”。ChatGPT Web 槽位显示不适用，不影响同一账号下 Codex 槽位的结果。

管理 API 合同（已实现，路径相对现有 Admin base）：

- `POST /api/codex/accounts/{slot_id}/steering-probes`，正文仅包含指定 `model`，使用 `Idempotency-Key` 请求头，首次接收返回 `202` 与任务 ID。
- `GET /api/codex/accounts/{slot_id}/steering-probes/{probe_id}`，返回阶段或完整安全报告。
- `GET /api/codex/accounts/{slot_id}/steering-probes/latest`，查询该槽位尚在保留期内的最近任务；打开弹窗只查询，不自动启动检测。

页面每次明确的新检测生成一个幂等键，网络重试和重复点击沿用同一键。服务端原子登记任务；同一键、同一槽位/模型/服务端配置版本返回已有任务，结果保留期间即使任务已结束也不再次生成；同一键绑定不同参数返回 `409`。同槽位已有排队或运行任务时，其他键的新请求返回 `409`，不得把不同模型的请求复用为已有任务。队列或结果存储容量不足返回 `429`，不创建任务或发起上游请求。

已接收任务的本地准入拒绝通过终结报告表达，仍可 GET 查询。结果过期或服务重启后 GET 返回 `404`，页面提示“结果已过期或服务已重启，请重新检测”；内存存储不承诺区分这两种原因。

入口复用现有管理认证、写操作来源/CSRF 防护及正文大小限制，结果设置 `Cache-Control: no-store`。请求不能覆盖上游 URL、凭据、代理或请求头。接口只登记在管理路由，不登记为下游 API 或可路由模型能力。

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

初版采用固定配置 `single_agent_no_tools_v1`：真实生成、`store=false`、合成文本输入、不声明客户端工具或 hosted tools、不绑定 conversation、不启用自动压缩或多 Agent。不使用 `generate=false` 预热事件作为生成证据。管理员只能选择模型，不能从浏览器覆盖探测参数。

该版本固定 instructions 为 `This is a synthetic protocol capability probe. Follow the user request.`，初始 input 为 `Write a numbered list of the integers from 1 to 100, one per line.`，steer input 为 `Stop the list. Reply with exactly STEERING_PROBE_OK.`。必要的 create 参数、合成内容及其版本由服务端统一维护，不额外覆盖推理设置；改变这些条件须发布新的 profile 版本。是否输出 `STEERING_PROBE_OK` 只作为独立内容观察，不作为协议支持的判据。

create 将初始 input 编码为 `[{"role":"user","content":[{"type":"input_text","text":"…"}]}]`，复用身份加工并保持握手与请求体的合成会话身份一致，包括指纹收敛关闭的模式。steer 的 input 为字符串，使用独立编码器。

模型由管理员明确选择，不自动替换。报告保存 `probe_profile` 版本和实际请求参数，使结论限定为“账号 × 模型 × 配置 × 时间”。遇到参数错误不静默删参、修改推理设置或重试；按配置错误记录，能力保持 `UNKNOWN`。上游响应模型不同则单列观测，不当作权重或后端身份认证。

Steering 使用专用 typed 结构和编码器，实际出站 JSON 恰好包含 `type`、`previous_response_id`、`input` 三个字段。仅复用连接、代理、TLS 和身份配置，不调用普通 create 的请求体加工器，避免自动注入 `client_metadata` 等字段。create 与 steer 不共用普通生成的逐轮统计和终态状态机。

官方文档目前将 Steering 限定于支持的 GPT-6 模型和执行模式，不按名称前缀猜测实际支持。`steering_not_supported` 只证明此次模型或配置不兼容，固定探测配置不代表所有生产参数组合。

事件处理：

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
| `UNSUPPORTED_FOR_PROFILE` | 上游明确返回此次配置的 steering_not_supported |
| `ACCEPTED_PENDING` | 已接受，仍等待 required input，尚未提交；本次检测已终结 |
| `UNKNOWN` | 认证、准入、参数、网络、超时或竞争窗口导致证据不足 |
| `NOT_APPLICABLE` | 所选槽位类型不在本次范围；不能只因 WS 不通而使用 |

另行返回 `admission_state`、`auth_state`、`model_access_state`、`websocket_state`、`phase`、`execution_outcome` 和安全错误代码；未到达的阶段为 `not_observed`。101 只证明握手成功，accepted 只证明入队。真实 `response.created` 可以证明本次生成已获准，不能证明模型权重或后端身份。

错误按实际发生阶段与上游结构化错误码分类：明确的凭据错误、模型访问拒绝、`429` 限流、网络/`5xx`、参数不兼容分别记录，不把所有 `403` 都归为 Token 无效，也不把这些失败归为 Steering 不支持。只有明确的 `steering_not_supported` 可得出 `UNSUPPORTED_FOR_PROFILE`；其余缺少充分协议证据的情况保持 `UNKNOWN`，且均不以探测结论回写健康状态。

初始准入（提交预检或执行时获取租约）拒绝的终结报告固定为：`admission_state=rejected`、`phase=admission`、`capability=UNKNOWN`、`execution_outcome=not_started`、`upstream_request_sent=false`。保存目标槽位及 owner 的拒绝原因；实际执行槽位为空，`auth_state`、`model_access_state`、`websocket_state` 均为 `not_observed`，Steering/响应 ID 与用量均未取得。该报告表示本次没有检测上游，不能用已有快照填充成本次成功观测。管理 API 已接收的异步任务仍可通过 GET 查询该终结报告，不把本地拒绝改为另一次探测。

租约获取后、必要凭据刷新后，启动 WS 前再通过 owner 复查所有准入事实。复查携已持有的 `LeaseID`，在调度锁内校验它属于规范化目标槽位，仅从并发检查中扣除自身的一个占用，不新增租约、不返回凭据。未知、已释放或跨账号租约拒绝为 `probe_lease_unavailable`；`Acquire=true` 不能与 `LeaseID` 同时使用。仍须执行最新额度、冷却和模型策略检查，不能因持有租约绕过。复查拒绝立即终结并释放原租约，不启动 WS/create/steer；报告为 `admission_state=rejected`、`phase=admission`、`execution_outcome=not_started`、`capability=UNKNOWN`，其中 not_started 指 WS 探测未启动。保留已获租约的实际槽位；若此前已执行 OAuth 刷新，保留 `credential_refresh_performed`、刷新结果及 `upstream_request_sent=true`，页面显示“本地准入复查拒绝，已执行凭据刷新，未发起 WS 探测”。配置版本变化优先终结为 `account_changed` 并标记历史观测；任务取消、预算到期和 owner 检查错误分别为 `canceled`、`timeout`、`admission_unavailable`，不冒充账号配置变化。

丢失确认、超时或断线不能证明 steer 被拒绝。只有 accepted 而没有 pending 或关联 successor created 时，最终为 `UNKNOWN`，执行结果区分提交确认超时或断线。已明确 pending 的任务按待输入终结，不再降为超时未知；已观察到关联 successor created 的任务保留 `SUPPORTED`，后续失败、超时或取消只改变执行结果。协议能力不等于响应最终完成或模型质量证明。

JSON 至少包括任务 ID、目标/实际槽位 ID、账号标签、请求/响应模型、上游端点、实际 transport、探测配置版本及参数摘要、账号执行配置版本、创建/执行开始/结束时间、分阶段观测、父/successor/Steering ID、能力、执行结果、安全错误代码及正式用量。这些是单次观测，不是全账号健康保证。

管理页将“账号当前健康状态”与“最近检测结果”分开展示。检测成功不自动恢复异常账号，检测失败或不支持不自动标记账号异常；本地拒绝也不覆盖为上游认证失败。重新授权或代理等执行配置变化后，已有报告标记为历史观测，不显示为当前配置的检测结论。账号卡片仅提供入口，打开弹窗时从服务端查询最近报告及配置历史标记，不在卡片缓存未经重新核对的能力结论。

报告不包含 Token、认证头、刷新凭据、生产内容或原始异常全文。结果只在管理面可见，不发布到 `/v1/models`，不作为下游路由依据。

## 生命周期与存储

- 任务使用现有受管理的后台生命周期，不依赖浏览器保持连接；停机取消并排空。
- 初版单槽位最多一个排队或运行任务，全局最多两个运行任务，等待队列最多 32 个；每个任务最多一次 create 和一次 steer，无隐式重试。排队仅等待检测执行资源，不持有账号租约，不为本地账号冷却或满载等待恢复。
- 槽位 ID 在 Proxy 的登记、幂等比较、同槽位判重和查询前，以及 owner 的并发账本查询前，统一去除首尾空白；模型 ID 仍精确匹配。空白别名不能产生第二个同槽位任务或绕过并发。
- 提交时先执行无出站、无租约的本地准入预检，拒绝即形成终结报告；执行时由 owner 原子完成严格指定槽位准入和租约获取，仍被拒绝则直接结束。预检不能代替实际执行准入。
- 总时限从服务端创建任务起计 120 秒，包含排队、准入、凭据刷新、握手和事件读取，阶段切换不延长总预算。握手上限为 10 秒与剩余预算的较小值；排队超时不请求上游。
- 探测新建的凭据刷新受任务 deadline 和 owner 停机取消约束，刷新前校验租约与执行配置版本，提交刷新结果时再次校验版本，防止旧刷新覆盖重新授权后的账号。若复用已经独立运行的 owner 刷新，只限时等待，不取消该独立生命周期。
- 解压后的单条完整 WS 消息最多 1 MiB，累计接收最多 8 MiB；限制作用于完整消息而非单个传输分片。输出参数不能替代时间和接收上限。
- 排队任务采用执行时账号配置并记录版本。账号删除、重新授权、代理或其他关键执行配置在运行期间变化时，取消任务、关闭连接，记录 `account_changed`，不重放；保留已取得的能力证据。owner 维护执行配置版本，正常 Token 刷新不触发该版本变化或自我取消。
- 所有退出路径关闭连接、停止读写任务、释放租约；失败后保留已获取的证据。
- 编排初始化与上游引擎分别由现有受管理后台任务执行；初始化完成后由 200 毫秒短周期控制器读取快照，不占用后台 worker 等待另一个引擎。关闭命令等待引擎真正退出，再释放租约；清理失败保持任务活跃并重试，不提前释放该槽位。控制器稍晚观察到超时或停机时，保留引擎已记录的 pending、完成或失败终态。
- 初版任务与结果保留在有界进程内存，合计最多 200 条，接收任务时为结果预留容量；已完成结果从结束时间起保留 30 分钟，幂等记录同寿命。清理已过期记录后仍满容量则拒绝新任务，不为新任务提前淘汰结果或活跃任务。页面提示有效期及重启后需重新检测，不新增数据库 schema 或修改账号池 bundle。

## 测试验收

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

本轮本地验收结果：

- `go test -p 1 ./... -count=1`：通过。
- `GOFLAGS=-p=1 bash scripts/check-codex-contract.sh`：通过，包含 Codex 数据面回归、受影响包 race、Web 检查、vet、差异与凭据模式扫描；随后补充的协议分类、控制器终态及准入测试另行通过 race。
- `17.6.1` 评审修复回归：账号单并发的自有租约复查通过，未知/已释放/跨账号租约被拒绝，复查只扣除自身一个占用；空白槽位别名不能绕过并发、同槽位任务、幂等及查询隔离。刷新前后的额度/冷却/租约/停机拒绝均不启动 WS，并释放租约且不写健康；刷新事实、配置变化、owner 错误、取消与预算到期分类独立，受影响 owner/Proxy 包通过最终 `-race -count=1` 复验。
- `node --test web/admin/*.test.cjs`：56 项全部通过，包含陈旧能力展示及刷新后复查拒绝文案回归。
- 最终 `go vet ./...`、主程序构建与 `git diff --check`：通过；构建产物为 `/tmp/aetherrelay-steering-build`。

以下是**合成报告节选，仅说明字段和分类，不是目标账号的 live 证据**：

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

尚未部署本轮修改，未针对真实 OAuth 账号执行 live 检测，不声明上游已支持 Steering。部署后由管理员手工检测，下载完整 JSON 并结合 `Codex steering probe created/finished` 日志按 `probe_id` 核对实际槽位、配置、阶段和消耗。真实上游若返回明确不支持、pending 或 UNKNOWN，应保留原始分类，不将 Mock 成功推广为生产能力保证。
