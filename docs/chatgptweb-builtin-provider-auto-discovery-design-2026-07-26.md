# ChatGPT Web 内建 Provider 自动发现实现方案

Status: implemented

Type: architecture-and-closure-plan

Last Updated: 2026-07-26

## 1. 目标与结论

启用 `chatgpt_web.enabled` 后，ai-proxy 必须自动提供一个固定 ID 为 `chatgptweb` 的内建 Provider。管理员不再在 `providers` 配置、`model_catalog` 或 Provider 管理页中手工创建该 Provider、填写模型匹配、上游地址、API Key 或端点能力。

该 Provider 的模型与模型级 operation 来自当前账号池中可用账号的 ChatGPT Web 模型枚举结果。有效模型目录同时驱动：

```text
账号池账号快照 ──> ChatGPT Web 上游模型枚举 ──> 账号级模型快照
                                                  │
静态配置目录 ───────────────────────────────────> Proxy 有效目录
                                                  ├── GET /v1/models
                                                  └── 请求期模型路由与账号选择
```

这不是向 YAML 动态回写模型或 Provider，也不是把账号凭据变成普通远程 Provider 的 `api_key`。`configruntime` 仍是用户显式配置的唯一 owner；`proxyapi` 仅在内存中合成请求期有效目录。

## 2. 范围

### 2.1 本轮交付

- `chatgpt_web.enabled: true` 时自动注入固定内建 Provider `chatgptweb`；
- 从 `ChatGPT Web /backend-api/models` 枚举模型，按账号保存可用模型与已验证 operation；
- 用“模型并集 + 按模型调度”向客户端透出模型；
- 将自动发现结果接入 `/v1/models`、`/v1/chat/completions`、`/v1/images/generations`、`/v1/images/edits`；
- Admin Provider 列表展示不可编辑的内建 Provider、可用性和冲突摘要；
- 在账号新增、删除、状态变化、手工刷新、周期刷新和模型快照过期后收敛有效目录；
- 覆盖无账号、模型冲突、部分账号差异、模型下线与上游枚举失败等测试场景。

### 2.2 不在范围

- `responses`、embeddings、Anthropic Messages、Assistants、Files、PPT/PSD；
- 账号选择由调用方指定、在公开 API 返回账号 ID 或任何 token；
- 将发现结果写入 `config.yaml`、环境变量、日志或版本库；
- 远端图片存储、Cloudflare clearance、egress proxy、密码重登；
- 自动猜测上游未明确声明的模型 operation 或容量。

## 3. 当前基线与需要替换的规则

当前代码已经实现 `chatgptweb` 协议的文本和图片传输，但要求用户在 `providers` 中显式声明 Provider，并在 `model_catalog` 中静态登记模型；启动校验要求一个 model 只能匹配一个 enabled Provider。这与本方案冲突，须完整替换，不保留兼容配置路径。

下列旧配置将在实现后视为无效并给出明确错误：

```yaml
providers:
  anything:
    protocol: chatgptweb
```

同样，`model_catalog` 不得用来手工声明路由到 `chatgptweb` 的模型。普通远程 Provider 的静态模型目录保持原有行为。

## 4. 领域决策

### 4.1 内建 Provider 合同

| 字段 | 固定规则 |
| --- | --- |
| Provider ID | `chatgptweb`（保留，大小写敏感） |
| 是否持久化到 YAML | 否 |
| `base_url` / `api_key` | 不存在；账号凭据只由账号池 owner 管理 |
| 直连能力 | `chat_completions`、`images` |
| 外部路径 | `/v1/chat/completions`、`/v1/images/generations`、`/v1/images/edits` |
| 客户端鉴权 | 仅 ai-proxy Client API Key |
| 无账号或无可用模型 | Provider 不可用；不把失效账号泄露给客户端 |

模型 operation 是模型级而非 Provider 级：只有上游明确支持文本的模型可提供 `chat_completions`，只有明确支持图片的模型可提供 `image_generations`。图片 operation 同时覆盖 generation 与 edit 路径，沿用当前 operation 合同。

### 4.2 并集与模型感知调度

有效目录采用“至少一个健康账号支持”的模型并集，不采用所有账号交集。这样 Plus、Team、不同地区或不同额度账号的差异不会压缩可透出能力。

请求必须带 `model` 和由入站 path 推导的 `operation`；账号池只从同时满足以下条件的账号中选择：

1. 账号状态与配额允许当前 operation；
2. 账号的最新模型快照包含请求模型；
3. 该模型快照包含所需 operation；
4. 图片请求还满足并发槽位限制。

没有候选账号时返回稳定的 Provider 不可用/模型不可用错误，不随机挑选不支持该模型的账号，也不回退到其它 Provider。

### 4.3 模型元数据可信度

上游模型枚举响应中的 `slug` 是模型 ID 的唯一来源。operation、创建时间、归属和容量只在上游有明确、已通过 fixture/live 验证的字段时投影。

静态 `model_catalog` 的容量字段当前为必填；自动目录不得伪造容量。实现时应将 Proxy 请求期目录与静态 `config.ModelInfo` 分离：自动模型的上下文窗口和最大输出为可选元数据，`/v1/models` 在未知时省略扩展字段。路由不依赖这些数值。

## 5. Owner 与落点

不新增通用 Registry、全局 Service facade 或新的 framework Module。现有 `proxyapi` 已经是组合配置、账号池、ChatGPT Web 上游和图片存储的 Application Module，适合拥有“有效路由目录”这个编排读模型。

| 位置 | owner / 职责 | 本次变化 |
| --- | --- | --- |
| `blocks/chatgptwebupstream` | ChatGPT Web 协议与 HTTP transport | 新增模型枚举 typed command/result；client 解析受限、可审计的模型 DTO |
| `application/chatgptaccountpool` | 账号正式状态、账号级能力快照与模型筛选 | 保存每账号模型快照；提供发现候选、写入快照、按 `model + operation` 获取文本/图片账号的合同 |
| `application/proxyapi` | 入站协议、路由编排、有效目录读模型 | 合成静态配置与内建目录；驱动发现任务；将有效目录原子下发给 Handler |
| `blocks/configruntime` | 用户显式 YAML 配置 | 不持久化发现结果；拒绝显式 `chatgptweb` Provider 配置 |
| `application/adminapi` | 管理入站适配器 | 只读展示内建 Provider 状态、模型数量、更新时间和静态模型冲突 |
| `web/admin` | 管理界面 | 显示不可编辑内建行；不提供新增/编辑/删除/探测入口 |

跨 owner 交互全部经 EventHub typed contract 完成。`proxyapi/biz` 内嵌 Base 并拥有发现流程；HTTP Handler 不持有 Hub，upstream Block、账号池 repository/store 和 Admin Handler 之间不直接互调。

## 6. 合同设计

### 6.1 upstream owner：模型枚举

`chatgptwebupstream/pkg/events` 新增由该 owner 定义的合同：

- `TopicListModels`；
- `ListModelsCommand { AccessToken string }`；
- `ListModelsResult { Models []ModelDescriptor }`；
- `ModelDescriptor { ID string; Operations []ModelOperation; CreatedAt int64; OwnedBy string }`。

`ModelOperation` 为受限枚举，只允许 `chat_completions` 与 `image_generations`。解析未知上游字段不进入组件合同；无法确认 operation 的模型不投影到结果。

### 6.2 account-pool owner：能力快照与选择

账号池 owner 新增自己的合同，所有写入仍由其 store 完成：

- 列出需发现的非敏感候选标识与只在 EventHub 内使用的访问凭据；
- 用稳定账号 ID 写入 `AccountModelSnapshot`；
- 查询账号池模型目录快照与版本号；
- `AcquireTextTokenCommand`、`AcquireImageTokenCommand` 增加 `Model`、`Operation`，按能力快照筛选；
- 账号删除、禁用、失效、刷新结果改变时递增目录版本。

快照属于账号资源的派生状态，可持久化在原账号对象的受控扩展字段中；不持久化原始上游响应、token 或未约束 JSON。加载旧 `accounts.json` 时没有模型快照的账号进入“待发现”，不会被模型路由选中。

### 6.3 proxyapi owner：有效目录

在 `application/proxyapi/pkg/` 新增纯值类型的 focused package，例如 `effectivecatalog`：

```text
Snapshot
├── StaticModels: 来自 configruntime 的已验证静态路由
├── BuiltinProvider: chatgptweb 的状态与更新时间
└── BuiltinModels: 自动发现的模型、operation、可选元数据与冲突状态
```

该 package 不含 EventHub、存储、HTTP 或可变句柄。`proxyapi/biz` 持有其当前快照，并用原子替换将其交给 service Handler。`/v1/models` 与 `ResolveTransportPlan` 必须使用同一 snapshot，避免“列表可见但不能路由”或反向不一致。

## 7. 生命周期与刷新

1. 启动时 Config Block 只加载显式配置；若其中出现 `protocol: chatgptweb`，启动失败。
2. `chatgpt_web.enabled=false` 时不注册内建 Provider，自动目录为空，既有静态配置照常运行。
3. 启用时，`proxyapi/biz.Run` 用 BackgroundRoutine 启动有界发现任务；初始目录为空但服务可启动。
4. 发现任务经账号池取得候选，经 upstream Block 枚举，再将每账号快照写回账号池。
5. 账号池版本变化后，Proxy 重建内建模型并集、冲突摘要和 Provider 健康状态，再原子替换有效目录。
6. 账号导入、OAuth 成功、删除、状态/配额变化、刷新成功和周期到期均触发或标脏下一轮发现；同一账号只允许一个在途发现任务。
7. 单账号枚举超时或失败只影响该账号；保留该账号的上一次未过期成功快照，超过 TTL 后从有效目录移除。
8. 停止时先停止发现调度，再释放 Module/Block；不得遗留 timer 或 goroutine。

模型枚举请求必须设置上限、超时、退避和并发限制，不能因大号池同时向上游发起无界请求。

## 8. 路由、冲突与错误语义

### 8.1 路由

- 静态模型仍按现有唯一 RouteOwner 校验；
- 自动模型无需 `providers.*.models` pattern，直接绑定 RouteOwner `chatgptweb`；
- `ResolveTransportPlan` 先读取有效目录中的 exact model，再按固定矩阵验证 `operation × capability × path`；
- `GET /v1/models` 只投影有效目录，不直接访问上游。

### 8.2 与静态配置冲突

自动发现到的模型如果已由任一 enabled 静态 Provider 精确路由，静态 Provider 优先，ChatGPT Web 的同名模型不注入有效目录。Admin 内建 Provider 行必须显示冲突数量与模型 ID 摘要。

这条规则确保任何时刻同一个 model 只有一个 RouteOwner；不支持请求期 fallback、`X-AI-Provider`、查询参数或 model 前缀选择 Provider。

### 8.3 错误

| 条件 | 客户端结果 |
| --- | --- |
| `chatgpt_web` 未启用 | 模型不存在于有效目录；请求按 `model_not_found` 处理 |
| 启用但尚未发现模型 | `/v1/models` 暂不含内建模型；请求 `model_not_found` |
| 模型在目录中但所有匹配账号均不可用 | `provider_unavailable`，不泄露账号状态 |
| 模型快照过期或上游发现失败 | 到期后从目录移除；未到期时继续使用最近成功快照 |
| 上游执行期限流/失效 | 保持既有分类、账号状态转换和图片槽位释放规则 |

## 9. Admin 与配置体验

- Provider 列表在 ChatGPT Web 启用时显示 `chatgptweb` 内建行；显示“内建/账号池”、可用账号数、可发现模型数、最近成功发现时间、冲突数和不可用原因。
- 内建行没有 Base URL、API Key、编辑、删除、开关或普通 probe 按钮；账号池页面是其唯一运维入口。
- 常规“新增 Provider”表单不提供 `chatgptweb` 协议选项。
- 删除 `config.example.yaml` 中手工 `chatgptweb` Provider 和模型样例；`docs/configuration.md` 改为只说明 `chatgpt_web` 启用与自动发现行为。

## 10. 实施顺序

1. 增加 upstream 模型枚举 client、typed contract、fixture 解析测试；先确认真实响应中的模型 ID 与能力字段。
2. 扩展账号池的账号级能力快照、版本、按模型调度与失效规则；补持久化兼容 fixture。
3. 在 `proxyapi/biz` 增加有界发现流程和 `effectivecatalog` 纯读模型；不改动 Config Block 的持久化职责。
4. 将 Handler 的模型列表和 TransportPlan 从静态 `Config.ModelCatalog` 改为同一有效目录快照；补文本和图片路由测试。
5. 调整 config 校验，拒绝显式 `chatgptweb` Provider 和静态绑定到该 Provider 的模型；移除旧文档样例。
6. 增加 Admin 内建 Provider 状态只读投影和页面行；不暴露账号凭据。
7. 使用受控账号数据完成 live 验证，只记录模型 ID、状态码、脱敏账号 ID 和结论。

## 11. 验收清单

- 启用 ChatGPT Web 且没有 `providers.chatgptweb` 时，内建 Provider 可出现并在首次成功发现后提供模型；
- 一个支持文本、一个支持图片的账号能使 `/v1/models` 同时出现对应模型；
- 文本、流式文本、文生图、图生图均只选择支持目标模型的账号；
- 静态 Provider 与自动模型同名时，静态路由保持唯一，Admin 有冲突提示；
- 禁用/删除唯一支持账号或快照 TTL 到期后，模型从有效目录移除；
- 枚举失败、无账号、上游限流和账号失效不泄露 token、邮箱、上游 URL 或原始响应；
- Admin 不能创建、编辑或删除 `chatgptweb` 内建 Provider；
- `go test ./... -count=1`、直接模块测试、`/v1/models` 与各路径 HTTP 合同测试均通过；
- 结构检查确认没有跨 owner store/client 直接访问、没有全局 registry、没有 EventHub `map[string]any` 合同。

## 12. 明确的破坏性变更

本方案不兼容已有手工 `chatgptweb` Provider 或手工 ChatGPT Web 模型路由配置。实施时不提供兼容桥、双写或静默迁移；管理员必须删除这些旧配置，改为仅启用 `chatgpt_web` 并维护账号池。

## 13. 实现记录（2026-07-26）

### 代码落点

| 位置 | 变更 |
| --- | --- |
| `blocks/chatgptwebupstream` | `TopicListModels` + client `ParseModelsResponse` / `ListModels`；operation 仅投影已验证字段 |
| `application/chatgptaccountpool` | 账号级 `model_snapshot` 持久化、catalog version、按 `model+operation` 选号、discovery/catalog EventHub 合同 |
| `application/proxyapi/pkg/effectivecatalog` | 静态+内建并集纯读模型；静态 exact 冲突优先 |
| `application/proxyapi/biz` | 有界发现任务（立即/30s watch/5m full）、原子发布有效目录 |
| `application/proxyapi/service/proxy` | `/v1/models` 与 `ResolveTransportPlan` 共用 effective catalog |
| `pkg/aiproxyconfig` | 拒绝显式 `providers.*.protocol: chatgptweb` / 名称 `chatgptweb` |
| `application/adminapi` + `web/admin` | Provider 列表只读内建行（账号数/模型数/冲突/不可用原因），禁止编辑删除探测 |

### 验证

- `go test`：aiproxyconfig、effectivecatalog、proxyapi、adminapi、accountpool、chatgptwebupstream client 相关包通过
- 配置合同：显式 chatgptweb Provider 启动失败
- 账号快照：持久化、过期剔除、按模型选号
- 有效目录：静态优先、冲突摘要、disabled/empty/discovering/ready 状态
