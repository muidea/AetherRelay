# x800 Codex → Anthropic 响应转换核查与收口方案

日期：2026-10-16。状态：调查、代码修复和本地验收完成，待上线验收。

对象：`root@x800.muidea.com:1212`，部署目录 `/home/workspace/deploy`，容器 `deploy_aetherrelay_1`。调查时镜像 revision 与本地基线均为 `dc8514c750eb1c0159065e1899843a6577ebf219`；本次修复是该基线上的工作区变更，未部署。本文时间除特别说明外均为北京时间。

## 1. 结论与证据边界

这批故障有三个已经复现的实现缺口：

1. Responses → Anthropic SSE 转换器拒绝正常出现的 `response.metadata`，解释全部 8 次流式转换异常。
2. 非流式转换器拒绝 `message.phase=commentary`。现存真实消息逐项回放和真实 HTTP handler 均复现转换 502，是 22 次历史非流式失败的明确候选触发点。
3. Provider 健康统计把 HTTP 502 的本地 `conversion` 当作上游失败；连续三次会开启 30 秒熔断。即使 `streamFail.CountUpstream=false`，这条统计路径仍会发生，进而影响同 Provider 的原生 Responses 请求。

22 次历史失败没有保存转换前的完成响应，错误包装也没有输出底层字段位置。因此可以逐条确认其失败阶段，但不能把这 22 次全部归因为 commentary；同样不能认为只放行 `response.metadata` 就解决了它们。修复验收必须补齐这一证据链。

## 2. 历史 30 次逐条核对

远端证据根目录：`/home/workspace/deploy/data/interactions`。

| 分组 | 身份与 round | 每条均成立的事实 |
| --- | --- | --- |
| 非流式 502，13 次 | `claude-owner/{000003,000004,000005,000044,000048,000053,000087,000088,000092,000097,000099,000113,000119}` | `anthropic_to_codex_responses`；上游 200；上游终态 `response.completed`；下游 `conversion_response_error` |
| 非流式 502，9 次 | `claude-office/{000033,000037,000039,000040,000046,000052,000058,000068,000075}` | 同上 |
| 流式异常，7 次 | `claude-owner/{000001,000002,000042,000067,000074,000080,000084}` | 下游已经提交 200；业务结果 `conversion`；上游错误归档精确为 `converted SSE: responses stream event "response.metadata"` |
| 流式异常，1 次 | `claude-office/000027` | 同上 |

合计非流式 22 次，流式 8 次。

样例：

- `claude-owner/000119`：11:01:37，下游 502；`upstream_response_001.json` 记录上游 200、`response.completed`；Request ID `27eba5938555d9d0a0463336cd689cad`。
- `claude-owner/000080`：10:55:09，上游观察到 `response.completed`，但转换侧先因 `response.metadata` 失败；不能因为上游读取任务已经观察到终态而把下游流记为成功。
- 22 次非流式失败中，部分已有完整请求归档，包括 `000097/000099/000113/000119`；但其 `response.json` 是发给客户端的错误信封，不是上游完成对象。
- 当前 `upstream_response[_001].json` 仅记录状态、header 和读取观测。开启完整正文归档并不自动保存成功上游的完成对象。
- 30 次异常的调用明细均为输入/输出 Token 零；转换器在字段失败时也会返回空 usage。这些零不能证明上游未产生用量，完成响应中已有计数时应独立保留。

## 3. 离线复现与最小线上验证

诊断文件位于本机 `/tmp/aetherrelay-conversion-audit.dW0YYI/`；初次调查通过 Go overlay 加载临时测试，当时生产代码未改动。以下 3.1–3.3 为修复前复现结果，修复后的离线回放见第 7 节。

### 3.1 response.metadata

从同机成功的原生 `/v1/responses` 归档提取 53 份完成响应及其 metadata 事件。53 份均在 `response.completed` 之前出现一次 `response.metadata`，转换器对 53 份事件全部报相同错误。

实际结构为：`type`、`sequence_number`、`response_id`、`metadata.openai_chatgpt_moderation_metadata`。它是附加元信息事件，原生路径正常消费；它本身不构成生成完成或工具调用。

代码定位：`responses_anthropic.go` 的 `responsesEventToAnthropicWithCapabilityState` 没有该 case，进入 default 拒绝分支（约 2260 行）。上游正常读取和非流式汇聚不依赖这个 mapper，因此与非流式 22 次错误不是同一直接触发点。

### 3.2 commentary

对上述样本中的真实 message 项逐项调用当前转换器：10 个 `commentary` 全部报 `output.message.phase`；4 个 `final_answer` 全部通过。将真实 commentary 完成对象作为 executor 结果交给 `/v1/messages` 的真实 handler，可复现 HTTP 502 和完全相同的 `conversion_response_error` 信封。

代码定位：`responses_anthropic.go:497` 只接纳 `phase=final_answer`；`responses_anthropic_test.go` 现有测试明确要求拒绝 commentary。因此这是当前既定转换边界与 Codex 实际响应形态的冲突，需要先更新有界合同，不能仅删掉一个判断。

53 个完整原生结果中，48 个含 `custom_tool_call`，超出本次 Anthropic 平面函数工具范围，未将它们当作 Messages 应当支持的样本。剩余 5 个完整结果为 4 个通过、1 个先因 commentary 失败；对失败样本做单变量排除后，还出现真实非空 `function_call.namespace=clock` 的拒绝。这是独立的命名空间语义问题。

为核对平面工具是否也必然出现 namespace，额外发送了一次小型合成原生请求：模型 `gpt-6.1-sol`，工具 `audit_echo(value)`，不执行任何工具。Request ID 为 `x800-conversion-audit-751012e62a7c4b57977db8df98e8cdb7`，远端归档为 `claude-owner/000210`。结果 HTTP 200，返回普通 function_call、不含 namespace；用原转换器回放通过。这次调用产生少量真实 Token 用量，不是历史会话重放，也不证明 22 次历史响应的具体字段。

### 3.3 熔断放大与诊断缺失

对 Registry 连续记录三次 `status=502,outcome=conversion`，得到 `failures=3,consecutive=3,circuit=open`；记录同样业务结果但 `status=200` 时不增加健康失败。原因为 `shouldTrackProviderHealth` 在排除本地 outcome 时遗漏 `conversion`，随后命中 `status>=500`。

现有 `TestProviderHealthIgnoresLocalClientAndConversionErrors` 仅覆盖 conversion/400，未覆盖响应转换的 502，因而现有测试通过也不能保护这条故障链。

`codex_messages.go:119` 将 `convertErr` 放入 `streamFail.Err`，但 `streamFail.Error()` 优先返回通用 Message。普通日志、客户端错误和 metadata 最终只保留通用说明，`conversion_error_path` 也没有在这个错误分支设置。

所有调查测试与选取的现有相关测试通过。这里的“通过”表示成功复现并断言当前缺陷，不表示修复已实现。

### 3.4 官方 Codex 源码对照

按用户提供的 `openai/codex` 仓库核对 revision `e790ce271749ce431e446a9e2cc66485fe60f931`：

- [`codex-api/src/sse/responses.rs`](https://github.com/openai/codex/blob/e790ce271749ce431e446a9e2cc66485fe60f931/codex-rs/codex-api/src/sse/responses.rs#L160) 将 metadata 定义为可选 JSON，而不是固定的审核结果 schema。`response.metadata` 还可能包含 `openai_verification_recommendation`、`metadata.type=safety_buffering`，并可携带 headers 中的 turn-state 和实际模型。CLI 先提取内部观测事件，主响应事件分支不把 metadata 当作正文或完成信号。
- 现场 53 份 metadata 仅观察到 `openai_chatgpt_moderation_metadata`，不能据此断言该事件只有审核信息。样本中的 `prompt`、`generation`、`tool_call`、`tool_response` 是审核阶段分组；内部分类代码不作无依据的业务解释。
- [`protocol/src/models.rs`](https://github.com/openai/codex/blob/e790ce271749ce431e446a9e2cc66485fe60f931/codex-rs/protocol/src/models.rs#L955) 将 commentary 定义为轮次中途的助手文字，后面可继续输出工具或文字。官方还定义 partial_answer，但现场仅见 commentary/final_answer；本次仍限定这两种非空 phase，未将所有新 phase 自动放行。
- 先前提供的 `microsoft/Codex-CLI` revision `331667b708ead5f209c2796ea4398c940a011a8b` 为 2022 年 GPT-3 Codex 补全示例，其 `openai.Completion.create` 不涉及本次 Responses 协议。

## 4. 实施范围

实施前已更新 [Codex 首要维护合同](codex-proxy-maintenance-contract.md) 和 [双向转换设计](responses-anthropic-conversion.md)。以下能力现已在本地实现，生产验收单独记录。

| 工作项 | 处理规则 | 主要位置 |
| --- | --- | --- |
| 已知元信息事件 | 在 Codex→Anthropic 映射中显式消费 `response.metadata`；不生成正文、不改变完成状态、不把它作为成功终态；记录有界忽略项。其它未知事件仍明确失败。 | `codex_anthropic_projection.go`、`codex_messages.go` |
| commentary 映射 | 在 Codex 响应投影范围接纳 absent、commentary、final_answer；正文映射为 Anthropic text 并按输出顺序保留，与工具调用一起返回；工具调用仍返回 tool_use stop reason。未知 phase 和错误类型明确失败。 | `responses_anthropic.go`、`codex_messages.go` |
| 本地失败隔离 | 在 HTTP 状态分支之前排除 `outcome=conversion` 对 Provider 健康、失败次数、熔断的影响；保留请求错误、转换指标和 usage 结算。不得伪记成功来清除真实故障。 | `aetherrelaymetrics/registry.go` |
| 具体错误定位 | 记录响应侧 path、event_type、phase、mode、request_id/event_id；底层错误结构化传递到日志和 metadata。客户端保持现有错误 code。 | `codex_messages.go`、`stream_result.go`、归档 metadata 接线 |
| 转换前正文留存 | 非流式在投影前按现有完整正文归档开关保存 `result.Body`，名称如 `upstream_response_001.body.json`，明确它是汇聚后的 Responses 完成对象；流式需要时保存 mapper 收到的 Responses SSE，并明确记录层次、预算与截断状态。 | `codex_messages.go`、`debug.go`、归档组件 |
| 已完成上游的 usage | 本地投影失败时，若完成对象已经携带有效 usage，独立提取真实计数后结算；缺失仍保持未知，不伪造零或成功。 | 转换返回值、usage 接线 |

commentary 不是 reasoning，不能套用 reasoning_output 的省略规则，也不能删掉文字或伪改为 final_answer。通用 Provider 目前明确拒绝 commentary，现已用独立 Codex 投影入口限定新行为；通用路径仍按原合同验收。

namespace 不在本次故障主修复范围。不能为了通过转换随意删除非空 namespace 或拼接工具名；未来支持必须按实际请求工具定义做无歧义映射。此次平面工具 probe 已通过，且历史完整出站请求的工具定义没有 namespace。

## 5. 验收矩阵

| 层次 | 必测输入 | 通过标准 |
| --- | --- | --- |
| Metadata 状态机 | metadata 出现在 created 后、文本后、工具后及终态前；metadata 后 EOF；未知事件；终态后事件 | 正常 metadata 不报错、不触发完成；缺终态和未知事件仍失败；错误后不伪造 message_stop |
| 非流式投影 | commentary-only、commentary+工具、commentary+final_answer、并行工具、仅工具、reasoning+commentary、未知 phase | 已声明语义完整保留；文本与工具保序；stop_reason 正确；未知语义明确拒绝 |
| 流式/非流式一致性 | 同一 Responses 语义的两种输入，包含 metadata、commentary、工具参数 done 和 usage | 输出文本、工具身份/参数、终止原因和用量一致；done 参数不重复；文本不重复或遗漏 |
| 负向诊断 | 未知输出字段、未知事件、畸形工具参数 | 明确记录失败 stage/path/type；客户端 code 保持稳定；归档可直接离线回放 |
| 健康隔离 | 连续三次 conversion/502、conversion/200、已有真实熔断后追加 conversion | 不新增/延长/清除真实熔断；原生 Responses 不因本地转换失败被拒绝；真实 upstream/502 仍可熔断 |
| 用量 | 成功上游后投影失败，usage 有值/缺失/显式零；流式错误终态 | HTTP 与 outcome 如实记录；已知 Token 不丢失；无重复结算 |
| 兼容边界 | 通用 Provider、原生 Responses、非空 namespace、custom_tool_call、未知 phase | 保持各自合同，未声明的映射不静默放行 |

至少加入真实 `/v1/messages` handler 级回归，不能只测试 mapper。使用当前临时样本建立最小固定 fixture；正式 fixture 应保留导致失败的字段和事件顺序。

候选实现完成后运行相关 Go 测试、race，以及仓库要求的 `make check-codex`。离线通过后再以固定镜像 revision 进行线上小流量验收：普通文本、工具调用、工具结果续轮各覆盖 stream=true/false，检查完整 Anthropic 终态、工具语义、usage 和归档完成对象。之后在真实 Claude 工具会话中观察连续至少 20 轮，期间同时验证原生 Responses 调用；真实上游过载单独统计。

## 6. 完成标准与发布顺序

先实现诊断留存和健康隔离，再实现 metadata 与 commentary 的有界映射，完成离线回放后作为一组候选发布。实施过程中如新的响应原文显示其它字段导致失败，应以新证据扩充合同和回归，不能直接扩大通用字段白名单。

只有线上 Messages 两种模式均通过语义验收、conversion 不再污染熔断、失败能定位到具体字段且可回放，才将本项标为收口。原先 22 次历史请求保留“转换阶段已确认、具体字段证据缺失”的结论，不回填猜测。

本次已修改本地转换实现及回归测试，未修改生产配置、重启或部署；生产故障是否消除仍待部署验收。

## 7. 本地实现与验证结果

新增 `codex_anthropic_projection.go`，Codex 非流式转换接纳 commentary 并保留其文字。Codex SSE 在复用既有验证与用量解析的同时按文本、工具、后续文本分配递增 block index；并行工具按添加顺序输出，等待完成较晚的前序工具；参数 done 与 delta 不重复输出。只有完整终态对象的流可投影其内容；已有增量输出与终态正文不一致则明确失败，不伪造成功终态。

转换错误携带 stage/path/event_type/phase/detail，日志关联 request_id 与原有 event_id。完成对象中的真实 usage 在投影失败时仍结算，缓存计数显式零和缺失保持区别。完整正文开关控制转换前 JSON/SSE 的落盘，stream 采用 64 MiB 上限，仅保留完整帧前缀，metadata 记录格式和截断标志；这些文件是 adapter 输入，不能称为原始线上传输。

永久回归位于 `codex_anthropic_projection_test.go` 和 `aetherrelaymetrics/conversion_health_test.go`，覆盖：

- 真实 handler 流式/非流式成功语义一致；commentary、并行工具逆序完成、参数 done、后续文字；工具参数大整数不丢精度、不重复。
- metadata 多处插入、metadata 后 EOF、未知事件/phase、namespace、终态后事件及终态内容不一致。失败后返回错误而非伪造 message_stop。
- 投影失败 usage 为非零、显式零、缺失，以及归档开关两种状态；错误位置和普通日志正文边界。
- 归档完整帧预算、截断状态及文件类型；stop prefix 在工具后的正确 text block 中刷新。
- conversion/200、400、502、503 不新增/延长/清除健康熔断；真实 upstream/502 仍可熔断；真实 handler 连续三次本地 502 后原生 Responses 仍可访问。

修复后通过临时 overlay 再次回放现场样本，结果存于本机 `closure-replay.log`：53/53 metadata 被消费且不产生正文/完成状态；10/10 commentary 与 4/4 final_answer 消息投影成功；平面工具 probe 的实际完成对象投影成功。53 个原生完整对象中，原可转换的 4 个仍通过，48 个 custom_tool_call 与 1 个 namespace 边界仍明确拒绝；未把原生工具形态全量放行。22 条历史失败的具体字段仍无法追溯，不宣称这批历史请求已全部成功回放。

本地检查全部通过：相关分包测试、`go test ./...`、`make check-codex`（含相关 race、32 项前端合同测试、`go vet ./...`、diff 检查和凭据扫描），以及 metrics/archive 的额外 race。尚未执行修复版本的线上 smoke 或真实 Claude 连续工具会话验收。

## 8. 17.3.1 output item 附带信息收口与 live 验收

现场新证据：Event `0f733a6913ea1e29855f4e07b4e64f16`，`claude-owner/000440`，2026-10-16T05:06:52.898561Z，执行账号 `muidea@gmail.com`。运行镜像已是 `f8cf1634`；`Edit` function_call 的两个附带字段都包含同一 turn_id。流式预校验首先拒绝 internal_chat_message_metadata_passthrough，仅去掉该字段又拒绝 metadata，去掉两者后已有三帧可通过。实际客户端仅有 message_start 和 api_error，未取得工具输出、完成事件或最终 usage。原始归档前缀通过不能证明整个上游请求成功。

本次实现：

- 共享 output item 归一化和字段规则，流式 added/done、完整终态和非流式复用；两个明确附带字段在独立副本中省略，容器内部扩展透明兼容，原文留在归档。忽略项只有固定字段分类，不增加动态维度或逐字段日志。
- 工具参数允许 added 空值，保留累计预算、完成 JSON 校验和身份校验；done 参数与已收增量不一致时明确失败，格式空白或对象键顺序变化允许。未知业务字段、类型及 phase 保持拒绝。
- 缓冲标记独立于输出耗时和用量落库，内存、DuckDB 重开、明细和 CSV 保持一致，不创建虚假输出时间、Token 或 TPS。现有附属表即可承载，无 schema 变更、不回填历史。
- 固定回归覆盖真实 Edit added 字段形态，以及合成的参数增量、done-only、item-done-only 和 terminal-only 完整流程；handler 覆盖文字/工具保序、流式与非流式内容及用量一致、附带信息原始归档保真且不进入客户端正文。合成后续帧明确不作为 live 证据。

### Live 验证步骤

1. 发布候选时记录固定 commit/image revision，并核对实际容器运行的 image ID 与 revision。确认 `archive_interactions=true`、`archive_full_content=true`，以便比对转换前后的内容。
2. 通过 `claude-owner` 的 `/v1/messages` 调用 `gpt-6.1-sol`，分别覆盖 stream=true/false：普通文字、强制 Edit 工具调用、工具结果续轮。记录每轮 Event ID、归档目录、实际账号/槽位和模型。若实际账号不是 muidea@gmail.com，应明确它不构成相同账号的复验。
3. 工具调用请求可使用下列正文，将 stream 分别设为 true/false。这里只生成工具调用，验证端无须写入真实文件；从响应提取实际 tool_use.id 和完整 assistant content，再用 tool_result 续轮，返回固定结果 `LIVE_EDIT_OK`，要求模型回复 `LIVE_OK`。续轮将 tool_choice 改为 `{"type":"none"}`，避免继续强制 Edit，也分别覆盖两种模式。

```json
{
  "model": "gpt-6.1-sol",
  "stream": true,
  "max_tokens": 1024,
  "messages": [{"role": "user", "content": "这是协议工具调用验证。请调用 Edit，将 /tmp/aetherrelay-live.txt 的 BEFORE 改为 AFTER。"}],
  "tools": [{"name": "Edit", "description": "协议验证工具，返回固定验证结果。", "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}, "old_string": {"type": "string"}, "new_string": {"type": "string"}}, "required": ["file_path", "old_string", "new_string"]}}],
  "tool_choice": {"type": "tool", "name": "Edit"}
}
```

4. 对工具轮检查：响应只有一次对应 tool_use，名称/调用 ID/参数正确且与上游一致；stop_reason=tool_use；流式有完整帧和唯一 message_stop；无 error；最终 outcome=success。续轮须携带原 assistant content 和对应 tool_result，不复用不同请求的 call ID。最终 usage 按实际上游值记录。
5. 检查转换前归档是否实际包含两个附带字段，且 ignored_features 有对应固定分类、客户端内容不泄漏附带信息。若上游该轮没有携带字段，只能确认普通工具链路成功，不能记为现场故障形态已复现并验证修复。
6. 对缓冲上游，核对归档 GenerationBuffered、页面“上游明确缓冲”及 CSV 一致。若出现缺终态/本地转换失败，HTTP200 仍须显示失败，usage 未取得保持未知，不纳入观测 TPS 样本。`first_event_duration_ms` 是转换入口首次交付观测，上游读取首事件耗时与客户端首次交付耗时在 attempt 归档/日志中分别核对，不能把它解释为模型首 Token 耗时。
7. 在真实 Claude 工具会话中继续观察至少 20 轮，期间抽查原生 Responses，并按字段路径核对 conversion 错误及 Provider 健康；出现未知业务语义时保留失败与归档，按证据扩展适配规则。

本次范围是代码与离线验收准备；本节不代表已发布或已完成 live 验收。

本次本地验收已通过：`go test ./...`、`make check-codex`（包含相关 race、32 项前端测试、vet、diff 和凭据检查）、`go test -race ./internal/pkg/aetherrelayusage`。另用现场原始 `upstream_response_001.projection.sse` 离线回放，三帧均无需移除字段即可转换，两个忽略项均记录，metadata 未进入客户端内容，缺少 completed 时未伪造完成。完整后续生命周期仍以固定合成回归验证，最终线上结论以本节 live 验收为准。

## 9. 17.5.2 WS timing 及全接口收口

### 现场证据

运行版本 `dec0efad51ffa66af7af25e407e38a662219028b`，容器 healthy，restart=0、OOM=false。首批 54 个完成归档中，13 个 Responses 流式与 6 个 Messages 非流式成功；32 个 Messages 流式均因 `responsesapi.websocket_timing` 转换失败，另有 3 个启动期账号冷却 503。503 与本地转换故障分别归因。

| Event ID | 归档 | 结果 |
|---|---|---|
| `30bf53df4a1546a9ef2be32e00e0b488` | `claude-office/002278` | 上游 101，握手 220ms，总耗时 4.045s，本地 conversion |
| `47c4b7f3e9bb82f5c45ba34c13c11b52` | `claude-owner/002298` | 上游 101，握手 177ms，总耗时 4.933s，本地 conversion |

两个请求均由 `muidea@gmail.com` 的槽位 `b7f48e2b-c128-49c9-b8da-7af08566b450` 执行 `gpt-6.1-sol`，各归档 71 个转换前事件。顺序包含 rate_limits、Codex metadata、created、in_progress、Edit 工具调用、62 个参数 delta、arguments done、output item done、response metadata 和 websocket timing；在 timing 转换失败处停止，未取得正式 completed/usage。客户端已交付工具参数，随后 Anthropic error，没有 message_stop。工具参数完成不证明客户端执行过 Edit。GET 握手与 WS create 是两个归档尝试，并非重复生成；没有发生 HTTP 回退。

官方 Codex 快照 `6ae8b8aa7c608730d4315d59c5c081897ae02780` 的 `responses_websocket.rs` 对该事件记录 trace 后继续处理，`client_websockets.rs` 有 created → timing → completed 回归。实际 timing 包括 logical_turn scope、response_id、engine 时间/计数和可扩展 critical_path；engine prompt/sample token 与正式 usage 不能混用。

### 代码边界与回归

- 在现有纯协议包集中四种已知附加事件；转换器省略、固定 ignored feature 去重，原生传输和有界完整正文归档保留。未知业务类型继续拒绝，附加事件内部扩展不误作 output/usage/error。
- Messages 与 Chat 继续读取正式完成和用量。Chat 补齐转换前 SSE 归档、输入字节预算、本地 conversion 分类和失败 error 终态；已发送 create 或输出后保持禁止 HTTP/换号重放。上游失败终态由 typed owner result 决定，不当作本地转换错误。
- 原生 Responses/WS 不再用估算替代缺失 Codex usage；WS 正式用量保留显式零和缓存 presence，遥测不更新续轮 response ID。缺终态明确失败，原生未知业务事件保持透传。
- 输出观测缺失的独立原因是旧用量存储只在 OutputTokensKnown 为真时保存 generation；现改为独立保存有效时间和部分输出，内存、DuckDB 重开、明细、CSV 一致，TPS 汇总仍只使用 Token 已知样本。迟到握手/旧 attempt 不覆盖最终观测，新 attempt 重置。原生 response.done 的计时冻结与 completed 一致。没有 schema 变更，不修补历史。
- 固定测试覆盖三类 HTTP 入口的 12 组流式/非流式文本/工具组合、上游 WS/安全 HTTP 回退与各类终态、仅遥测/空完成/断线、未知业务、失败 typed 分类及 usage、原生 WS 多轮与遥测透传、compact HTTP 压缩项、观测持久化与 TPS 样本隔离。模型目录、input_tokens、鉴权和账号策略由全量合同回归验证，不新增生成路径。

### 部署后的 live 验收

1. 记录新镜像 revision，确认实际容器运行版本、健康和重启计数；本地检查通过不等于 x800 已部署。
2. 对 Messages、Chat、Responses 分别执行流式和非流式文字、Edit 调用及工具结果续轮，记录 Event ID、账号/槽位、模型和每次出站路径；同账号复验单独注明。
3. 抽查实际包含 timing 的上游事件链：工具参数 → output_item.done → metadata/timing → 正式 completed/usage。转换器只记录 websocket_timing 降级项，Messages 唯一 message_stop，Chat 正常 finish_reason/[DONE]，原生 Responses/WS 保留 timing 原文。若该请求未带 timing，只能记作普通链路通过。
4. 原生 WS 连续多轮逐 create 独立结算和归档，核对工具历史、response ID、真实 usage 和账号策略，没有上一轮用量/时间泄漏。
5. 可控测试上游验证握手安全失败只发生一次同账号 HTTP 回退；create 后断线或输出后失败无重放，缺终态仍失败。已有工具参数且无 usage 时应显示首输出时间、正耗时和部分输出，Token/TPS 仍未知。不要以 timing token 补账。
6. compact 继续 HTTP 并含真实 compaction item；models/input_tokens 不发起生成。连续至少 20 轮真实工具会话观察 conversion 复发、账号冷却和 Provider 健康。未知业务事件保留归档并按证据扩展规则。

本节是本地代码与离线验收范围，尚未部署或进行修复版本 live 验证。

本地验收通过：`go test ./...`、`make check-codex`（相关包 race、协议/生成观测/用量存储 race、34 项前端合同、`go vet ./...`、diff 与凭据扫描）。通过临时 overlay 回放上述两个现场 SSE 前缀，各 71 事件全部通过投影并保留一个 Edit 工具块；ignored_features 包含 websocket_timing，未伪造 completed/usage，输出观测为 partial。现场归档没有正式终态，故该回放只证明原失败前缀已兼容，完整成功仍由合成回归和后续 live 分别验证；真实请求及凭据未加入仓库。
