# 客户端 API Key、Provider 与模型联动收口设计

本文定义 AetherRelay 为客户端 API Key 绑定 Provider 访问范围，并在业务调用时按该范围发布模型目录、限制路由候选的最终设计。本文是后续代码收口的实施依据，覆盖 schema、存储、认证快照、模型目录、请求路由、Admin API、Web 管理端、测试与文档更新。

相关正式设计：

- [安全与认证设计](security.md)
- [核心代理与路由设计](proxy-core.md)
- [OpenAI Responses 与 Anthropic Messages 双向转换设计](responses-anthropic-conversion.md)

## 1. 背景与当前缺口

当前客户端 API Key 只承担入站认证、用量归属和交互归档隔离：

- `ClientIdentity` 只有稳定 `KeyID`，没有 Provider 访问范围。
- DuckDB 的 `client_api_key_metadata` 只保存摘要、启停和生命周期时间。
- `/v1/models` 始终返回全局 EffectiveCatalog 的全部模型。
- `ResolveTransportPlans` 只按全局候选、协议端点、优先级、fallback 和健康度选择 Provider。
- 因此任意已启用客户端 Key 都能看到并尝试调用所有全局模型，不能按业务隔离 Provider 或模型集合。

本次收口需要建立以下联动：

```text
客户端 API Key
    -> Provider 访问策略
        -> EffectiveCatalog 中获准的候选 Provider
            -> 当前 Key 可见的模型与能力
                -> 当前请求可用的 TransportPlan
```

## 2. 目标与非目标

### 2.1 目标

1. 管理员可在创建客户端 Key 时选择允许访问的 Provider，后续可独立修改。
2. `/v1/models` 只返回当前已认证 Key 可以调用的模型和端点能力。
3. 业务直接提交未发布模型时也必须被拒绝，不能绕过 `/v1/models`。
4. 同一模型由多个 Provider 提供时，只保留当前 Key 获准的候选，继续支持授权范围内的优先级、健康度与故障转移。
5. ChatGPT Web、Codex OAuth 等内建 Provider 与普通 Provider 使用同一授权语义。
6. Provider、模型目录或账号池状态变化后，Key 的有效模型自动联动，不要求逐 Key 重写模型列表。
7. 权限更新和认证索引热切换保持请求边界原子性；在途请求使用旧快照，新请求使用新快照。

### 2.2 非目标

- 不把客户端 API Key 转发给上游；上游仍使用 Provider 自己的 API Key 或账号池凭据。
- 不增加请求侧 `X-AI-Provider`、query provider 或 `provider/model` 选择机制。
- 不为客户端 Key 维护独立模型 allowlist；模型权限完全由 Provider 绑定和 EffectiveCatalog 派生。
- 不按瞬时健康度频繁改变 `/v1/models`；熔断和临时故障只影响执行期候选。
- 不在本阶段实现配额、限速、Token 预算或 endpoint 级 ACL。
- 不允许普通业务 Key 调用 Admin API。

## 3. 核心概念与不变量

### 3.1 Provider 访问模式

每个客户端 Key 必须显式拥有一个 Provider 访问策略：

```json
{
  "mode": "selected",
  "provider_ids": ["deepseek", "codexoauth"]
}
```

仅允许两种模式：

| mode | 语义 |
| --- | --- |
| `all` | 允许当前及未来新增的全部 Provider；`provider_ids` 必须为空 |
| `selected` | 只允许 `provider_ids` 中明确列出的 Provider；管理 API 写入时至少一个 |

禁止使用空数组隐式表达“全部”。`selected + []` 只可能由数据损坏产生，运行时按 deny-all 处理，不能回退为 `all`。

### 3.2 Provider ID

- Provider 绑定只保存规范化稳定 ID，不保存 display name、base URL、模型名或上游密钥。
- 管理型 Provider ID 使用当前配置中的小写 map key。
- 内建 Provider 使用保留 ID：`chatgptweb`、`codexoauth`。
- Provider 名不可重命名；需要改名时视为新建、调整 Key 绑定、再删除旧 Provider。

### 3.3 模型可见性

一个模型对当前 Key 可见，当且仅当 EffectiveCatalog 中至少有一个候选满足：

```text
candidate.RouteOwner 被当前 Key 的 ProviderAccess 允许
```

定义：

```text
Key 可见模型
= 全局 EffectiveCatalog 模型候选
  过滤 ProviderAccess
```

Provider 是否启用、账号池是否有可路由账号、模型是否被发现，已经体现在 EffectiveCatalog 候选中，不在 Key 记录内重复保存。

### 3.4 能力发布

`/v1/models` 中以下字段只能由获准候选汇总：

- `supported_endpoints`
- `capabilities.conversions`
- 是否存在原生或转换路径

容量和模型自身能力继续来自 exact `model_metadata`，但只有模型至少存在一个获准候选时才允许发布。禁止因为全局存在未授权候选而扩大 endpoint 或转换能力。

### 3.5 执行期错误边界

| 条件 | 返回 |
| --- | --- |
| 全局不存在模型 | `model_not_found` |
| 全局存在但当前 Key 没有任何获准候选 | `model_not_found`，不泄露该模型存在 |
| 有获准候选但客户端 endpoint 无兼容 TransportPlan | `endpoint_unsupported` 或现有转换 typed error |
| 有获准兼容候选但全部不健康 | `provider_unavailable` |
| 上游请求失败 | 现有 `upstream_unavailable` / 流式 outcome 合同 |

不得返回“模型存在但当前 Key 无权限”等可枚举其它业务目录的信息。

## 4. 最终数据模型

### 4.1 Schema

在 usage runtime 最终 schema 中扩展客户端 Key 元数据，并增加规范化关联表：

```sql
CREATE TABLE client_api_key_metadata (
    api_key_id          VARCHAR PRIMARY KEY,
    created_at          TIMESTAMPTZ NOT NULL,
    last_used_at        TIMESTAMPTZ,
    key_hash            VARCHAR,
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    last_rotated_at     TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    provider_access_mode VARCHAR NOT NULL,
    CHECK (provider_access_mode IN ('all', 'selected'))
);

CREATE TABLE client_api_key_provider_access (
    api_key_id  VARCHAR NOT NULL,
    provider_id VARCHAR NOT NULL,
    PRIMARY KEY (api_key_id, provider_id)
);

CREATE INDEX idx_client_api_key_provider_access_provider
ON client_api_key_provider_access(provider_id);
```

约束由应用事务补齐：

- `all` 不允许存在关联行。
- Admin 写入 `selected` 时至少一条关联行。
- `provider_id` 必须是写入时已知 Provider，包括已禁用 Provider 和内建 Provider。
- Provider ID 统一 trim、转小写、去重、稳定排序。

不依赖跨 owner 的 Provider 外键。Provider 目录由 configruntime/providerstore 管理，客户端 Key 及其访问策略由 usage runtime 管理。

### 4.2 Schema 基线策略

当前项目明确不要求保留历史数据兼容性。本功能直接调整最终 schema，并继续从初始版本基线开始：

- `currentSchemaVersion` 保持 `1`。
- `currentSchemaName` 固定改为 `usage_provider_access_v1`。
- schema 名不匹配时原子重建 usage runtime 自己拥有的表。
- reset 顺序先删除 `client_api_key_provider_access`，再删除 `usage_events`、`client_api_key_metadata` 和 migration 记录。
- Provider、账号池、图片、任务、搜索历史、临时对话和交互文件不属于本 schema，不得删除。

该动作会清除旧 usage 和旧客户端 Key。发布说明必须要求管理员提前备份，并在升级后重新创建客户端 Key 和访问策略。

### 4.3 Go 领域模型

新增独立小包 `internal/pkg/aetherrelayclientaccess`，避免 usage、clientauth、effectivecatalog 之间形成反向依赖：

```go
type Mode string

const (
    ModeAll      Mode = "all"
    ModeSelected Mode = "selected"
)

type Policy struct {
    Mode        Mode
    ProviderIDs []string
}
```

该包负责：

- shape 校验与规范化；
- clone，避免切片跨请求修改；
- `Allows(providerID string) bool`；
- `All()` 和 `Selected(ids)` 构造；
- 稳定排序和去重。

它不读取 config、不查询 Provider、不做 HTTP DTO 序列化。

`usage.ClientAPIKeyRecord` 增加规范化后的 `ProviderAccess clientaccess.Policy`。

## 5. 后端收口设计

### 5.1 Store 接口与 DuckDB

`usage.Store` 增加：

```go
SetClientAPIKeyProviderAccess(context.Context, string, clientaccess.Policy) error
ClientAPIKeyIDsForProvider(context.Context, string) ([]string, error)
```

现有方法调整：

- `ListClientAPIKeys` 一次读取 Key 主记录和关联表，返回完整 Policy；不得形成每 Key 一次 SQL 的 N+1 查询。
- `CreateClientAPIKey` 在同一事务插入主记录和关联行；任一步失败全部回滚。
- `SetClientAPIKeyProviderAccess` 在同一事务更新 mode、删除旧关联、写入新关联。
- `DeleteClientAPIKey` 在现有删除 usage 的事务中先删除关联行，再删除 Key 主记录。
- `RotateClientAPIKey`、启停、Touch 不修改 ProviderAccess。
- `ClientAPIKeyIDsForProvider` 只返回 `selected` 模式的显式引用，按 Key ID 排序。

MemoryStore 必须实现完全相同的语义，不能在测试中默认放宽为 `all`。

### 5.2 认证索引与请求快照

`clientauth.ClientIdentity` 扩展为：

```go
type ClientIdentity struct {
    KeyID          string
    ProviderAccess clientaccess.Policy
}
```

认证索引从 `ListClientAPIKeys` 同时构建摘要和 Policy：

- 明文 Key 和摘要处理不变。
- Policy 在进入索引前 clone。
- `ResolveHeaders` 返回包含同一代 ProviderAccess 的不可变身份。
- `WithClientIdentity` 再次 clone，避免调用方修改索引内切片。
- UpdateConfig/客户端 Key 热刷新用一次 atomic pointer 切换完整索引。
- 在途请求继续使用旧身份快照；更新完成后的新请求使用新策略。

内部 Admin 功能身份必须显式使用 `ModeAll`：

```go
ClientIdentity{
    KeyID:          "admin:" + ownerID,
    ProviderAccess: clientaccess.All(),
}
```

禁止依赖零值代表 `all`。未知或零值 Policy 一律 deny-all。

### 5.3 EffectiveCatalog 作用域 API

在 `effectivecatalog` 增加共享纯函数，不复制或修改全局快照：

```go
func (s Snapshot) CandidatesForAccess(modelID string, policy clientaccess.Policy) []Candidate
func (s Snapshot) LookupForAccess(modelID string, policy clientaccess.Policy) (Route, bool)
func (s Snapshot) SortedModelIDsForAccess(policy clientaccess.Policy) []string
func (s Snapshot) ProviderIDsForAccess(policy clientaccess.Policy) []string
```

要求：

- 返回新 slice，不暴露内部候选数组。
- 保留原候选顺序，不重新定义 priority/fallback。
- 同一模型的未授权候选必须完全消失。
- `SortedModelIDsForAccess` 只返回至少有一个获准候选的模型。
- `ModeAll` 与现有全局 API 结果一致。
- deny-all 返回空集合。

现有全局 `CandidatesFor`、`Lookup` 保留给 Admin 全局目录和内部构建，但业务请求不能再直接使用它们。

### 5.4 `/v1/models` 联动

`handleModels` 在认证完成后从 request context 读取 `ClientIdentity.ProviderAccess`，调用 scoped builder：

```go
buildModelsListResponse(snap, identity.ProviderAccess)
```

需要逐项改造：

- 模型 ID 使用 `SortedModelIDsForAccess`。
- route/capacity 使用 `LookupForAccess`。
- `modelSupportedEndpoints` 只遍历 `CandidatesForAccess`。
- `modelHasConversionDirection` 只遍历 `CandidatesForAccess`。
- reasoning/native metadata 仅在 scoped model 已入选后投影。
- 返回结构继续不包含 Provider ID、访问模式或其它业务信息。
- GET 与 POST `/v1/models` 必须完全一致。
- `/v1/models` 继续不根据瞬时 circuit/health 过滤，保持稳定目录。

### 5.5 执行请求路由联动

所有调用模型的入口最终必须通过同一个带 Policy 的 TransportPlan 解析：

```go
ResolveTransportPlansForAccess(
    cfg,
    snapshot,
    identity.ProviderAccess,
    method,
    path,
    model,
)
```

处理顺序固定为：

1. 校验 method/path/model。
2. 使用 `LookupForAccess` 判断模型是否对当前 Key 可见。
3. 使用 `CandidatesForAccess` 获取获准候选。
4. 应用 transport matrix、转换模板、priority/fallback。
5. 应用请求特性过滤，例如文件附件。
6. 应用 Provider/model 健康度和熔断。
7. 执行获准候选链。

覆盖入口：

- `/v1/models`
- `/v1/chat/completions`
- `/v1/messages`
- `/v1/responses`
- `/v1/completions`
- `/v1/embeddings`
- `/v1/images/generations`
- `/v1/images/edits`
- `/v1/search`
- Admin 内部临时对话、在线搜索和图片执行 command

禁止在各 handler 中分别实现 Provider allowlist。所有入口必须复用 scoped catalog/route helper。

### 5.6 候选、转换与 fallback 不变量

示例：模型 `shared-model` 全局候选为：

```text
provider-a priority=200 fallback=true
provider-b priority=100 fallback=true
codexoauth priority=90 fallback=true
```

Key 只绑定 `provider-b`、`codexoauth` 后，实际候选必须是：

```text
provider-b
codexoauth
```

以下行为禁止：

- `provider-b` 失败后回退到未授权的 `provider-a`。
- 因 `provider-a` 支持 `/v1/messages` 而向该 Key 发布 messages 能力。
- 因全局存在某方向 Level 3 转换而向该 Key 发布未授权转换能力。
- 在所有授权候选不健康时临时绕过权限使用健康的未授权 Provider。

### 5.7 Provider 生命周期

#### 禁用与重新启用

- 已禁用 Provider 仍可保留在 `selected` 绑定中。
- Provider 禁用后从 EffectiveCatalog 候选消失，对应模型自动从该 Key 的目录消失。
- 重新启用并恢复候选后，访问自动恢复，不改 Key 记录。

#### 删除

静态 Provider 删除前必须调用 `ClientAPIKeyIDsForProvider`：

- 无引用：允许按现有流程删除。
- 有引用：返回 HTTP 409，错误信息列出引用它的客户端 Key ID，要求管理员先修改这些 Key。

采用“拒绝删除”而不是跨 providerstore/usage store 级联，避免两个 owner 之间出现无法原子提交的部分成功。

内建 Provider 仍不可删除。Provider 删除检查与客户端 Key ProviderAccess 写入都必须复用 Admin Handler 的 `updateMu`，消除检查与写入的竞态。

#### Provider 模型或 endpoint 修改

Provider 的 models、protocol、endpoints、conversion template 或账号目录变化只触发 EffectiveCatalog 重建。Key 绑定保持不变，有效模型和能力自动重新计算。

### 5.8 Admin API 合同

Admin 的 Key 列表计数和有效模型接口必须读取 proxyapi 当前 atomic EffectiveCatalog，不能根据 Provider 表的 `models` 字段自行推导。`RuntimeConfig` 增加只读方法：

```go
EffectiveCatalogSnapshot() effectivecatalog.Snapshot
```

proxy Handler 返回当前快照副本，Admin 测试 stub 显式提供 Empty 或测试快照。ProviderAccess 写入时的“已知 Provider universe”固定为 `ConfigSnapshot().Providers` 的全部 key（含 disabled）加 `chatgptweb`、`codexoauth`；不得用当前 catalog candidates 校验，否则暂时无模型或已禁用 Provider 将无法预先授权。

#### ProviderAccess DTO

```json
{
  "mode": "selected",
  "provider_ids": ["codexoauth", "deepseek"]
}
```

所有数组按 ID 稳定排序。请求使用 `DisallowUnknownFields`。

#### 创建客户端 Key

```http
POST /admin/api/client-api-keys
```

```json
{
  "id": "workorch",
  "enabled": true,
  "provider_access": {
    "mode": "selected",
    "provider_ids": ["deepseek"]
  }
}
```

- `provider_access` 必填。
- 创建和关联写入必须在同一 DuckDB 事务。
- 只有持久化和运行时索引激活全部成功后才返回明文 Key。
- 响应继续使用 `Cache-Control: no-store`，明文只显示一次。

#### 列表

```http
GET /admin/api/client-api-keys
```

每条记录增加：

```json
{
  "id": "workorch",
  "enabled": true,
  "provider_access": {
    "mode": "selected",
    "provider_ids": ["deepseek"]
  },
  "effective_provider_ids": ["deepseek"],
  "unavailable_provider_ids": [],
  "effective_model_count": 3
}
```

定义：

- `provider_ids` 是持久化策略。
- `effective_provider_ids` 是当前至少能形成一个目录候选的授权 Provider，不按瞬时健康度过滤。
- `unavailable_provider_ids` 是已绑定但当前禁用、无账号、未发现模型或异常缺失，因而没有目录候选的 Provider。
- `effective_model_count` 按 scoped catalog 去重计数。

`all` 模式下 `provider_ids` 为空，effective 字段仍按当前目录计算。

#### 更新访问策略

```http
PUT /admin/api/client-api-keys/{id}/provider-access
```

body 为完整 ProviderAccess DTO，使用 PUT 替换语义，不提供增量 add/remove 接口。成功返回更新后的完整 `clientKeyView`。

现有：

- `PATCH /client-api-keys/{id}` 继续只修改 enabled。
- `POST /client-api-keys/{id}/rotate` 不修改 ProviderAccess。
- `DELETE /client-api-keys/{id}` 同步删除访问关联、usage 和交互归档。

#### 查看有效模型

```http
GET /admin/api/client-api-keys/{id}/models
```

返回 Admin 专用投影：

```json
{
  "object": "list",
  "data": [
    {
      "id": "deepseek-v4-flash",
      "provider_ids": ["deepseek"],
      "supported_endpoints": ["/v1/messages", "/v1/responses"],
      "context_window_tokens": 1000000,
      "max_output_tokens": 29000
    }
  ]
}
```

该接口可向管理员展示 Provider ID，但不得返回 base URL、上游 Key、账号 ID 或凭据来源。候选和 endpoint 仍必须使用与数据面相同的 scoped catalog helper。

#### 校验与状态码

| 场景 | 状态 |
| --- | --- |
| mode 非法、all 携带 IDs、selected 无 IDs、未知 Provider | 400 |
| Key 不存在 | 404 |
| Provider 删除时仍被 Key 引用 | 409 |
| usage/client key store 不可用 | 503 |
| 运行时索引准备失败 | 500，且不写数据库、不泄露密钥 |

Admin 错误 envelope 继续沿用 `{"error":{"message":"..."}}`。

### 5.9 运行时激活与失败处理

客户端 Key 创建会生成只返回一次的明文，因此不能先提交数据库、再执行可能失败的 reload。否则会出现“Key 已持久化，但 Admin 未拿到明文”的不可恢复状态。

新增专用 `ClientKeyRuntime` 合同，将构建与激活拆开：

```go
type ClientKeyRuntime interface {
    PrepareClientKeyIndex(map[string]usage.ClientAPIKeyRecord) (*clientauth.Index, error)
    ActivateClientKeyIndex(*clientauth.Index)
}
```

- `PrepareClientKeyIndex` 是纯准备阶段：完整校验记录并构建不可变索引，不修改当前运行状态、不访问网络。
- `ActivateClientKeyIndex` 只执行 atomic pointer swap，不返回错误。

ProviderAccess 创建、更新、启停、轮换和删除的最终一致顺序：

1. 在 Admin `updateMu` 内读取当前全部客户端 Key 记录。
2. 在内存副本上应用本次变更，形成 prospective records。
3. 调用 `PrepareClientKeyIndex`；失败则不写数据库、不改变运行时。
4. DuckDB 事务写入同一变更；失败则丢弃 prepared index，运行时不变。
5. 调用 `ActivateClientKeyIndex` 原子切换；该步骤不得失败。
6. 创建/轮换场景此时才返回一次性明文 Key。

这样数据库与新请求的运行时索引在一次 Admin 临界区内完成切换，在途请求继续使用旧 context 快照。不得通过重载 config.yaml 持久化策略，也不得再用 `runtime.UpdateConfig(cfg)` 处理纯客户端 Key 变更，避免无意义地重建 Provider catalog、HTTP client 和健康状态。

服务启动和 Provider config 热更新仍可从 DuckDB 读取全部 Key 后调用同一个 prepare/activate 实现。任何持久化记录不合法都必须使启动或热更新明确失败，不能把非法 Policy 隐式改成 `all`。

## 6. Web 管理端收口

### 6.1 客户端 Key 列表

在“客户端 Key”页面增加：

- Provider 权限列：显示“全部 Provider”或“已选择 N 个”。
- 有效 Provider 列：显示当前有效数；存在 unavailable 时给出状态提示。
- 可用模型列：显示去重模型数量，并提供“查看模型”操作。
- 操作列增加“编辑权限”，保留启用/禁用、轮换、删除。

移动端优先保留 ID、状态、Provider 权限和操作；时间字段可按现有响应式规则隐藏。操作按钮使用现有 `.actions`，不得因文字长度撑破表格。

### 6.2 创建 Key 对话框

创建表单增加独立“Provider 访问范围”功能区：

1. 使用分段选择：`指定 Provider`（默认）/ `全部 Provider`。
2. `指定 Provider` 下展示可搜索复选列表。
3. 普通 Provider 与内建 Provider 分组，但提交只使用稳定 ID。
4. 每行展示名称、来源、启用状态、当前模型数；不展示 base URL 和密钥状态以外的敏感信息。
5. 已禁用或暂不可用 Provider 允许选择，但显示明确状态和“当前不会发布模型”。
6. 未选择 Provider 时禁用“创建”并显示字段错误。
7. `全部 Provider` 显示说明：未来新增 Provider 也会自动授权。
8. 表单实时显示按当前选择估算的有效 Provider 数和去重模型数。

创建成功后的明文 Key 展示流程保持不变，策略信息不得写入 localStorage/sessionStorage。

### 6.3 编辑权限对话框

新增独立 `clientKeyProviderAccessDialog`：

- 标题显示 Key ID，Key ID 只读。
- 复用创建表单的 mode 和 Provider 选择组件。
- 打开时从服务端记录初始化，不复用上一次未保存草稿。
- 保存调用 PUT 完整替换接口。
- 保存成功关闭对话框、刷新 Key 列表和模型计数。
- 切换 mode 或关闭对话框不会修改服务端。
- 不与启停或轮换操作合并，避免一次操作同时改变凭据生命周期和权限。

### 6.4 有效模型对话框

“查看模型”打开只读对话框：

- 模型 ID；
- 获准 Provider；
- supported endpoints；
- 上下文窗口与最大输出；
- 当前无模型时给出“请检查 Provider 启用状态、模型配置或账号池发现结果”。

列表支持模型 ID 搜索。内容使用表格或 `<dl>`，不使用嵌套卡片。

### 6.5 Provider 删除交互

当 DELETE Provider 返回 409：

- 显示服务端返回的引用 Key ID。
- 不关闭 Provider 编辑对话框或从列表移除行。
- 提示先前往“客户端 Key”修改 Provider 权限。
- 不提供前端静默级联删除绑定的选项。

### 6.6 前端通用要求

- 补齐 `enUS` 的静态与动态文案。
- mode 使用分段控件，Provider 使用 checkbox，多选不使用自由文本。
- 所有保存按钮有 busy/disabled 状态，防止重复提交。
- 对话框支持关闭、Escape、失败后保留用户输入。
- 所有动态字符串使用 `esc()`；Provider ID 不进入 HTML attribute 前必须转义。
- 桌面 1440px、平板 1024px、移动 390px 均不得出现文字遮挡或操作列不可用。
- 页面只保存非敏感显示状态，不保存 API Key、ProviderAccess 草稿或完整模型目录到浏览器持久化。

## 7. 安全、观测与性能

### 7.1 安全

- ProviderAccess 是授权边界，不能只作为 UI 过滤。
- 数据面拒绝未授权模型时使用 `model_not_found`，防止模型枚举。
- Admin 可以查看 Provider 关联；业务 `/v1/models` 不能暴露 Provider ID。
- 原始客户端 Key、摘要、Provider Key 和账号凭据继续禁止进入日志、响应、归档和 Web DOM。
- `all` 是显式高权限模式，Web 必须提示会自动包含未来 Provider。

### 7.2 观测

- usage、metrics 和 interactions 继续记录实际选中的 `provider`、`model`、`api_key_id`，不新增高基数 ProviderAccess label。
- 本地 `model_not_found` 仍按当前 typed error 记录 outcome；不得记录未授权候选列表。
- Admin 权限更新日志最多记录 `api_key_id`、mode、Provider ID 数量，不记录原始 Key。
- Provider 删除被阻止时允许记录 provider ID 和引用 Key ID，二者都是管理员定义的稳定标识。

### 7.3 性能

- 普通业务请求只按单模型候选做 Policy 判断，复杂度 O(该模型候选数)。
- `/v1/models` 扫描全局模型目录，复杂度 O(模型数 + 候选数)，不访问 DuckDB。
- 请求期禁止查询客户端 Key 或关联表。
- 客户端 Key 列表一次批量读取关联，避免 N+1 SQL。
- Policy 的 Provider IDs 在写入和装载时排序；候选判断可使用只读 map 或二分查找，不能每次重新规范化。

## 8. 代码收口清单

### 8.1 后端

- [x] 新增 `internal/pkg/aetherrelayclientaccess` 及单元测试。
- [x] 调整 `internal/pkg/aetherrelayusage/migrations.go` 最终 schema、名称和 reset 顺序。
- [x] 扩展 `usage.ClientAPIKeyRecord`、`usage.Store`。
- [x] 完成 DuckDB Store 的批量读取、创建、替换策略、引用查询和删除事务。
- [x] 完成 MemoryStore 语义对齐。
- [x] 扩展 `clientauth.ClientIdentity` 和认证索引快照。
- [x] 增加专用 `PrepareClientKeyIndex` / `ActivateClientKeyIndex` 运行时接口，替换 Key 变更时的 config 重载。
- [x] 增加 EffectiveCatalog scoped candidates/lookup/model IDs helper。
- [x] 改造 `/v1/models` 的模型、endpoint 和 conversion capability 投影。
- [x] 改造 TransportPlan 解析，确保所有数据端点应用同一 Policy。
- [x] 将内部 Admin feature identity 显式设为 `all`。
- [x] 扩展 Admin 创建、列表、访问策略 PUT、有效模型 GET。
- [x] 通过 `RuntimeConfig.EffectiveCatalogSnapshot` 向 Admin 提供同代只读目录，禁止从 Provider models 重建。
- [x] Provider 删除前检查引用并返回 409。
- [x] 更新 Admin handler/runtime stub 和所有编译期接口实现。
- [x] 保持 Key 删除同步清理 usage、关联和 interaction archive。

### 8.2 Web 前端

- [x] 客户端 Key 列表增加权限、有效 Provider、模型数量。
- [x] 创建 Key 对话框增加 mode 与 Provider 多选。
- [x] 新增编辑 ProviderAccess 对话框。
- [x] 新增有效模型只读对话框与搜索。
- [x] Provider 删除 409 显示引用 Key 提示。
- [x] 补齐 busy、空状态、错误状态和 unavailable 状态。
- [x] 补齐中英文文案。
- [x] 完成桌面/移动端布局检查和脚本语法检查。

### 8.3 文档

- [x] 更新 `docs/design/security.md`：客户端身份包含 ProviderAccess。
- [x] 更新 `docs/design/proxy-core.md`：目录与候选链按 Key scope 过滤。
- [x] 更新 `docs/integration.md`：`/v1/models` 是 Key 作用域目录，业务不得缓存跨 Key 复用。
- [x] 更新 `docs/features.md`：客户端 Key Provider 权限管理。
- [x] 更新 `docs/configuration.md`：schema reset 和升级数据清理说明。
- [x] 更新 `docs/operations.md`：Provider 删除冲突、模型不可见和 503 排障路径。
- [x] 更新 `docs/deployment.md`：升级后重新创建 Key 的发布步骤。

## 9. 测试与验收矩阵

### 9.1 领域与存储测试

- Policy mode、规范化、去重、排序、clone、deny-all 零值。
- fresh schema 包含 mode、关联表、索引和新 schema name。
- 旧 schema name 触发仅 usage owner 表重建。
- 创建 Key 与关联原子成功/失败回滚。
- 替换策略不残留旧关联。
- Key 删除同步删除关联和 usage。
- Provider 引用查询稳定排序。
- DuckDBStore 与 MemoryStore 行为一致。

### 9.2 认证与热更新测试

- `all`、`selected` Policy 随认证结果进入 context。
- 禁用、未知、冲突 Header 仍为 401 且不创建 usage。
- Policy 切换后新请求使用新快照，在途请求保持旧快照。
- 轮换 Key 保留 Policy。
- 内部 Admin identity 始终为显式 `all`。

### 9.3 Catalog 与 `/v1/models`

- Key A 只绑定 Provider A，只看到 A 的模型。
- Key B 只绑定 Provider B，只看到 B 的模型。
- 同名模型在不同 Key 下 endpoint/capability 投影不同。
- 未授权候选不能贡献 conversion、tools、images 或 supported endpoint。
- `all` 与当前全局目录结果一致。
- selected unknown/deny-all 返回空模型列表。
- GET/POST `/v1/models` 一致且不暴露 Provider ID。

### 9.4 路由矩阵

至少覆盖每个数据端点：

| 场景 | 预期 |
| --- | --- |
| 请求获准 Provider 独占模型 | 正常路由 |
| 请求全局存在但未授权模型 | 404/400 协议对应的 `model_not_found` |
| 同模型高优先级 Provider 未授权 | 跳过，使用获准低优先级候选 |
| 获准候选失败，未授权候选健康 | 不回退未授权候选 |
| 多个获准候选 | 保持 priority、fallback、health 规则 |
| 获准 Provider 全部熔断 | `provider_unavailable` |
| 未授权 Provider 独有 conversion Level 3 | 请求预检拒绝，不泄露能力 |
| 内部临时对话/搜索/图片 | 显式 all，行为不回归 |

### 9.5 Admin API

- 创建 selected/all、非法 mode、空 selected、all 携带 IDs、未知 Provider。
- 列表 projection 和模型数量准确。
- PUT 完整替换并热生效。
- PATCH enabled、rotate 保留 Policy。
- DELETE Key 清理关联、usage、archive。
- Provider 被引用时 DELETE 返回 409；解除引用后可删除。
- 内建 Provider 可绑定但不可删除。
- Admin mutation/CSRF/Origin 保护保持有效。

### 9.6 Web 与回归

- 新建、编辑、查看模型、启停、轮换、删除完整流程。
- selected 未选择时不能提交；all 有未来 Provider 风险提示。
- 禁用/无模型 Provider 的状态展示。
- Provider 删除冲突提示可定位 Key。
- Chrome desktop/mobile 截图无重叠，按钮可操作。
- 前端 `<script>` 通过 `node --check`。
- `scripts/check-format.sh`、`git diff --check`、`go test ./...` 全部通过。

## 10. 建议开发顺序

1. **领域与 schema**：Policy 包、最终 schema、DuckDB/Memory Store 和测试。
2. **认证快照**：ClientIdentity、索引构建、专用 reload 和热更新测试。
3. **Scoped Catalog**：候选、模型 ID、endpoint/capability 过滤及纯函数测试。
4. **路由强制执行**：所有数据端点使用 Policy，先完成安全边界再开放 Admin 写入口。
5. **Admin API**：创建/列表/PUT/models、Provider 删除引用检查。
6. **Web 管理端**：列表、创建、编辑、模型详情、冲突提示和响应式布局。
7. **集成验证**：用至少两个 Key、两个 Provider 和一个同名模型执行完整转发矩阵。
8. **正式文档**：刷新安全、路由、集成、配置、运维和部署文档。

每一步都必须保持可编译和测试通过；不得先只过滤 `/v1/models` 再延后执行期授权，否则会产生可绕过的伪访问控制窗口。

## 11. 完成定义

满足以下条件才可认为代码收口完成：

1. 客户端 Key 创建时必须明确 ProviderAccess，运行时不存在隐式全权限零值。
2. `/v1/models` 和所有模型执行端点使用同一 scoped candidate 规则。
3. 未授权 Provider 永远不会进入 TransportPlan、fallback 或 capability 投影。
4. Provider/模型/账号池变化能自动联动 Key 的有效模型，无需保存模型副本。
5. Provider 删除不会留下非原子级联结果，存在引用时明确拒绝。
6. Admin 前后端可完整创建、查看、修改和验证关联。
7. schema、MemoryStore、DuckDBStore、认证、catalog、路由、Admin API 和 Web 均有对应测试。
8. 正式集成与运维文档明确说明模型目录按 API Key 隔离。
9. 全量格式、测试和桌面/移动端可视检查通过。
# 图片与交互数据作用域

客户端 Key 的稳定 ID 同时是图片任务、图片资产和交互归档的生命周期作用域。`/v1/images/generations`、`/v1/images/edits` 从认证上下文取得该 ID；Admin 图片任务与图片库 API 必须显式选择已存在的 Key ID。图片索引与标签使用 `(api_key_id, path)` 复合主键，文件系统按安全化 Key ID 分目录。删除客户端 Key 时，先清理任务、图片/缩略图/标签和 `interactions/{api_key_id}/`，再删除 Key 记录并激活新认证索引；任何缺失或未知作用域都拒绝，不回退到共享目录。
