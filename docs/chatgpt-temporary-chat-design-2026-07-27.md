# ChatGPT Web 临时多轮对话设计（2026-07-27）

> 状态：implemented
>
> 范围：ChatGPT Web 的管理台文本多轮调试与历史会话恢复。
> 本文是实现与验收依据；不要求兼容此前不存在的临时对话实现。

## 1. 决策摘要

“临时”描述的是可控保留期，而不是浏览器会话或进程内状态。对话、消息和上游续聊锚点由服务端 DuckDB 持久化；浏览器只持有当前页面状态和 URL 中用于定位的非敏感会话 ID，不能使用 `localStorage` 或 `sessionStorage` 保存消息正文、上游会话 ID、账号 ID 或恢复元数据。

每个会话固定一个账号、一个文本模型和一条上游 ChatGPT Web conversation。后续轮次必须携带已持久化的 `conversation_id` 与上一条 assistant 的 `message_id`，而不是把完整历史重新提交给上游。

本轮明确不实现：research 模型、联网搜索、调研文档、来源引用、图片附件、工具调用或跨账号迁移会话。research 相关模型不得以普通聊天模型身份暴露给管理台或 `/v1/models`。

## 2. 目标与非目标

### 2.1 目标

1. 管理员可以创建、查看、删除和恢复历史文本会话。
2. 页面刷新、浏览器关闭后重新进入、服务重启后，已完成的会话与消息可重新加载。
3. 对同一会话的每一轮请求使用同一个可用账号和同一条上游 conversation。
4. 文本以增量方式显示，支持取消正在生成的本轮。
5. 失败、重启或网络中断时不盲目重发已经提交给上游的消息。
6. token、原始 SSE、浏览器本地持久化内容和未脱敏上游响应不会出现在 Admin 页面或 DuckDB 会话表中。

### 2.2 非目标

1. 不把当前功能表述为联网调研或深度研究能力。
2. 不尝试恢复服务中断时上游仍在运行的一条文本流；该状态只能安全地标记为待人工处理。
3. 不在本轮实现多管理员权限模型。当前 `owner_id` 从已认证 Admin principal 派生，不能由浏览器提交；现有单管理员部署中它稳定为当前管理员用户名。
4. 不保留 access token，不创建浏览器端离线副本，也不向 OpenAI 兼容代理端点暴露本功能。

## 3. 当前缺口

已有 ChatGPT Web 文本通路能够发送消息、接收 SSE、取得 `conversation_id`，也已具备开始、拉取和取消文本流的内部合同。但当前文本请求每次生成新的 `parent_message_id`，结果中不保留 assistant message ID，无法可靠续接同一个上游会话；普通 proxy 请求也会重新从账号池选择账号。

因此本设计不是在 `web/admin/index.html` 中累加消息数组，而是补齐以下服务端边界：

```text
Admin 页面
  -> Admin API（HTTP 与鉴权）
    -> chatgpttemporarychat Module（会话 owner、DuckDB、编排）
      -> chatgptaccountpool（固定账号、账号结果）
      -> chatgptwebupstream（真实 ChatGPT Web 文本会话）
```

`adminapi`、页面和 `proxyapi` 不得直接访问临时会话 Store、账号 Store 或上游 client。跨 owner 调用一律使用各 owner 的 typed EventHub 合同。

## 4. 模块与代码落点

新增 Application Module：`internal/modules/application/chatgpttemporarychat`。它拥有持久会话、恢复规则和跨组件文本编排，因此应为 Module，而不是塞入 `adminapi` 或建立一个仅转调的 Block。

| 位置 | 职责 |
| --- | --- |
| `chatgpttemporarychat/module.go` | Plugin 生命周期桥接；只保存本 Module 的 Biz 与 route-independent 生命周期状态。 |
| `chatgpttemporarychat/biz` | 内嵌 `basebiz.Base`；订阅对话合同、调用账号池/上游事件、维护流运行态与恢复状态。 |
| `chatgpttemporarychat/internal/store` | 此 owner 的 DuckDB 会话与消息读写、分页、保留期清理；不向其它 Module 导出。 |
| `chatgpttemporarychat/pkg/common` | Unit ID、状态与边界常量。 |
| `chatgpttemporarychat/pkg/events` | 对外 typed EventHub topic、command、result、view。 |
| `internal/pkg/aiproxystate` | 仅增加临时会话 owner 所需的专用 schema 与窄 CRUD 方法；不建立通用 document/KV API。 |
| `adminapi/biz` | 通过 temporarychat 事件合同实现窄 runtime 方法。 |
| `adminapi/service/admin` | 受保护 HTTP 路由、请求验证与 SSE/长轮询适配，不包含会话策略。 |
| `web/admin/index.html` | 历史列表、聊天视图、受控轮询/取消与纯文本安全渲染；不持久化会话数据。 |

入口显式导入并启用新 Module。`module.go -> biz -> store/EventHub` 保持单向依赖；HTTP service 不得持有 EventHub。

## 5. 数据权威与保留策略

### 5.1 权威归属

| 数据 | 权威 owner | 允许持久化位置 |
| --- | --- | --- |
| 账号、token、账号状态 | `chatgptaccountpool` | 账号池表；临时会话只保存 `account_id`。 |
| 上游 Web transport、SSE、上游请求体 | `chatgptwebupstream` | 不持久化到临时会话；流结束后仅投影受限结果。 |
| 临时会话、消息、续聊锚点、会话状态 | `chatgpttemporarychat` | `state.database` 的专用 DuckDB 表。 |
| 页面展示状态 | 浏览器内存 | 页面存活期；不得写 localStorage/sessionStorage。 |

### 5.2 配置

在 `chatgpt_web` 下增加正式配置，默认值如下：

```yaml
chatgpt_web:
  temporary_chat:
    enabled: true
    retention_days: 30
    max_conversations: 2000
    max_messages_per_conversation: 200
    max_message_bytes: 262144
    turn_timeout_seconds: 300
```

`retention_days` 必须为正数。清理任务只删除已到期、没有活跃流的会话；管理员主动删除不进入回收站。达到 `max_conversations` 时拒绝创建新会话并提示先删除旧记录，不能静默删除历史。

### 5.3 DuckDB schema

`aiproxystate.migrate` 增加以下专用表，所有正文使用参数化写入。字段名可随实现微调，但语义不得减少：

```sql
CREATE TABLE chatgpt_temporary_conversations (
  owner_id VARCHAR NOT NULL,
  conversation_id VARCHAR NOT NULL,
  title VARCHAR NOT NULL,
  account_id VARCHAR NOT NULL,
  model VARCHAR NOT NULL, -- 创建时请求的模型
  actual_model VARCHAR NOT NULL DEFAULT '', -- 上游最近一轮明确返回的实际模型
  thinking_effort VARCHAR NOT NULL DEFAULT '',
  system_prompt VARCHAR NOT NULL DEFAULT '',
  upstream_conversation_id VARCHAR NOT NULL DEFAULT '',
  parent_message_id VARCHAR NOT NULL DEFAULT '',
  status VARCHAR NOT NULL,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  PRIMARY KEY (owner_id, conversation_id)
);

CREATE TABLE chatgpt_temporary_messages (
  owner_id VARCHAR NOT NULL,
  conversation_id VARCHAR NOT NULL,
  sequence BIGINT NOT NULL,
  message_id VARCHAR NOT NULL,
  role VARCHAR NOT NULL,
  content VARCHAR NOT NULL,
  upstream_message_id VARCHAR NOT NULL DEFAULT '',
  actual_model VARCHAR NOT NULL DEFAULT '', -- assistant message 的上游实际模型
  status VARCHAR NOT NULL,
  error_class VARCHAR NOT NULL DEFAULT '',
  error_message VARCHAR NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  completed_at TIMESTAMP,
  PRIMARY KEY (owner_id, conversation_id, sequence)
);
```

需建立 `(owner_id, updated_at DESC)` 和 `(owner_id, conversation_id, sequence)` 索引或等价查询优化。会话标题由首条用户消息截断生成，不能额外调用模型生成标题。

`Documents` 只提供 `CreateTemporaryConversation`、`ListTemporaryConversations`、`LoadTemporaryConversation`、`AppendTemporaryMessage`、`UpdateTemporaryConversation`、`UpdateTemporaryMessage`、`DeleteTemporaryConversation`、`PurgeExpiredTemporaryConversations` 等明确方法。内部 Store 管理事务和领域校验；不得把原始 `*sql.DB`、`Documents` 或 repository 通过 EventHub 返回。

## 6. 领域状态机

### 6.1 会话状态

```text
idle --start turn--> streaming --normal completion--> idle
  |                     |              |
  |                     |              +-- terminal upstream error --> idle
  |                     +-- process stop/timeout --> recovery_required
  +-- delete/expiry --> closed
recovery_required --new conversation--> closed (旧会话仅可读)
```

- `idle`：可发送下一条用户消息。
- `streaming`：恰有一条进行中消息；同会话的第二个发送请求返回冲突。
- `recovery_required`：前一条已提交但未确认完成，不能继续在该上游分支上安全发送；会话可读取、复制或作为新会话的人工参考。
- `closed`：已删除或已到期，不再对外返回正文。

### 6.2 消息状态

用户消息在启动上游前以 `streaming` 写入；assistant 消息以空内容、`streaming` 写入。每个安全的增量可追加/覆盖 assistant 内容，流完成时与会话续聊锚点同一事务提交为 `completed`。

服务启动恢复时，任何 `streaming` 用户或 assistant 消息都标记为 `interrupted`，关联会话转为 `recovery_required`。禁止自动重发；网络中断也采用同一规则。这样不会产生重复提问或不确定的上游分支。

## 7. 上游文本续聊合同

### 7.1 必需字段

扩展 ChatGPT Web upstream client、事件合同和流结果：

| 对象 | 新增字段 |
| --- | --- |
| `TextRequest` / `CompleteTextCommand` / `StartTextCommand` | `ConversationID`、`ParentMessageID`。 |
| `TextResult` / `CompleteTextResult` | `ConversationID`、`AssistantMessageID`、可选 `ActualModel`。 |
| `TextDelta` / `PullTextResult` 的完成事件 | `ConversationID`、`AssistantMessageID`、可选 `ActualModel`、最终错误分类。 |

SSE parser 必须从 assistant message patch 解析 message ID；不能猜测或用随机 ID 代替。若 patch 的 `message.metadata.model_slug` 存在，才将其投影为 `ActualModel`；模型正文中的自述不是实际模型证据，缺失该元数据时必须保持为空。首轮消息可带 system prompt 和 user message；续聊只提交新 user message，并把已保存的上游 `conversation_id` 与上一条 assistant `message_id` 传入 ChatGPT Web payload。

在 fixture 中必须断言第二轮请求包含既有 `conversation_id`、既有 `parent_message_id` 和唯一的新用户消息。实现完成后还需要使用非生产账号做首轮→续聊 live 冒烟验证；只记录脱敏账号 ID、状态和结论。

### 7.2 固定账号

新增账号池合同：

```go
TopicAcquireTextAccount
AcquireTextAccountCommand{AccountID, Model, Operation}
AcquireTextAccountResult{AccessToken, Account}

TopicRecordTextResult
RecordTextResultCommand{AccountID, Success, ErrorClass}
RecordTextResultResult{Account}
```

`AcquireTextAccount` 必须检查账号存在、未禁用/异常并支持指定模型的 `chat_completions` 操作。它不能复用图片账号获取或图片 in-flight slot。

`RecordTextResult` 由账号池 owner 根据 `account_id` 更新结果；`invalid_token` 将账号转异常，TLS、超时、限流与普通上游错误仅记录失败，不删除或误禁账号。临时会话不保存 token，也不在流结束后依赖 token 执行状态转换。

账号不存在、失效或失去模型能力时，会话保持历史可读，但不能继续。页面提示“原账号不可用，请新建会话”，不得自动换号。

## 8. EventHub 合同

临时会话 Module 在 `pkg/events` 定义以下 topic 与具体 DTO。全部字段为有界值类型；禁止 `map[string]any`、JSON 字符串桥、channel、HTTP writer、repository 或 token。

| Topic | Command | Result | 语义 |
| --- | --- | --- | --- |
| `create` | `CreateConversationCommand` | `ConversationResult` | 创建会话，选择并固定账号。 |
| `list` | `ListConversationsCommand` | `ListConversationsResult` | 按 owner 分页读取历史摘要。 |
| `get` | `GetConversationCommand` | `ConversationDetailResult` | 读取会话与有界消息页。 |
| `start_turn` | `StartTurnCommand` | `StartTurnResult` | 事务写入本轮后启动内部上游流。 |
| `pull_turn` | `PullTurnCommand` | `PullTurnResult` | 长轮询获取增量、完成或错误投影。 |
| `cancel_turn` | `CancelTurnCommand` | `CancelTurnResult` | 取消活跃上游流并结束本轮。 |
| `delete` | `DeleteConversationCommand` | `DeleteConversationResult` | 取消活跃流并永久删除历史。 |

`PullTurnResult` 只返回 `Delta`、`Done`、`Message`（完成时的有限 view）、`ErrorClass` 与 `ErrorMessage`。上游 stream ID 仅保留在 temporarychat Biz 内存运行态，绝不进入 HTTP 或数据库。

`StartTurn`、`CancelTurn` 和 `DeleteConversation` 使用同步 `Send`；上游流本身由 Module 的 `BackgroundRoutine` 执行。持久化消息变更先于后台任务提交，后台任务终态写入由同一会话串行保护，防止取消、超时和完成竞态覆盖彼此。

## 9. Admin HTTP 合同

所有路径相对于 `ADMIN_BASE`，且沿用 Admin 会话、CSRF 与 `X-AI-Proxy-Admin: 1` 写保护。会话 owner 由服务端认证 principal 推导，HTTP body 和 query 不接受 `owner_id`。

| 方法 | 路径 | 请求 | 成功响应 |
| --- | --- | --- | --- |
| `POST` | `/api/chatgpt/temporary-conversations` | `model`、可选 `thinking_effort`、`system_prompt` | `201`，会话 view。 |
| `GET` | `/api/chatgpt/temporary-conversations` | `cursor`、`limit` | 会话摘要页。 |
| `GET` | `/api/chatgpt/temporary-conversations/{id}` | 可选 `before_sequence`、`limit` | 会话详情与消息页。 |
| `POST` | `/api/chatgpt/temporary-conversations/{id}/turns` | `content` | `202`，`turn_id` 与消息 view。 |
| `GET` | `/api/chatgpt/temporary-conversations/{id}/turns/{turn_id}/events` | `timeout_ms`（250–15000） | 单个有界增量/完成事件。 |
| `POST` | `/api/chatgpt/temporary-conversations/{id}/turns/{turn_id}/cancel` | 空 body | `202`，取消后的消息 view。 |
| `DELETE` | `/api/chatgpt/temporary-conversations/{id}` | 空 body | `204`。 |

所有响应设为 `Cache-Control: no-store`。错误使用现有安全 envelope：`400` 参数错误、`401/403` 认证或写保护失败、`404` owner 范围内无会话、`409` 会话正在生成或需要恢复、`410` 已过期、`503` Module/账号不可用、`502` 上游终态失败。不得把 prompt、上游请求、token 或原始 SSE 放入错误文本。

## 10. 管理台交互

在 ChatGPT Web 下新增“临时对话”子页。布局为左侧历史会话列表、右侧消息流与输入区：

1. 首次进入从 API 读取最近历史，不依赖浏览器存储；hash 仅可包含会话 ID 用于定位。
2. 新建会话填写模型、thinking effort 和可选 system prompt。创建成功后显示固定账号的脱敏显示名、请求模型、上游实际模型与过期时间；实际模型仅在上游 SSE 明确返回时展示，否则标记为“上游未返回”。实际模型与请求模型不一致时必须明确标记“上游路由已调整”。
3. 发送时禁用同会话发送按钮，使用 events API 以有限退避轮询增量；完成、取消或错误后恢复输入。
4. 页面重新加载后重新读取会话和消息。历史滚动加载使用 `before_sequence`，不能一次加载无界正文。
5. `recovery_required` 显示最后一条 `interrupted` 消息、原因和“新建会话”操作；不显示“重试本轮”。
6. 支持复制单条/全部文本、创建新会话和删除历史。删除需二次确认；不提供浏览器端离线缓存或导出 token。
7. 回复正文先按纯文本显示；若后续启用 Markdown，必须使用严格白名单渲染，禁止内联 HTML、脚本、事件属性与不受控 URL。

模型选择器只消费已验证且允许的普通 `chat_completions` 模型。模型发现必须过滤明确的 `research` / `deep_research` / 搜索工具专用条目；未验证的研究能力默认不暴露。

## 11. 安全、隐私与可观测性

1. DuckDB 文件、`state.dir` 与备份受主机权限保护；会话正文属于管理员输入输出内容，运维文档必须明确其保留期与删除方式。
2. 会话正文不写浏览器存储、URL、日志、指标 label、错误 envelope 或 interaction archive。配置 `archive_full_content: false` 不替代本 Module 的持久化边界。
3. 所有动态页面内容使用现有转义函数；不得把上游文本直接拼入可执行 `innerHTML`。
4. 日志最多记录截断会话 ID、脱敏账号 ID、模型、turn ID、耗时和错误分类。指标不以会话 ID 或正文为 label。
5. 管理员删除会话会先取消运行态 stream，再在同一 owner 范围内删除消息与会话；取消失败不得阻塞本地删除，但必须记录安全错误分类。
6. 过期清理、服务 shutdown 和 Module teardown 需要取消活跃流并将未完成消息持久化为 `interrupted`。

## 12. 实施顺序

1. **上游续聊基础**：扩展 client、SSE parser、upstream 事件与 fixture，获得可靠的 conversation/message ID 续接。
2. **账号固定合同**：新增按 ID 获取文本账号与文本结果回写，补账号池测试。
3. **持久化 owner**：增加 DuckDB schema、Documents 窄方法、temporarychat Store、恢复状态机与保留期清理。
4. **Module 接线**：新增 Module、EventHub 合同、入口显式装配、Admin Biz runtime 方法。
5. **Admin HTTP 与页面**：增加受保护 API、历史列表、消息页、轮询与取消；页面不使用 Web Storage。
6. **模型目录收口**：过滤 research 专用项，保证临时对话与 `/v1/models` 不虚假暴露 research 能力。
7. **文档与验证**：刷新 operations/configuration/Admin 设计文档，完成回归和受控 live 验证。

## 13. 验收标准

### 功能

1. 创建后的首轮和第二轮使用相同 `account_id`，第二轮上游 payload 使用保存的 `conversation_id` 和 `parent_message_id`。
2. 刷新页面、重新打开浏览器或重启服务后，已完成会话和消息仍可加载；重启后可在原账号可用时继续发送新轮次。
3. 同会话并发发送返回 `409`；取消、正常完成和失败均能释放会话以供下一步处理。
4. 上游或进程中断留下的流被持久化为 `interrupted` / `recovery_required`，不会自动重发。
5. 账号失效后历史仍可读，但继续发送被拒绝且不会切换账号。
6. 删除与过期清理后会话、消息和运行态 stream 均不可再读取。

### 安全

1. 浏览器 localStorage、sessionStorage、URL、DOM 属性、日志与指标中不存在 token、会话正文、上游 conversation ID 或 parent message ID。
2. 所有 HTTP 写操作要求现有 Admin mutation 保护；跨 owner 的 ID 猜测返回 `404`。
3. 页面渲染用户/上游文本不会执行 HTML 或脚本。
4. research 模型或研究工具专用条目不进入临时对话模型选择器或公开模型目录。

### 测试与验证

- Store：创建、分页、重启加载、原子完成、删除、保留期清理、streaming 恢复为 interrupted。
- Upstream client：首轮与续聊 payload、assistant message ID 解析、SSE 最终元数据、取消。
- Biz：固定账号、失效账号、单会话串行、取消/完成竞态、invalid token 与 transient failure 的账号结果。
- Admin HTTP：认证/CSRF、参数上限、404/409/410/503/502、安全错误、no-store 与正文不泄露。
- 浏览器：历史加载、刷新恢复、流增量、取消、删除、无 Web Storage 写入。
- 全量：`scripts/check-format.sh`、`GOCACHE=/tmp/ai-proxy-gocache go test ./... -count=1`、`make build`。

## 14. 后续文档收口

实现完成后，更新：

- `docs/operations.md`：会话正文保留期、删除、备份和恢复限制；
- `docs/configuration.md`：`chatgpt_web.temporary_chat` 配置；
- `docs/chatgpt-web-admin-closure-design-2026-07-26.md`：增加临时对话页、API 映射与安全不变量；
- 本文：将状态改为 `implemented`，记录最终 API、schema 和验证结果。

## 15. 实现记录（2026-07-27）

- Module：`internal/modules/application/chatgpttemporarychat`（EventHub 合同 + DuckDB Store + 流运行态）。
- 上游续聊：`TextRequest`/`CompleteText*`/`StartText*`/`PullTextResult` 携带 `ConversationID`/`ParentMessageID`/`AssistantMessageID`；SSE 解析 assistant message id 与可选 `message.metadata.model_slug`，后者作为实际模型投影。
- 账号固定：`AcquireTextAccount` / `RecordTextResult`（不占用图片 in-flight slot；`invalid_token` 转异常）。
- Admin HTTP：`/api/chatgpt/temporary-conversations/**`；owner 由 Admin 会话 principal 派生。
- 管理页：ChatGPT Web →「临时对话」；hash 仅定位会话 ID；会话头区分请求模型与上游实际模型，后者缺失时明确显示“上游未返回”；不写 Web Storage。
- 模型目录：research / deep_research 专用项在 upstream models 投影阶段过滤。
- 配置：`chatgpt_web.temporary_chat.*`（默认启用、30 天保留、容量上限）。
- 收口：每轮启动、终态消息和续聊锚点使用 DuckDB 事务提交；TLS、超时和未知上游中断统一转为 `interrupted` / `recovery_required`，取消保持 `cancelled`；`turn_timeout_seconds` 约束完整上游流。
- Shutdown：Module teardown 先取消活跃流并等待流 worker 持久化终态，再关闭 DuckDB；过期会话在读取与列表投影阶段立即拒绝，不等待定时清理。
