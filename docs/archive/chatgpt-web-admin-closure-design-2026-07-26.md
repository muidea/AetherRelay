# ChatGPT Web 管理页收口设计

Status: implemented

Type: web-design-and-closure-plan

Last Updated: 2026-07-26

## 1. 目的与结论

`AetherRelay` 已迁移 ChatGPT Web 上游、账号池、图片任务和本地图片管理能力，但 Admin Web 当前只提供 Provider、客户端 Key 与使用统计。结果是已存在的管理 API 没有可用的操作入口，管理员必须直接调用 HTTP API 才能维护 ChatGPT 账号和图片。

本设计将既有单页 Admin 管理台扩展为同时承载通用代理管理和 ChatGPT Web 运维的页面；不把旧 `chatgpt2api` Web 原样搬运，也不改变当前 Go 模块划分、事件 owner 或上游调用路径。

本轮确认的决策：

1. 在既有 `web/admin/index.html` 中新增一级页签“ChatGPT Web”，其下设置“账号池”“图片任务”“图片库”三个二级页签。
2. 首轮只消费已经存在的 `/admin/api/chatgpt/**` 管理 API；除本设计明确列出的最小接口缺口外，不新建 Module、Block 或平行后端。
3. 管理页仅面向 Admin，沿用现有会话、loopback 限制、CSRF 和写操作确认机制；不引入旧系统的 `admin/user` 多角色体系。
4. 账号 token、refresh token、密码和代理地址均不得出现在列表、编辑表单、错误提示、浏览器持久化状态或普通通知中；邮箱直接显示。
5. 账号导出是唯一的有意敏感响应：必须二次确认、`Cache-Control: no-store`、不写入 localStorage/sessionStorage、下载或复制完成后立即释放内存引用。
6. 图片任务的 `owner_id` 是任务隔离边界，页面必须由管理员显式选择或输入，不能静默使用一个固定共享 owner。

## 2. 范围

### 2.1 本轮交付

| 能力 | 账号池 | 图片任务 | 图片库 |
| --- | --- | --- | --- |
| 查询与筛选 | 状态、类型、邮箱关键词 | 指定 owner 下的任务和状态 | 日期范围、标签 |
| 创建/提交 | token JSON/文本导入、OAuth 起止 | 文生图、图生图、恢复轮询 | 不适用 |
| 维护 | 编辑状态/类型/配额、批量刷新、删除、受控导出 | 任务状态展示、失败重试/恢复轮询 | 标签维护、批量删除、存储统计 |
| 结果呈现 | 账号汇总与刷新进度 | 图片结果缩略图/失败原因 | 图片缩略图/预览 |

### 2.2 明确不在范围

以下旧页面能力已被产品决策排除，不能在实现中以“兼容”为由重新加入：

- CPA/Sub2API 导入、连接和池配置；
- egress proxy、Cloudflare clearance、FlareSolverr 与代理探测；
- R2、WebDAV、远端图片同步、远端存储策略；
- 密码重登；
- 备份管理；
- PPT/PSD、可编辑文件和其他 `/files/**` 任务；
- Python 版本的多角色用户体系；
- 通用日志删除、旧 Debug Chat 页面。

当前也不做图片压缩/清理策略 UI。它们不是图片库的基本可用条件，未来若实现必须先定义保留策略、审计与不可逆删除边界。

## 3. 信息架构与导航

现有一级页签保持不变：`Provider 管理`、`客户端 Key`、`使用统计`。新增的页面层级如下：

```text
Admin
├── Provider 管理
├── 客户端 Key
├── 使用统计
└── ChatGPT Web
    ├── 账号池
    ├── 图片任务
    └── 图片库
```

“ChatGPT Web”页签应仅在其运行时能力可用时展示；若 Admin API 返回 `503`，保留页签但显示“ChatGPT Web 组件未启用或不可用”的空状态和重试按钮，而不是渲染空表格或把失败误解为没有数据。

页面必须继续使用 `ADMIN_BASE` 与 `apiURL()` 构造 URL，支持非默认 `/admin` base path；不得硬编码 `/admin`。

## 4. 页面功能定义

### 4.1 账号池

#### 列表与汇总

进入页签时调用 `GET /api/chatgpt/accounts`。页面提供：

- 汇总卡片：总数、正常、限流、异常、禁用，以及可用配额合计；
- 本地关键词搜索（邮箱）、状态与类型筛选；
- 表格列与原 `chatgpt2api` 账号池的运营视图一致：账号 ID、类型、来源、状态、脱敏账号信息、创建时间、额度、恢复时间、在途图片数、成功、失败和操作；表格列按可用视口自适应，小屏隐藏低优先级列并保持关键操作可见，不出现横向滚动条；
- 支持多选，批量刷新、批量删除和批量导出；
- 所有 token 只允许后端返回的脱敏值用于辅助识别，页面不提供 token 复制按钮。

账号 ID 应成为所有写操作的唯一标识。页面不得以 token、邮箱或表格下标作为 API 请求标识。账号池 owner 的非敏感只读 `AccountView` 已提供 `restore_at`、`image_inflight`、`success`、`fail` 和 `created_at`，用于该运营表格；其中在途图片数为当前进程运行态，进程重启后归零。与旧页面不同，账号列使用稳定账号 ID，邮箱直接显示，且不显示或复制完整 token。

#### 导入、编辑、刷新与删除

导入对话框只支持当前后端接收的 access token 集合：可将文本按换行/逗号拆分，也可粘贴 JSON 数组（字符串或含 `access_token` 的对象）；前端提取、去空白和去重后仅提交 `tokens`。`source_type` 可由下拉框选择“手工导入”或留空；UI 不提供 CPA/Sub2API 选项。关闭、取消或导入完成后必须清空原始输入。

编辑对话框只允许 `type`、`status`、`quota` 三项；当前管理 API 出于安全设计会清空 `proxy`，故页面不得显示或编辑 proxy。单个或选中的账号可提交刷新，随后按 `progress_id` 轮询进度，直至终态后重新加载列表。刷新中的按钮应禁用重复提交，但不阻塞其他只读操作。

删除必须显示选中账号数量和不可逆提示。成功后清空已删除的选中状态并重新加载。

#### OAuth 与导出

OAuth 使用独立对话框：管理员可填可选 `email_hint`，调用 start 后显示上游登录步骤；提交 callback 前必须显示会话仍在进行中。OAuth URL、callback 和 session ID 不得写入 URL、浏览器存储、日志或普通 toast。完成或取消后销毁内存状态并刷新账号列表。

导出采用“选择账号 -> 二次确认 -> POST -> 浏览器下载”的流程。导出内容在请求完成后只保留到 Blob 创建完成；不在页面预览、不显示在 DOM、不把服务端响应字符串写入错误信息。导出结束后主动执行 `URL.revokeObjectURL`。

### 4.2 图片任务

图片任务是 Admin 代为测试/运维 ChatGPT 图片能力的工作台，不是面向最终用户的聊天产品。页面顶部必须存在必填的 `owner_id` 控件，并明确说明它决定任务的查询与恢复隔离边界。

页面包含：

- 生成表单：`owner_id`、`client_task_id`、prompt、model、size、quality；`client_task_id` 默认生成 UUID，并允许重新生成；
- 编辑表单：在生成表单的基础上提交至少一张图片。首轮支持 data URL 或已可访问的图片 URL；不额外引入上传/文件服务；
- 任务列表：任务 ID、会话标识（若有）、模式、模型、状态、进度、耗时、错误摘要、结果缩略图、提交/更新时间；状态与已知进度用中文运营语义显示，错误高亮且截断，缩略图可预览；表格按可用视口自适应，小屏隐藏低优先级列，不出现横向滚动条；
- 自动轮询只针对当前页面发起且未终态的任务；离开页签、切换 owner 或页面卸载时取消 timer；
- 失败任务已有 `conversation_id` 即显示“恢复轮询”，默认 `extra_timeout_secs=30`；恢复只读取同一会话，不会再次提交生成，可处理轮询超时及历史版本误记为 `"<nil>"` 的任务。尚未建立会话且 `bootstrap` TLS/超时失败的生成任务显示“重新提交”，其它失败不提供盲目重试；
- 图片可点击预览。后端返回的 URL 必须作为普通图片资源加载，不能内联不可信 HTML。

提交或恢复成功应将返回任务加入当前 owner 的列表，再开始受控轮询。切换 owner 时清空上一个 owner 的列表与所有轮询任务，防止跨 owner 显示。

### 4.3 图片库

图片库使用本地图片存储的只读结果和受控维护操作。进入页签时并行读取图片列表、标签列表和存储统计。

- 存储卡片展示图片数量与已用空间等后端返回的统计值；
- 日期范围为可选的 `start_date`、`end_date`，并在修改后重新查询；标签筛选首轮在已加载结果中本地完成；
- 网格显示缩略图、生成时间、模型/任务标识（若后端提供）和标签；
- 点击缩略图打开同页 lightbox；不创建新的公开文件下载端点；
- 可对单个图片编辑标签，标签提交成功后局部更新或重新查询；
- 批量删除必须二次确认，并说明其不可恢复；成功后刷新列表与统计；
- 原图片 URL 来自受保护 Admin 上下文时，图片标签需使用同源 URL；不向第三方 URL 发送 Admin 认证 Header。

## 5. 现有 API 合同与前端映射

所有路径以下均相对于 `ADMIN_BASE`，即通常的 `/admin`。GET 也必须使用既有管理认证请求封装；所有写操作必须附带 `X-AetherRelay-Admin: 1`、当前 CSRF Header 和 JSON `Content-Type`。

| 页面操作 | 方法和路径 | 请求关键字段 | 前端处理 |
| --- | --- | --- | --- |
| 账号列表 | `GET /api/chatgpt/accounts` | 无 | 仅消费脱敏 view；本地过滤/汇总 |
| 账号导入 | `POST /api/chatgpt/accounts` | `tokens`、可选 `source_type` | 创建后全量刷新 |
| 账号编辑 | `PATCH /api/chatgpt/accounts/{id}` | `type`、`status`、`quota` | 成功后替换或刷新行 |
| 账号删除 | `DELETE /api/chatgpt/accounts` | `ids` | 二次确认后刷新 |
| 账号刷新 | `POST /api/chatgpt/accounts/refresh` | `account_ids` | 接收 `progress_id` 后轮询 |
| 刷新进度 | `GET /api/chatgpt/accounts/refresh/progress/{id}` | 无 | 终态停止轮询并刷新列表 |
| OAuth 开始/完成 | `POST /api/chatgpt/accounts/oauth/start`、`.../finish` | hint；`session_id`、`callback` | 仅页面内临时状态 |
| 账号导出 | `POST /api/chatgpt/accounts/export` | `ids` | no-store 下载，不展示明文 |
| 任务查询 | `GET /api/chatgpt/image-tasks` | 必填 `owner_id`，可选 `ids` | owner 切换时清空旧数据 |
| 文生图 | `POST /api/chatgpt/image-tasks/generations` | `owner_id`、`client_task_id`、`prompt`，可选模型参数 | 加入列表并轮询 |
| 图生图 | `POST /api/chatgpt/image-tasks/edits` | 同上，另加 `images` 或 `image` | 同上 |
| 恢复轮询 | `POST /api/chatgpt/image-tasks/{id}/resume-poll` | `owner_id`、可选 `extra_timeout_secs` | 重新进入轮询 |
| 图片列表 | `GET /api/chatgpt/images` | 可选日期范围 | 重绘网格 |
| 图片统计 | `GET /api/chatgpt/images/storage` | 无 | 更新统计卡片 |
| 标签查询/更新 | `GET` / `POST /api/chatgpt/images/tags` | 更新用 `path`、`tags` | 更新对应项目 |
| 图片删除 | `POST /api/chatgpt/images/delete` | `paths` | 二次确认后刷新 |

`GET /api/chatgpt/image-tasks` 在缺失 `owner_id` 时返回 `400`，这不是空任务列表。前端应在 owner 未填写时显示引导状态并不发请求。

## 6. 最小后端接口缺口

首轮页面可基于现有接口实现，不需要为视觉效果扩展领域能力。实现开始前必须先以 Admin API 集成测试确认每个响应 DTO 的字段和稳定性。

仅在下列需求不能由现有响应满足时，才允许补最小接口，且必须先更新本设计和合同测试：

1. 图片缩略图 URL 不是 Admin 同源受控 URL，或无法由浏览器直接安全访问时，可增加一个只读、Admin 鉴权、路径严格校验的图片读取端点；不得暴露通用 `/files/**`。
2. 账号或任务列表确需展示当前合同以外的运营字段时，由其 owner 补充稳定的非敏感只读 view；不得退化为以脱敏 token 作标识。账号池的恢复时间、图片在途数、成功/失败计数和创建时间已由账号池 owner 实现并覆盖合同测试。
3. 图片任务响应不含状态、进度、错误摘要或安全结果引用时，补充只读 task view，不把上游原始响应透传给浏览器。

不应为了页面而增加：远端存储、压缩清理、通用上传、代理配置、账号密码重登或另一套图片任务持久化。

## 7. 安全与交互不变量

1. 延续 Admin 的认证与写操作保护：未登录返回认证错误，非 loopback 的未启用认证访问被拒绝，写请求必须通过 CSRF/`X-AetherRelay-Admin` 校验。
2. 全部 Admin HTML 和 API 响应使用 `Cache-Control: no-store`；页面不得把账号、任务、图片 URL、OAuth 数据或导出内容写入 localStorage/sessionStorage。
3. 所有动态文本通过既有转义函数渲染；不得将 prompt、邮箱、错误信息、标签或上游字段插入 `innerHTML`。
4. 页面错误只展示安全的服务端错误 envelope，禁止显示完整请求体、OAuth callback、token、data URL 或上游原始响应。
5. 导出操作使用专用确认对话框，不能通过普通 `<a href>` 或 GET URL 触发；完成后清理 Blob URL 与 JavaScript 引用。
6. 批量删除、导出、OAuth 完成、刷新和图片任务提交均应防重复提交；请求完成后恢复按钮状态。
7. 自动轮询应使用有限退避（例如 1s、2s、3s，最大 5s）并有总时限；连续网络失败时停止并让管理员手动恢复，不能无限请求。

## 8. 代码落点与组件边界

| 位置 | 责任 |
| --- | --- |
| `web/admin/index.html` | 新增一级/二级页签、视图状态、表单、无障碍标签、API 调用、轮询和安全清理。首轮保持单文件，避免为静态管理台引入前端构建链。 |
| `web/embed.go` | 无逻辑变更；继续嵌入 Admin 静态文件。 |
| `internal/modules/application/adminapi/service/admin` | 保持 HTTP adapter；仅在第 6 节确认的 DTO/受保护读取缺口存在时添加最小 handler 与合同测试。 |
| `internal/modules/application/adminapi/biz` | 继续编排已迁移的 account pool、image task 与 image store 能力；不得由 Web 直接跨 Module 调用。 |
| `chatgptaccountpool`、`chatgptimagetask`、`chatgptimagestore` | 保持各自领域 owner、事件与 DTO；本轮 Web 接入不是迁移这些组件的理由。 |

前端不可定义或复制领域 DTO。HTTP payload 仅在 Admin 页面中作为 transport view 使用；跨组件合同仍由当前 owner 的 `pkg/events` 管理。

## 9. 实施批次

### Phase 1：账号池（必须）

完成页签、列表、筛选、导入、编辑、刷新进度、删除、OAuth 和受控导出。补充 Admin handler 与嵌入页面测试，重点验证敏感字段不会被渲染或持久化。

### Phase 2：图片任务（必须）

完成显式 owner、生成/编辑、任务查询、有限轮询、恢复轮询和安全的结果预览。先确认 task result DTO 可安全展示；不足时按第 6 节补最小 read model。

### Phase 3：图片库（必须）

完成存储统计、日期筛选、网格/预览、标签和删除。验证图片 URL 的 Admin 鉴权、路径校验和 CSP 是否支持安全加载。

### Phase 4：可观测性衔接（后续可选）

如果运营需要，将使用统计页增加 ChatGPT account ID（非 token）和图片任务的聚合筛选；这属于 usage schema 变更，必须单独设计，不能在本轮临时添加。

## 10. 验收标准

### 功能验收

1. 管理员可在浏览器完成账号导入、编辑、刷新、OAuth、删除和确认导出，无需手工调用 `/api/chatgpt/accounts/**`。
2. 管理员可为指定 `owner_id` 提交图片生成与编辑任务，看到状态变化，刷新页面后可重新查询并恢复未终态任务。
3. 管理员可浏览本地图片、按日期查看、编辑标签、删除选中图片，并看到存储统计更新。
4. 既有 Provider、客户端 Key 与使用统计功能回归通过；非默认 Admin base path 仍可正常使用。

### 安全验收

1. 账号列表和编辑响应中没有完整 access token、refresh token、密码或 proxy；页面 DOM、网络失败通知、localStorage/sessionStorage 和 URL 中均无这些值。
2. 导出响应带 `Cache-Control: no-store`，需要二次确认，且页面不记录其内容。
3. 缺失 `owner_id` 不发送任务查询；切换 owner 不显示上一个 owner 的任务。
4. 所有写操作携带既有 Admin mutation headers；未经授权、缺 CSRF、错误 base path 和非 loopback（认证关闭时）访问均按现有安全合同拒绝。
5. prompt、标签和后端错误被当作文本显示，不能通过管理页执行脚本。

### 验证方式

- Go：`GOCACHE=/tmp/AetherRelay-gocache go test ./... -count=1`；
- 新增 Admin HTTP 合同测试，覆盖所有页面调用的成功、400、401/403、503 和敏感字段脱敏；
- 浏览器冒烟：账号 CRUD/刷新/OAuth mock、图片任务 submit/poll/resume、图片标签/删除；
- 使用指定的本地账号数据进行一次受控 live 验证，仅记录状态码、任务 ID/脱敏账号标识和功能结论，不记录账号凭据、OAuth URL、上游 URL、图片正文或上游响应正文。

## 11. 文档收口

代码完成后需同步：

- `README.md`：把 Admin 页面说明更新为“Provider、客户端 Key、使用统计与 ChatGPT Web 管理”；
- `docs/configuration.md`：说明 ChatGPT Web 组件启用条件、Admin 权限前提与数据目录；
- `docs/operations.md`：补充账号导出、OAuth、图片删除的运维安全注意事项；
- 本文状态改为 `implemented`，并记录实际补充的最小 API 合同和验证结果。

## 12. 实现记录（2026-07-26）

### 实际代码落点

| 位置 | 变更 |
| --- | --- |
| `web/admin/index.html` | 新增一级页签「ChatGPT Web」与二级「账号池 / 图片任务 / 图片库」；导入/编辑/刷新/OAuth/导出、显式 owner 图片任务、图片网格/标签/删除与 lightbox；路由 hash `#/chatgpt/{accounts\|tasks\|images}` |
| `internal/modules/application/chatgptaccountpool/pkg/events/contract.go`、`internal/modules/application/chatgptaccountpool/internal/store/store.go` | 账号池 owner 的只读 view 补齐恢复时间、在途图片数、成功/失败计数和创建时间；图片结果写入成功/失败计数，在途数仅代表当前进程运行态 |
| `internal/modules/application/adminapi/service/admin/handler.go` | 最小只读内容端点 `GET /api/chatgpt/images/content?path=&thumb=`；列表响应将 `url`/`thumbnail_url` 重写为 Admin 同源受控 URL |
| `internal/modules/application/adminapi/biz/chatgpt.go` | `GetChatGPTImageBytes` / `GetChatGPTImageThumbnail` 经 EventHub 调用 imagestore owner |
| `internal/modules/application/adminapi/service/admin/chatgpt_accounts_test.go` | 账号运营字段与敏感字段脱敏、列表 URL 重写、内容端点成功/路径穿越/503 合同测试 |

### 最小 API 合同补充

仅第 6 节第 1 项：

- `GET <admin_base_path>/api/chatgpt/images/content?path=<store-relative>&thumb=1`（可选）
- 要求 Admin 会话（认证开启时）；`Cache-Control: no-store`；拒绝 `..` 等非法路径；不暴露通用 `/files/**`
- `GET .../api/chatgpt/images` 返回的图片 URL 统一为上述 content 端点，不再使用未挂载的 `/images/**` 公共路径

未增加：远端存储、压缩清理、通用上传、代理配置、密码重登、平行图片任务持久化。

账号池表格使用稳定账号 ID 替代旧页面 token 列，其他运营列与原 `chatgpt2api` 对齐；列宽按视口自适应，窄屏隐藏低优先级列以避免横向滚动。完整 token 与 proxy 仍不进入普通管理页，邮箱直接显示。

### 验证结果

- `node --check`：`web/admin/index.html` 内联脚本语法通过
- Admin 包合同测试（含 `TestChatGPT*`）：通过
  `LIBRARY_PATH=/usr/lib/gcc/x86_64-linux-gnu/14 go test ./internal/modules/application/adminapi/service/admin/ -count=1 -run 'TestChatGPT'`
- 浏览器冒烟与受控 live 上游验收：待在启用 ChatGPT Web 组件的本地实例上由运维执行；实现侧已覆盖 503 空状态、缺失 owner 不发请求、导出 Blob 即用即销与 OAuth 内存态清理

## 临时对话页与 API（2026-07-27 增补）

在 ChatGPT Web 管理页增加「临时对话」子页：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/chatgpt/temporary-conversations` | 创建会话并固定账号 |
| `GET` | `/api/chatgpt/temporary-conversations` | 历史摘要分页 |
| `GET` | `/api/chatgpt/temporary-conversations/{id}` | 会话详情与消息页 |
| `POST` | `/api/chatgpt/temporary-conversations/{id}/turns` | 启动一轮 |
| `GET` | `/api/chatgpt/temporary-conversations/{id}/turns/{turn_id}/events` | 长轮询增量 |
| `POST` | `/api/chatgpt/temporary-conversations/{id}/turns/{turn_id}/cancel` | 取消本轮 |
| `DELETE` | `/api/chatgpt/temporary-conversations/{id}` | 永久删除 |

安全不变量：owner 仅来自已认证 Admin principal；响应 `Cache-Control: no-store`；页面不将消息正文/上游 conversation id/token 写入 localStorage 或 sessionStorage；错误 envelope 不回传 prompt 或原始 SSE。详见 `archive/chatgpt-temporary-chat-design-2026-07-27.md`。
