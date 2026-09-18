# Codex 身份与会话语义基准

> 状态：`active`
>
> 首次核对日期：2026-09-18
>
> 适用合同：[Codex 反向代理首要维护合同](codex-proxy-maintenance-contract.md)
>
> 实现基线：AetherRelay `11.0.0` 工作树（2026-09-19）

本文是 AetherRelay 中 Codex `Installation`、`Session`、`Thread`、`X-Client-Request-Id`、`Window`、`Turn`、调度 `sessionHash` 与 Turn-State scope 的语义基准。它把真实 Codex CLI 流量观察与当前代理策略分开记录，供后续实现、评审、测试和现场排障使用。

本文是首要维护合同中 `CP-VER-006` 与 `CP-HDR-024` 的规范性说明。凡修改下列任一行为，必须在**同一提交**中同步更新本文、首要维护合同的版本/规则、对应自动化测试和实施追踪表：

- 客户端会话信号的解析优先级；
- `sessionHash`、上游 Session/Thread/Window 的派生或格式；
- `X-Client-Request-Id`、`Session_Id` 等兼容 header；
- `client_metadata` 或 `X-Codex-Turn-Metadata` 的身份字段归属；
- fingerprint `off/scoped`；
- prompt cache 或账号调度对会话身份的使用；
- Turn-State scope、记录或回填边界；
- API Key ID、模型或客户端身份对上述命名空间的影响。

只修改代码或测试而不刷新本文，视为合同未完成；只补充现场证据而不改变行为时，按 PATCH 更新合同版本与本文“现场证据”章节。

## 1. 必须区分的四层身份

“Session”不能脱离所有权和用途单独讨论。当前链路至少包含四层身份：

```text
客户端 Codex CLI
  Installation
    └── Client Session
          └── Client Thread
                └── Window
                      └── Turn
                            └── 一个或多个 HTTP / Tool 请求

Client Session Signal
  ├── sessionHash：账号亲和、LogicalConversation 与 off 上游身份
  ├── promptCacheHash：排除 routing-only 信号后的默认 prompt cache 身份
  ├── SessionScope：Turn-State 的客户端声明会话摘要
  └── Upstream Identity：本次 attempt 实际发送的 Session/Thread/Window
```

四层必须分别命名：

| 层级 | 所有者 | 主要用途 | 是否允许直接混用 |
| --- | --- | --- | --- |
| 客户端身份 | Codex CLI | 描述安装、会话、线程、窗口和 turn | 不得直接覆盖代理身份 |
| `sessionHash` / `promptCacheHash` | ProxyAPI service | 调度亲和、默认 cache key、默认上游身份 | routing-only 信号不得进入 cache key；两者都不能充当无状态请求的 Turn-State scope |
| 上游身份 | CodexUpstream attempt | 上游上下文、缓存与指纹连续性 | 每次 failover 必须按新账号重建 |
| Turn-State scope | ProxyAPI biz | 账号内按客户端声明会话记录 opaque 状态 | 不能仅按收敛后的上游 Session 分桶 |

## 2. 字段语义与所有权

### 2.1 Installation

`Installation` 表示 Codex CLI 安装实例或设备级身份，生命周期高于 Session、Thread、Window 和 Turn。

客户端语义：

- 同一个 CLI 安装可以拥有多个 Session；
- 同一个 CLI 更换 API Key 明文后 Installation 可以保持不变；
- 不同 CLI 安装通常拥有不同 Installation；
- 当前现场样本把 `installation_id` 放在 `X-Codex-Turn-Metadata` 中，没有单独发送 `X-Codex-Installation-Id`。

AetherRelay 策略：

- 客户端 Installation 不直接透传；
- fingerprint `off` 时不上送 Installation；
- `scoped` 时，从所选账号的加密随机 seed 派生账号级 Installation；
- Installation 不参与账号选择，也不改变客户端 Key 的用量归属。

因此必须区分 `Client Installation` 与 `Upstream Installation`：两者描述同类层级，但前者属于 CLI，后者属于代理选中的账号 profile。

### 2.2 Client Session

`Client Session` 是某个 CLI 中具体会话或对话的一级身份。它不等于 Installation，也不由 API Key 明文决定。

当前真实 CLI 样本始终满足：

```text
Session-Id = Thread-Id = X-Client-Request-Id
```

这只证明当前样本把三个字段折叠为同一值，不能推导协议永远没有独立 Thread。

### 2.3 sessionHash

`sessionHash` 是 AetherRelay 的确定性 UUID，不是客户端 Session 原值：

```text
sessionHash = StableUUID(KeyID + Model + Client Session Signal)
```

其用途是：

- 账号调度亲和；
- fingerprint `off` 时的默认上游 Session/Thread；
- fingerprint `scoped` 中的 LogicalConversation 投影输入。

默认 `prompt_cache_key` 使用同一命名空间规则的独立 `promptCacheHash`，但它会排除 `X-Claude-Code-Session-Id` 等 routing-only 信号，不能简单复用 `sessionHash`。客户端显式提供的 `prompt_cache_key` 保持原值。

客户端会话信号按 `CP-SCHED-002` 的实现优先级解析；body-only 情况优先使用 `client_metadata.session_id`，只有它缺失时才把 `thread_id` 作为会话兜底。显式不同的 Thread 另行形成 LogicalThread，不得让同一 LogicalConversation 因 Thread 变化而得到不同的 scoped Session。信号完全缺失时，调度可使用合成的 `default`，但该合成值不得成为 Turn-State 记录单位。

API Key 明文不得进入派生。替换同一 Key ID 槽位中的 Key 明文不会改变 `KeyID` 命名空间；创建不同 Key ID 才构成新的客户端身份命名空间。

### 2.4 Upstream Session

`Upstream Session` 是本次 attempt 实际发送给 Codex 上游的一级上下文和缓存命名空间。它由 fingerprint mode 决定，客户端原 Session 不直接透传。

| fingerprint mode | Upstream Session |
| --- | --- |
| `off` | `sessionHash` |
| `scoped` | 账号 seed 与 LogicalConversation（`sessionHash`）共同派生 |

升级 Session 派生格式会改变可观察身份与默认 cache key，必须按 `CP-VER-001` 评估 MAJOR 版本影响。

### 2.5 Thread

`Thread` 是 Session 下更细的逻辑对话分支或隔离维度。

| fingerprint mode | Upstream Thread |
| --- | --- |
| `off` | 等于 Upstream Session |
| `scoped` | 有独立 LogicalThread 时按账号、会话与 Thread 派生，否则等于 scoped Session |

当前现场流量没有观察到原生 CLI 发送 `Thread-Id != Session-Id`。因此“客户端何时创建独立 Thread”仍是未验证边界，不能凭字段名称猜测。

`Thread-Id`、`X-Client-Request-Id` 与 `client_metadata.thread_id` 用于提取显式不同的 LogicalThread；值与客户端 Session 相同时视为没有独立 Thread。LogicalThread 不改变账号调度使用的 `sessionHash`。

### 2.6 X-Client-Request-Id

尽管名称包含 Request，当前实现与全部现场样本都把它作为稳定 Thread 身份载体：

```text
X-Client-Request-Id = Thread-Id
```

它跨同一 Session 的多个 HTTP 请求复用，不是 request-per-call 随机 ID。AetherRelay 自己的诊断 `request_id` 才是单请求标识，两者不得混淆。

### 2.7 X-Codex-Window-Id 与 window_number

Window 是 Session/Thread 内的局部窗口身份：

```text
X-Codex-Window-Id = <代理控制的有效 Session/Thread>:<客户端声明的数字后缀>
```

所有权：

- 前缀由代理控制，客户端前缀不透传；
- 数字后缀优先取客户端 `X-Codex-Window-Id`；
- 其次取 `X-Codex-Turn-Metadata.window_number`；
- 都没有或无效时使用 `0`。

Window number 不参与账号选择、Session 派生或 Turn-State 分桶。最终 header、Turn Metadata 和 body `client_metadata` 必须使用同一个 Window；存在 `window_number` 属性时，它必须与最终数字后缀一致。

### 2.8 Turn

Turn 是一次逻辑交互，生命周期短于 Session，但不必等于一次 HTTP 请求。一个 Turn 可覆盖多个工具调用相关请求，一个 Session 可包含多个 Turn。

客户端优先字段：

- `turn_id`
- `root_turn_id`
- `turn_started_at_unix_ms`
- `request_kind`
- `window_number` 及其它 `CP-HDR-011` 白名单属性

客户端未声明 `root_turn_id` 时可以回落到 `turn_id`；fingerprint profile 仅在客户端未声明 turn 字段时提供 attempt 级兜底。

## 3. Turn Metadata 字段分层

### 3.1 代理所有的身份字段

- `installation_id`
- `session_id`
- `thread_id`
- `window_id`

这些字段必须从本次 attempt 的实际上游 header 重建，客户端值不得直接回灌。三个载体必须一致：

1. flat header；
2. `X-Codex-Turn-Metadata`；
3. body `client_metadata` 及其内嵌 `x-codex-turn-metadata`。

### 3.2 客户端所有的 turn 字段与属性

- `turn_id`、`root_turn_id`、`turn_started_at_unix_ms`；
- `window_number`、`context_window_id`、`request_kind`、`thread_source`；
- `sandbox`、`sandbox_mode`、`agent_name`；
- `auto_review_enabled`、`node_repl_auto_review_required`、`node_repl_disabled`。

这些字段在通过有界校验后保留。未知字段只记录字段名到 `ignored_features`，不得记录值或改变身份归属。

## 4. API Key、模型和身份命名空间

必须区分 API Key 明文和稳定 Key ID：

| 操作 | 当前身份影响 |
| --- | --- |
| 替换同一 Key ID 槽位的 Key 明文 | 不改变 `sessionHash` 的 Key 命名空间 |
| 创建不同 Key ID | 改变 `sessionHash` 命名空间 |
| 更换模型 | 改变 `sessionHash` |
| 更换客户端 Session signal | 改变 `sessionHash` |
| 更换 CLI Installation | 默认不单独改变 `sessionHash` |
| fingerprint 切换到 `scoped` | 改变实际上游 Session/Thread 归属 |

任何测试“更换 API Key 是否改变 Session”时，必须明确测试的是 Key 明文替换还是 Key ID 变化，不能把两者合并描述。

## 5. Turn-State scope

Turn-State 不能只按 Upstream Session 记录。`scoped` 虽保持 LogicalConversation 隔离，记录单位仍必须显式保留客户端声明会话维度：

```text
Turn-State Scope =
    Account
    + Fingerprint Mode
    + Fingerprint Session
    + Client-declared Conversation Digest
```

客户端声明会话的来源优先级为：

1. 显式 Session 类 header；
2. `client_metadata.session_id/thread_id` 固定字段元组；
3. 显式 `prompt_cache_key`；
4. 全部缺失时没有 scope，不记录也不回放。

`session_id="x"` 与 `thread_id="x"` 必须是不同元组，字段名是身份的一部分。调度使用的合成 `default` 绝不能成为 Turn-State bucket。

## 6. fingerprint 当前方案

### 6.1 运行时模式

| 模式 | Installation | Session | Thread | Window 前缀 |
| --- | --- | --- | --- | --- |
| `off` | 不发送 | 客户端隔离 `sessionHash` | 等于 Session | 有效 Session/Thread |
| `scoped`（默认） | 账号级 | 账号 + LogicalConversation | 账号 + LogicalConversation + 可选 LogicalThread | Thread |

HTTP、SSE、compact 和 WebSocket 握手必须使用同一份不可变 attempt snapshot。failover 到另一个账号时必须重新生成，不得残留上一账号的 Installation、Session、Thread、Window 或 Turn-State。

运行时、管理 API、账号池导入和管理页只接受 `off/scoped`。缺失或空值默认 `scoped`；加密存量中的未知值和已删除的 `device/session/full` 在加载时直接重写为 `scoped`。显式 `off` 保留为排障和快速回退开关。

### 6.2 scoped 投影

生产语义为：

| 模式 | 定位 | Installation | Session | Thread |
| --- | --- | --- | --- | --- |
| `off` | 兼容、排障和快速回退 | 不发送 | 客户端隔离身份 | 保留客户端逻辑隔离 |
| `scoped` | 默认生产收敛模式 | 账号级 | 账号 + LogicalConversation | 账号 + LogicalConversation + LogicalThread |

`scoped` 的核心不是把多个会话合并，而是把客户端身份确定性地投影为代理控制的账号域身份：

```text
Upstream Installation = UUID(accountSeed, "installation")
Upstream Session      = UUID(accountSeed, LogicalConversation, "session")
Upstream Thread       = UUID(accountSeed, LogicalConversation, LogicalThread, "thread")
Upstream Window       = Upstream Thread + ":" + ClientWindowNumber
```

对同一账号，该映射必须是单射：不同 LogicalConversation 不得得到相同的 Session/Thread 身份元组。客户端没有独立 Thread 时，Upstream Thread 等于该 scoped Session。

旧 `device/session/full` 已失效并从全部配置入口删除；实现不得根据这些字符串进入隐藏兼容分支。

### 6.3 信息守恒与账号切换

账号选择前必须构造不可变的语义胶囊：

```text
SemanticEnvelope
  ├── LogicalConversation / LogicalThread
  ├── LogicalTurn / root turn / started_at
  ├── Window Number
  ├── PromptCacheIdentity
  ├── normalized payload、input/tool 顺序
  ├── turn metadata 与 features
  └── diagnostics
```

failover 时保持该胶囊不变，只允许替换账号物理投影：

```text
AccountProjection
  ├── Authorization / Account ID
  ├── Upstream Installation
  ├── scoped Session / Thread
  ├── Window 前缀
  └── 该账号自己的 Turn-State
```

信息守恒要求：

1. 不同下游身份元组保持可区分，不能因收敛碰撞；
2. 字段缺失、显式空值、显式 `:0` 和 `window_number=0` 保持可区分；
3. 客户端合法 Turn、Window Number、显式 cache key、正文和工具顺序不得在重试中变化；
4. JSON 大整数、字符串和布尔值保持类型、精度与字节语义；
5. header、Turn Metadata、body `client_metadata` 使用同一 attempt projection；
6. 未知字段必须显式记录字段名到 ignored-features，不得静默丢失或无条件透传；
7. 信息守恒不得放宽凭据、客户端原始身份、Turn-State 和跨账号来源保护。

## 7. 2026-09-18 现场证据

现场核对使用受控交互归档，仅统计字段关系、长度、唯一值数量和载体一致性，不在本文记录原始身份值、Turn-State 或凭据。

### 7.1 work-office

前提：同一个 Codex CLI、同一个 API Key ID 的连续会话记录。

观察：

- 客户端 Session/Thread/X-Client-Request-Id 全部相等，Session 为 36 字符 UUID；
- 同一 Installation 下存在多个 Session；
- 客户端 Installation 稳定，但 fingerprint `off` 的上游不携带 Installation；
- 旧代理把客户端 Session 重建为 64 字符摘要；
- 相同客户端 Session 与模型稳定映射到相同上游 Session；
- 旧代理曾把非零 Window header 改为 `0`，同时保留 metadata 的非零 `window_number`，形成矛盾。

### 7.2 test-office

前提：另一个 Codex CLI、另一个 API Key ID，模型为 `gpt-5.6-terra`。

观察：

- 14 个成功请求、两个客户端 Session、一个 Installation；
- Installation、Session、Thread 与 work-office 均无交集；
- User-Agent profile 不同，Originator 相同；
- 新代理生成 36 字符确定性 UUID；
- 两个客户端 Session 各自稳定映射到一个上游 Session；
- Window 全部为 `0`，header 与 metadata 全部一致；
- 客户端身份未透传，turn 字段保留；一次缺失的 `root_turn_id` 由 `turn_id` 补齐。

这组样本同时改变了 CLI、Key ID 和模型，因此上游 Session 不重叠不能归因于其中单一因素。

### 7.3 work-office_new

前提：work-office 同一 CLI 替换 API Key 明文，但保留 `api_key_id=work-office`；归档包含旧阶段与新增阶段。

以 round `000160` 的上游身份形态切换为新增阶段边界：

- 新增 58 个入站请求，其中 23 个产生上游请求；
- Installation、User-Agent、Originator 跨边界保持一致；
- 有一个客户端 Session 与同一模型 `gpt-5.6-sol` 跨边界持续使用；
- 上游身份由旧 64 字符摘要切换为 36 字符 UUID，因此观察到的变化属于身份算法升级，不能归因于 Key 明文替换；
- metadata 中 Key ID 始终是 `work-office`，符合“同一槽位替换明文不改变 Key 命名空间”；
- 新阶段 15 个非零 Window `38` 与 8 个 Window `0` 均在 header 和 metadata 中保持一致；
- 客户端 Installation 继续被剥离，现场仍是 fingerprint `off`。

## 8. 已确认与未确认边界

### 8.1 已确认

1. Installation 属于 CLI 安装实例，不属于 API Key 明文。
2. 同一个 CLI 可以拥有多个 Session。
3. Client Session 可跨同一 Key ID 下的 Key 明文替换继续存在。
4. 当前 CLI 样本中 Session、Thread、X-Client-Request-Id 相等。
5. `X-Client-Request-Id` 不是单 HTTP 请求 ID。
6. 上游 Session 由代理重建，不直接透传客户端 Session。
7. `sessionHash` 由 Key ID、模型和客户端会话信号命名空间化。
8. Window 前缀归代理、数字后缀归客户端；非零值已获现场验证。
9. 客户端 Installation 在 fingerprint `off` 时不向上游透传。
10. Turn 字段与代理身份字段具有不同所有权。
11. Turn-State scope 必须独立于收敛后的 Upstream Session。

### 8.2 尚未由现场流量确认

1. 同一 CLI、Session、模型切换到不同 Key ID 后的上游 Session。
2. 同一 API Key ID 被两个不同 CLI 同时使用时的完整行为。
3. 原生 CLI 何时产生 `Thread-Id != Session-Id`。
4. 默认 `scoped` 的真实账号 canary 与上游长期行为。
5. `Session_Id` 兼容别名的必要性；三组样本均未出现。
6. 正文 `client_metadata` 与 header 的现场一致性；本次归档没有保存正文。
7. 只有 `Thread-Id`、没有 `Session-Id` 时是否应成为调度信号。

未确认项不得被实现注释或对外文档表述为现场事实；只能标记为合同决策、自动化测试覆盖或待验证假设。

## 9. 后续开发检查表

涉及本文字段的每次修改必须完成：

- [ ] 明确变更的是客户端身份、`sessionHash`、上游身份还是 Turn-State scope；
- [ ] 明确 API Key 明文与 Key ID 的影响是否不同；
- [ ] 覆盖 `off/scoped`，并确认没有重新引入 `device/session/full` 隐式兼容分支；
- [ ] 验证不同 LogicalConversation 在 scoped 投影中不会碰撞；
- [ ] 验证 failover 只替换 AccountProjection，SemanticEnvelope 保持不变；
- [ ] 覆盖 HTTP、SSE、compact、WebSocket 适用入口；
- [ ] 验证 header、Turn Metadata 与 body `client_metadata` 的一致性；
- [ ] 验证 failover 不残留上一账号身份；
- [ ] 验证无状态请求不会获得共享 Turn-State bucket；
- [ ] 同步更新本文、首要维护合同版本/规则、自动化测试与实施追踪表；
- [ ] 如使用真实归档，只记录脱敏统计和关系，不提交原始身份或凭据。
