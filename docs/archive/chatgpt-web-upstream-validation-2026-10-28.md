# ChatGPT Web 浏览器一致性收口：补充验收记录（2026-10-28）

关联方案：[浏览器访问一致性完善方案](chatgpt-web-upstream-closure-plan-2026-10-28.md)。本文记录真实结果，不将单元测试通过等同于全部功能真实验收通过。

## 1. 结论

已补验流式响应、临时对话、多轮记忆、文本附件、生图、图片编辑、图片下载、任务恢复及重启后的设备身份。随后按用户指定改用 `alexg@mulife.vip`，**图片识别、联网搜索及搜索历史也通过**。本轮计划补验的正常流程已取得成功结果。

首轮 `aleng@mulife.vip` 搜索及图片识别未成功；补充诊断明确观察到该账号的 `gpt-6` 搜索返回 SSE `usage_limit`，并复现收口前 SSE 解析丢失该错误的行为。更换账号后的正常成功不能替代异常场景验收；后续按用户要求完成的本地代码收口及验证见第 8 节。

首轮只补充验证和记录；之后用户要求根据结果进行代码收口，详见第 8 节。

下列首轮实测属于 `aleng@mulife.vip`；`alexg@mulife.vip` 的补验结果见第 5 节。

## 2. 验证环境和操作边界

- 使用本轮源码构建的本地独立实例，监听 `127.0.0.1:44170`；独立配置、数据库、加密密钥和本地 Client Key `functional-check`。
- 按授权使用 x800 号池 `aleng@mulife.vip` 的 Web access token；未导入真实 refresh token，没有执行真实 OAuth 凭据轮换。
- x800 数据库/WAL及加密配置仅用于只读取得本地副本；本地解密后只选取指定账号。设备身份由本地账号库初始化，与生产数据库隔离。
- 本地 CONNECT 经 SSH `-W` 使用 x800 网络出口。调用不经过 x800 已部署的 AetherRelay HTTP 服务；没有改动远端部署、配置或服务生命周期。
- 各次上游操作采用独立 Client/Cookie Jar；图片恢复在本地重启后执行。
- 下表时间均使用东八区；上游原始时间戳仍保留 UTC 表达。

验证前后只读检查的 x800 容器信息一致：

```text
container: deploy-aetherrelay-1
image: ghcr.io/muidea/aetherrelay:latest
started_at: 2026-10-27T16:14:24.097105014Z
```

## 3. 首轮真实调用结果：aleng@mulife.vip

| 功能 | 实测入口及检查 | 结果 |
| --- | --- | --- |
| 账号信息 | 独立上游客户端读取，邮箱匹配 | 通过，5.335 秒；此前最小验证 |
| 模型列表 | 上游列表及本地 `/v1/models` | 通过，12 个模型 |
| 非流式最小文本 | `gpt-6`，要求回复 `OK` | 通过，6.881 秒；此前最小验证 |
| 客户端流式文本 | `/v1/chat/completions`，`stream=true` | 通过，HTTP 200、`text/event-stream`、正文 `STREAM_OK`、有 `[DONE]`，7.228 秒 |
| 临时对话 | 创建会话、提交轮次、轮询事件 | 通过，首轮回复 `SAVED` |
| 多轮历史 | 第二轮要求回忆首轮随机标记，查询已存消息 | 通过，标记匹配，保存 4 条消息 |
| 文本附件 | 实际 multipart 上传 `validation.txt`，模型读取随机标记 | 通过，内容匹配；按会话/消息/附件 ID 下载返回原始 32 字节 |
| 图片生成 | 功能集异步任务，`gpt-6`，白底蓝色圆形，`256x256` | 通过，39.572 秒；1 张 PNG，43,192 字节，尺寸及图像内容符合要求 |
| 图片编辑 | 上传生成图，要求改成红色圆形 | 通过，211.129 秒，包含并行搜索期间的排队等待；1 张 PNG，43,255 字节，256×256，内容符合要求 |
| 管理端图片下载 | `/admin/api/chatgpt/images/content?api_key_id=functional-check&path=...` | 生成图和编辑图均 HTTP 200，完整下载 |
| 图片任务恢复 | 本地注入失败记录，重启后调用 `resume-poll`，读取原会话 | 通过，4.649 秒；恢复图与原生成图逐字节一致 |
| 持久设备身份 | 本地重启前后解密存储字段并比较 SHA-256 | 通过，设备身份未变化 |
| 联网搜索 | `/admin/api/features/search`，请求新加坡 IANA 时区及来源 | 未通过；首次调用 320 秒客户端超时；补充诊断取得明确限额错误，见下节 |
| 临时对话图片识别 | multipart `images` 上传蓝色圆形，要求识别颜色 | 未通过；轮询 HTTP 200 但轮次失败，无答案；实际执行错误为 `assistant response not found in SSE` |

图片任务标识和会话标识：

| 用途 | 本地任务 | 上游会话 |
| --- | --- | --- |
| 生成 | `functional-generate-0c629172` | `6ae1707a-929c-83ec-aada-cf86b63e7450` |
| 编辑 | `functional-edit-b9a8e158` | `6ae171c8-14e4-83ec-b235-43dd04ba41e4` |
| 恢复 | `functional-resume-injected` | 复用生成会话 |

恢复用例仅在**已停止的本地测试数据库**中复制成功任务，保留账号和会话 ID，清空结果并设置“轮询超时”。随后通过正式管理端恢复接口完成获取，没有重投图片生成。该用例验证真实上游恢复链，不等同于观察到上游真实发生过超时故障。

### 图片下载 404 的核对

直接读取任务记录里的未签名 `/images/functional-check/...png` 返回 404，即使附带 Bearer 也不能替代资源签名。现有公共图片端点要求短期签名；管理页的 `taskImageSrc` 优先使用 `b64_json`，否则改写为管理端内容接口。改用该接口后完整下载通过。因此该 404 是探测方式不符合端点合同，未证明图片存储或管理页显示失败。

本轮没有真实调用客户端 `response_format=url` 的签名生成/访问链；相关权限、签名和作用域仍以已有自动化测试结论为准。未进行真实浏览器页面交互验收。

## 4. 搜索失败的证据与待收口项

### 4.1 首次完整应用调用

11:48:12 左右提交；上游创建会话 `6ae1709a-e01c-83ec-8fb0-ccd4752ae619`，但没有取得答案。11:53:32 客户端等待 320 秒后取消；代理记录为 `search_poll` 的 `context canceled`，最终 HTTP 502。使用新的独立连接读取该会话得到上游 HTTP 404。

首次调用没有保留完整上游 SSE，**不能将补充调用的限额错误直接当成首次调用的已证实错误**。

### 4.2 补充完整事件流诊断

本地诊断客户端对新的搜索调用完整读取 SSE，再交给现有解析与轮询。观察到：

- prepare、首页、Sentinel prepare/finalize、conversation 均 HTTP 200。
- 上游新会话：`6ae17243-cddc-83ec-aba9-365807b2513a`。
- conversation SSE 明确包含以下失败事件，随后 `[DONE]`：

```json
{
  "message": null,
  "conversation_id": "6ae17243-cddc-83ec-aba9-365807b2513a",
  "error": "你已达到限额。请稍后重试。",
  "error_code": "usage_limit"
}
```

- 该会话 GET 轮询可以返回 HTTP 200，但没有成功答案；现有搜索仍持续轮询，直到诊断设置的 80 秒期限到达。
- 达到限额后停止新增生成类调用。没有为了继续验证去修改 x800 限额、账号或配置。

### 4.3 收口前已定位的解析问题

使用已记录的相同 SSE 做本地重放，确认：

1. `parseSearchConversationID` 在拿到第一个会话 ID 时返回成功，因此后续 `error_code=usage_limit` 未被识别，仍进入轮询。
2. `parseTextSSE` 对该错误事件没有生成对应分类，最终只返回 `text conversation: assistant response not found in SSE`。

与基线 `6cbc042` 比对，这两处错误解析行为在本轮浏览器一致性修改前已存在。新的请求头能够完成 prepare/finalize/conversation 以及会话 GET，但不能据此推断所有条件下均无回归。

验收时提出的收口项：统一识别 SSE 中的明确错误，保留上游错误码/提示并映射限额分类；搜索在确认失败后立即结束，避免无意义轮询；同时保留已有“不在会话开始后重投生成”的约束。增加“HTTP 200 + SSE usage_limit”“已发会话 ID 后再出错”“有部分输出后出错”的回归用例。第 8 节记录对应实现。

临时对话图片识别在相近时段返回相同的通用解析错误，但未捕获该次原始 SSE，**其原始错误不能直接确认为 usage_limit**。随后改用用户指定账号成功补验了正常流程，见下节；首轮账号恢复后的行为没有重新测试。

## 5. 指定账号补验：alexg@mulife.vip

只读取得新指定账号的 access token；仅在本地实例删除旧测试账号并导入新账号。上游账号信息读取确认邮箱为 `alexg@mulife.vip`，本地池只保留该账号，ID `842f9c212553`。未导入真实 refresh token。

| 项目 | 结果 |
| --- | --- |
| 账号信息与模型目录 | 邮箱匹配；本地模型目录返回 12 个模型 |
| 临时对话图片上传与识别 | 通过；multipart 上传蓝色圆形，模型准确回复 `BLUE`，9.487 秒 |
| 会话内原图下载 | 通过；HTTP 200，43,192 字节，与上传图片逐字节一致 |
| 功能集联网搜索 | 通过；HTTP 200，实际模型 `gpt-6`，11.143 秒 |
| 搜索正文 | 明确回答 `Asia/Singapore`，包含 IANA 时区数据库链接 |
| 搜索来源 | 返回 13 条，包含 IANA 官方来源 |
| 搜索历史落库及读取 | 通过；列表可找到记录，详情正文及来源与当次响应一致 |

搜索成功时间 `2026-10-28T04:01:43.935956Z`，历史 ID `ae4dc2f3-5f95-4001-8e94-56e322338c11`。应用调用事件：图片识别 `7ad1470cf452875a30a3515556d72263`，搜索 `5801d33bf129070031f38f40f2d52d3a`。

验证脚本最初按 `text` 检查搜索正文，误记为失败；实际管理端合同字段是 `output_text`。随后直接核验已保存响应及历史，修正断言为通过，**没有为修正断言重复发起搜索**。导入账号后邮箱尚未回填时的早期身份断言，同样由后续正式账号信息读取确认通过。

## 6. 受控异常与自动化验证

已有最终源码检查通过：`go test ./...`、受影响包 `go test -race`、`go vet ./...`、构建与格式/差异检查。补验期间又运行受影响包的定向 race 异常测试，全部通过，覆盖：

- 首次认证失败后的凭据刷新及一次安全重试；已输出或会话已开始后不重投。
- 搜索 prepare 传输故障的新 Client 重试及受约束的账号切换。
- 临时对话、图片任务在安全边界内的恢复。
- 取消、超时、账号设备身份持久化、旧记录迁移及失败写入。

真实账号凭据失效、真实账号切换和额度恢复未故意制造；这些项目只有受控注入结果，不能写为真实上游故障实测通过。

## 7. 结果文件与清理

本机非凭据证据保存在 `/tmp/aetherrelay-gptweb-functional-20261028/`：`results.jsonl`、应用日志、异常测试日志、搜索诊断日志及错误事件摘要、图片结果与本地重启检查记录。该路径为临时工件，不保证长期保留。

验证结束后停止本地实例和 CONNECT 转发，移除临时源码、导入 token、测试密钥、本地账号数据库及包含临时恢复令牌的原始搜索 SSE。保留原始错误码、提示、会话 ID、时间与结果，不丢失上述诊断事实。没有提交代码或部署 x800。

## 8. 根据验收结果进行 SSE 错误收口

### 8.1 代码行为

- 在现有上游客户端增加共享错误解析 helper，文本、搜索、图片 SSE 共用；不增加运行单元、配置或跨组件资源依赖。
- 识别顶层明确错误、嵌套错误对象及根 delta envelope；普通消息正文和正常 `error:null` 不视为失败。原始错误码/提示分别有界保留为 128/512 字节，继续进入错误链；已观测的完整 `usage_limit` 及中文提示不丢失。
- `usage_limit`、`rate_limit`、`rate_limit_exceeded`、`too_many_requests`、`quota_exceeded`、`insufficient_quota` 映射为现有 `rate_limit` 类别。未知码保留为 `upstream`；SSE 中类似认证的字符串不作为真实 HTTP 401，不触发凭据移除或生成重放。
- 文本遇到明确错误立即返回，保留会话 ID、已有内容、assistant ID 和冻结的部分输出观测；实际 HTTP 状态保留为 200。代理流式路径使用上游传入的错误提示，避免替换成通用失败。
- 搜索不再取得 ID 就结束 SSE：在现有大小上限和请求 context 内读取至 `[DONE]`/EOF，遇到明确错误立即返回，不进入 poll。超过 4 MiB 明确失败，避免截断被误判为成功提交。搜索 poll 收到 429 或 HTTP 200 中的明确 JSON 错误即结束；保留会话和已有部分结果。既有 prepare 阶段恢复预算不扩大。
- 图片 SSE 同样在明确失败后结束，保留会话及已见引用，不继续轮询，不误报生成成功。

### 8.2 验证

基于验收取得的 `usage_limit` 原始事件形状进行本地受控重放，覆盖错误先于 ID、ID 后出错、部分输出后出错、末帧无空行、正常结束、未知错误、不误判消息正文、搜索上限，以及 HTTP 200/429 轮询失败。

代理 EventHub 用例覆盖非流式文本、流式文本、搜索各自有/无部分结果：均保留错误提示和会话，仅执行一次；不刷新、不移除账号、不切号重投；准确反馈一次原模型 `rate_limit` 失败。没有故意耗尽真实账号额度或轮换线上凭据。

完整 `go test ./...`、受影响包 `go test -race` 与 `go vet ./...` 均通过。收口后的真实账号最小正向验证同样通过：

| 账号 | 链路 | 结果 |
| --- | --- | --- |
| `alexg@mulife.vip` | 新解析读取完整搜索 SSE 后再 poll | 成功，`gpt-6`，10.215 秒，答案包含 `Asia/Singapore`，返回 13 条来源；会话 `6ae18572-4780-83ec-a255-ea56b7459957` |
| `alexg@mulife.vip` | 独立 Client/Cookie Jar 普通文本 | 成功，8.745 秒，正文 `SSE_OK`，实际 HTTP 200 |

这两次最小验证直接运行本地新代码上游客户端，保持同账号设备值；不经过 x800 部署的代理，不修改其账号库、服务或配置。真实限额故障没有再次人为制造，限额分支依据上述受控回放结论。非凭据日志：`/tmp/aetherrelay-sse-closure-live-20261028/live.log`；完整自动化日志：`/tmp/aetherrelay-sse-closure-{full-test,race,vet}.log`。临时源码、账号数据库副本及密钥在验证后清理。

## 9. 全部未提交修改对 Codex 号池的影响复核

本节保留收口前的复核结论；9.3 的设备值异常启动问题已按后续用户要求修复，最终行为见第 10 节。

### 9.1 结论与范围

以 `6cbc042` 为基线，复核当前全部未提交的生产代码及新增文件。正常启动后，未发现本轮 Web 浏览器一致性、设备身份及 SSE 错误收口直接改变 Codex 上游通讯、账号选择、凭据刷新、并发、重试或冷却行为。不能据此声称两个 Provider 在整个进程生命周期内完全隔离：新增的 Web 启动迁移有一项条件性影响，见 9.3。

本次只执行本地源码审查和自动化验证；没有访问或修改 x800 服务、配置、账号库，也没有使用真实 Codex OAuth 凭据请求上游。此前 `alexg@mulife.vip` 的实测属于 Web，不能当作 Codex 实测证据。本次仅补充隔离回归测试和验收记录，未更改生产实现。

### 9.2 直接链路与共享边界

| 核对项 | 证据与判断 |
| --- | --- |
| HTTP / WebSocket / TLS | Codex 使用 `codexupstream/biz` 的独立 `newHTTPClient`、`newWebsocketDialer` 和 `/backend-api/codex/responses`；Web 使用自己的 TLS Client。Codex 依赖图没有 Web 模块及其客户端，没有改写全局 Transport。 |
| 身份与请求头 | Codex 的 ClientIdentity、Originator、ChatGPT-Account-ID、会话与指纹生成未修改。Web 的 Chrome 133、`oai-device-id`、语言及时区 helper 位于 Web 客户端内部，没有写入 `time.Local` 或全局身份状态。共享 fhttp 依赖版本未变。 |
| 错误与恢复 | 新 SSE parser 仅在 Web 文本、图片、搜索中调用；Codex 原有 Responses 事件解析、429/401 分类、限额恢复时间和“发送后不重放”边界未修改。 |
| 号池状态 | Web 与 Codex 的命令分别使用 `aetherrelay.chatgpt.accountpool.*` 与 `aetherrelay.codex.accountpool.*`。设备 ID 不进入 Codex Acquire/Refresh/RecordResult 合同；Codex 并发配置和租约未变。 |
| 共享数据库 | 同一 DuckDB 连接上的加密记录分别使用 `chatgpt_web_accounts` 与 `codex_oauth_accounts`，主键为 `(scope,id)`；Web 迁移、反馈及删除的 SQL 均限定作用域。新增测试故意使用相同账号 ID 和邮箱，确认 Codex 密文、位置和更新时间逐项不变。 |
| 共享代理与事件通道 | 普通 Codex HTTP 请求直接调用 `CompleteCodexResponses` / `StreamCodexResponses`，其账号和上游事件走独立 lane。新增受控测试阻塞 Web 搜索功能事件，确认 Codex 仍在 Web 搜索结束前完成 acquire、执行、release 和成功结算。此测试验证调度隔离，不代表真实上游并发性能验收。 |
| 管理操作与资源 | Web 搜索功能事件和部分 Codex 管理事件仍共享 proxy owner 默认 lane，原本就可能排队；本轮完整消费 Web 搜索 SSE，可能改变该次占用时长。共享进程、后台任务和数据库也仍有资源竞争，不应将功能隔离描述为资源硬隔离。 |
| 模型目录 | `discovery.go` 的本轮修改只为 Web 列表调用传递 DeviceID；Codex discovery/usage 和目录合并算法未改。若 Web 的可用候选变化，已有的同名模型优先级/回退规则仍可能选到不同 Provider；不等同于 Codex 上游实现被更改。 |

### 9.3 已确认的条件性启动影响

`chatgptaccountpool/internal/store/store.go` 的 `Open` 现在在加载后立即调用 `saveLocked`，为旧 Web 账号持久化设备 ID；`saveEncryptedLocked` 对非空设备值要求规范 UUIDv4。

本地注入一条 schema 合法但 `web_device_id=malformed-device` 的 Web 记录，`Open` 明确返回 `initialize account device identity: ... invalid device identity`。同库 Codex 记录未被修改。由 `chatgptaccountpool/biz.New` → Module.Setup → framework service startup → `internal/services/aetherrelay/entry.go` 的错误传播可确认，此错误会使整个服务启动退出，连带阻断 Codex 访问，而非只使这一条 Web 账号不可用。该账号模块初始化也未按 Web Provider 的路由开关跳过。

首次旧记录迁移还新增了写事务；该事务失败同样会导致启动失败。空池或设备值已初始化、序列化后无需变更的池，不执行额外 upsert；不能将此风险描述为每次启动都会批量重写全部账号。数据库原本就要求可写，但首次设备迁移增加了一个必须成功的写入步骤。

建议后续收口将“单条 Web 设备身份异常”限制在该账号或 Web Provider 内，并明确返回诊断，避免阻断健康 Codex Provider；不得通过每次请求随机生成设备 ID、静默轮换有效设备身份或泄露凭据规避。共享存储整体故障则应保持明确报错，不能假定仍可安全运行。此处记录建议，未擅自修改本轮生产实现。

### 9.4 本地验证

- 无缓存执行 Codex accountpool/upstream 全部测试，通过。
- 无缓存执行 proxy Biz、HTTP handler 和 effectivecatalog 全部测试，通过，包含 HTTP/SSE、WS 回退、发送后不重放、限额/冷却、OAuth 刷新、模型发现及用量同步的既有用例。
- 对上述包及 Web store 执行 `go test -race -count=1`，全部通过；新增边界用例单独执行 race 检查，同样通过。
- 新增测试所在的 Web store 与 proxy Biz 包 `go vet` 通过，`git diff --check` 通过。
- 新增 `TestWebDeviceMigrationAndFeedbackPreserveCodexDocuments`：旧记录正常迁移及非法设备 ID 失败分支；共享数据库中相同 ID 的 Codex 记录保持原样。
- 新增 `TestCodexCompletesWhileWebSearchHoldsProxyFeatureLane`：Web 功能事件阻塞期间 Codex 普通请求仍可完成。

日志：`/tmp/aetherrelay-codex-isolation-blocks-test.log`、`/tmp/aetherrelay-codex-isolation-proxy-test.log`、`/tmp/aetherrelay-codex-isolation-race.log`、`/tmp/aetherrelay-codex-isolation-boundaries.log`、`/tmp/aetherrelay-codex-isolation-vet.log`；依赖图：`/tmp/aetherrelay-codex-dependencies.txt`。临时日志不保证长期保留。

## 10. Web 设备身份异常隔离收口

针对 9.3 的单账号数据异常新增本地隔离，不改 Codex 上游、号池、框架生命周期或配置：

- 缺失/空字符串仍在首次保存时生成并持久化 UUIDv4；正常设备身份不轮换。非法 UUID、非规范 UUID、非字符串和 `null` 原值保留，不自动修复或生成替代值。
- 单账号异常不再使 Web store 初始化失败。通过派生状态将该账号视为异常；人工禁用状态保持禁用。原始持久状态与设备值保留，更新凭据、修改状态均不能绕过身份检查。
- 异常账号不参与普通或指定账号的文本/图片调度、图片恢复、模型发现及可用目录，也不参与定时/显式 Web 信息刷新、自动 OAuth 续期和密码重新登录。
- 管理合同新增 `device_identity_error=invalid_device_identity`，管理页统一槽位和 Web 列表显示“设备身份异常 · 已停止调度”；提供中英文诊断，不暴露原始设备值。启动日志记录账号 ID 和错误分类。
- 异常账号仍可进行管理更新、普通凭据导出和删除；其存在不会使健康账号的持久化变更失败。存储打开、解密、schema、事务写入等真实故障继续明确返回错误，不回退到无持久化身份。

永久回归用例已调整为“异常账号隔离但启动成功”，并覆盖健康账号的文本/图片调度、指定异常账号拒绝、目录/发现过滤、手工刷新过滤、启停后的持续限制、错误类型原值保存、重启后保持、Codex 作用域不变以及管理页诊断。

本轮验证日志：`/tmp/aetherrelay-device-quarantine-{test,store-test,full-test,race,ui-test,vet}.log`。只在本地修改和验证，没有部署、提交或修改 x800 配置。共享代理管理 lane 排队属于 9.2 记录的既有边界，本次未扩展为调度架构改造。

无缓存全量 `go test -count=1 ./...`、全量 `go vet ./...`、65 项管理页 Node 测试、构建和差异检查均通过。Web 号池、Codex 号池和 proxy Biz 的 race 测试通过。首次与全量构建并行运行 race 时，未修改的 Codex modeltrace `oversize` 用例在约 2.07 秒结束，未取得期望的协议错误；其测试设定的采样预算为 2 秒。单独连续重跑该测试 3 次通过，再重跑整个 `codexupstream/...` race 通过，未修改 Codex 生产代码或该测试预算。补充日志：`/tmp/aetherrelay-device-quarantine-codex-{sample-recheck,race-recheck}.log`、`/tmp/aetherrelay-device-quarantine-build.log`。
