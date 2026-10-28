# x800 Codex 输出后中断核对（2026-10-28）

事件 `4f1916ec3717637d3bdd4a953ec1693e`，Request ID `b4696b5ef9c5deb5e352af8f0889dc93`，归档轮次 `57582`。客户端 `codex-owner`，槽位 `d5e3f15b-9275-4f3a-9f5b-8cb3d1df0029`，账号 `athanmomiy@gmail.com`，模型 `gpt-6.1-sol`。

本次核对在本地 Web 设备身份隔离收口及回归后进行。只读读取 x800 容器日志、部署信息、配置中的非凭据参数及交互元数据；没有修改服务、配置或账号，没有重启、部署或重放真实请求。本地未提交修改没有部署到 x800，不能归因为本轮本地修改。

## 已确认的失败位置

客户端使用 HTTP `/v1/responses` 流式接口，AetherRelay 上游使用 WebSocket `wss://chatgpt.com/backend-api/codex/responses`。握手返回 101，生成请求已发送，未进行 HTTP 传输回退。上游累计观测到 203 个事件、123,054 字节，响应模型与请求模型一致；没有收到可识别的终止事件，读取阶段以 `network`、`transport_reason=unknown` 结束。

| 时间（UTC+8） | 观测 |
| --- | --- |
| 13:12:11.117 | 收到客户端请求 |
| 13:12:11.292 | 发起 WS 上游 attempt |
| 13:12:11.590 | 上游握手完成，297 毫秒 |
| 13:12:42.395 | usage 记录的首个实际输出；客户端 HTTP 200 于约 42.402 提交 |
| 13:12:42.782 | 最后一个已观测上游事件 |
| 13:12:43.796 | `Codex attempt failed`，`phase=pull`，没有终止事件 |
| 13:12:43.816～43.838 | usage/归档分别完成结算，outcome 为 `upstream_truncated` |

上游读取观测耗时 32,187 毫秒，首个可交付业务事件等待 31,156 毫秒。首个上游事件在响应头后的约 255 毫秒出现；该前置事件与首个业务输出使用不同计时口径，不能把 31 秒解释为握手慢或连接完全没有数据。

## 相邻请求证据

对轮次 `57540..57630` 的三个 Codex 客户端归档元数据进行有界核对，得到：

| 客户端 | 成功 | `upstream_truncated` |
| --- | ---: | ---: |
| codex-owner | 50 | 1 |
| codex-office | 3 | 0 |
| codex-ubuntu | 37 | 0 |

这是归档轮次范围的样本，不代表全天故障率。其中同槽位、同模型的成功记录分别为 13、3、21 条；失败为本事件一条。

- 前一次 `codex-owner` 同槽位调用：`427a23990b12c658e5154925ad4a257d`，13:12:04.762 开始，13:12:09.491 成功。
- 期间 `codex-ubuntu` 同槽位调用：`00281e667e82a8e696d5fd634641d564`，13:12:12.118 开始，13:12:25.262 成功。
- 后一次 `codex-owner` 同槽位调用：`1c379f26558e45b8fdf5fa1e5c5057bc`，13:12:45.605 开始，13:13:18.986 成功，耗时约 33.4 秒。

这些记录不支持账号持续失效、模型不可用或约 30 秒统一截断的解释，也不能单独证明具体是哪一跳发生异常。

## 现有证据无法确认的底层原因

该轮归档 `full_content_enabled=false`，没有 `response.sse` 正文。运行日志和上游响应元数据只保存了 `Codex upstream network failed` 及 `unknown`，没有原始 WS 错误类型、关闭码或消息长度。

源码中 `responsesWebsocketBody.Read` 会直接返回 `ReadMessage` 错误，也可能返回 `normalizeResponsesWSEvent` 的 JSON 解析错误；通用传输分类器不识别 WS `CloseError`、`ErrReadLimit` 和 JSON 错误。这些不同原因可能落到相同 `network/unknown`。因此这里只能确认“上游 WS 读取/帧处理链路在输出后异常结束”，不能断言已证明物理网络断连或远端主动关闭。

配置的请求、首业务事件及流空闲超时均为 300 秒；本轮耗时约 32.7 秒且错误非 timeout，没有证据指向这些超时。客户端入站正文 1,121,392 字节，上游生成消息正文 1,121,121 字节；请求体超过 1 MiB 不能证明触发接收消息大小限制，因为接收限制针对上游单条 WS 消息。本次没有收到的异常帧大小记录，不能据此要求增大限制。

## 页面字段的解释

- 上游 101 表示 WS 握手成功；客户端 200 表示 AetherRelay 已开始交付 SSE。二者都不代表生成最终完成。
- `部分输出=true`、`upstream_truncated` 与缺失终止事件一致；输出后不自动切号或改用 HTTP 重放，`retryable=false` 是避免重复生成的现有边界。
- Token、缓存用量未知，是没有取得结束阶段用量数据的结果；不是已证实的零 Token 或缓存故障。
- `ignored_features` 中的 turn metadata 字段是代理既有的元数据归一化记录。此轮已开始实际输出，没有证据证明这些字段的省略造成后续异常。
- 当前代码对输出后的网络类异常记录 availability-neutral 反馈，不据此将健康账号或模型拉入冷却；本事件后同槽位成功调用与此边界一致。

## 后续建议

保持已输出后不自动重放、现有超时和消息限制。优先补充有界 WS 诊断：关闭码、异常来源（WS 读/JSON 解析）、消息超限标记及相关消息字节计数；保留这些事实到上游 attempt 归档与运行日志，并分别覆盖正常关闭但缺终止事件、异常关闭、消息超限、非法 JSON 的自动化测试。无需为取得关闭码打开完整正文归档，也不需要采集账号凭据。

上述为最初核对时的建议，当时没有修改 Codex 实现。远端证据位于 `/home/workspace/deploy/data/interactions/codex-owner/057582/`；本地运行日志片段为 `/tmp/aetherrelay-x800-event-4f1916ec-runtime.log`。核对时容器为 `ghcr.io/muidea/aetherrelay:latest`，启动时间 `2026-10-27T16:14:24.097105014Z`。

## 后续本地代码收口

用户授权后补齐 WS 失败诊断，覆盖 HTTP 完整响应、HTTP 流式响应和原生 WebSocket。未连接真实上游，没有修改、重启或部署 x800 服务及配置。

| 字段 | 含义 |
| --- | --- |
| `transport_reason` | 增加 websocket_close / websocket_message_limit / websocket_invalid_json / websocket_read_error / websocket_write_error；既有结构化网络、超时及取消原因继续保留 |
| `websocket_failure_stage` | read / decode / write |
| `websocket_error_type` | close、message_limit、json_syntax、json_type、json_invalid、network、timeout、canceled、read_error、write_error 等固定枚举 |
| `websocket_close_code` | 库提供的关闭码；1006/1015 是本地合成码，不能视为远端实际发送 |
| `websocket_close_received` | 是否收到远端 Close；1005 表示空 Close，1006/1015 为 false |
| `websocket_message_limit_bytes` | 握手后实际设置的接收消息上限 |
| `websocket_last_message_observed` / `websocket_last_message_bytes` | 是否成功读取过消息及最后一条完整 WS 消息长度；不代表被拒绝的超限消息大小 |
| `websocket_json_error_offset` | JSON 语法或类型错误提供的偏移量；不保存错误原文或消息正文 |

诊断通过 typed observation 传递到失败日志及最新/编号 attempt 元数据归档，不依赖完整正文开关。冻结首个故障，清理取消与迟到回调不覆盖原始诊断；合法终态后的连接关闭不污染已完成回合。现有错误分类、账号反馈、自动重试/回退、身份、超时、消息大小限制和客户端错误 envelope 保持原有语义，输出后仍不重放。

回归还发现原生 WS 读循环退出后立即从注册表移除，会使尚未消费的队列失去按 session ID 读取的入口。现改为保留到最终 Done 被消费、显式关闭或停机，放弃消费时最多 30 秒自动回收；保留项仍占原容量，避免无界积累。活跃多回合连接的 response.completed 不触发会话回收，HTTP fallback 不走此流程。首次非法 JSON 观测可携带一次请求归档，后续增量不会重复扩大正文内存。

本次新增诊断不能追溯补齐历史轮次 57582 的缺失事实，仍不能断言其具体关闭码或底层原因。管理页本轮不新增字段，详细事实通过运行日志和 attempt 归档查看。

### 本地验证结果

- 三条执行路径各覆盖成功终结、拼接 JSON、1000 关闭但缺少终态、1011 关闭、本地合成 1006、消息超限和非法 JSON，共 21 个场景；普通业务错误分类及原生非法 JSON 转发策略保持原状。
- 额外覆盖真实本地 WS 读取取消与写入失败、首个诊断冻结、迟到清理不覆盖、下一回合重置、已完成回合隔离、正文不逐增量复制，以及延迟 Pull、消费终态、放弃消费超时回收、显式关闭、停机和旧定时器隔离。
- 代理层覆盖输出后无切号重放、原账号中性反馈、迟到 Cancel 不覆盖原因；归档关闭完整正文时保留相同 attempt 诊断，最新及编号文件一致；客户端错误 envelope 保持不变。
- `go test -p 1 -count=1 ./...`、相关 transport/Codex upstream/proxy biz/proxy handler 包的 `go test -race -p 1 -count=1`、新增 WS 边界场景的补充竞态回归、`go vet -p 1 ./...`、`go build -p 1 ./...`、`git diff --check` 均通过。测试日志见本地 `/tmp/aetherrelay-ws-diagnostics-*.log`。

以上是离线与本地模拟上游验收，不代表已在 x800 部署或对真实 Codex 上游重放验证。
