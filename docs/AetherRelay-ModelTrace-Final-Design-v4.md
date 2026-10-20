> 2026-10-20 集成决策更新：直接替换原“检测上游能力”，入口改名“检测模型指纹”，不新增功能集页面。直接复制 PoC v0.2.0 算法与资产到 internal/pkg/modeltrace；独立 application/modeldetection Module 拥有挑战/任务/评分，Proxy 提供严格账号采样端口。首版采用 Pages 中文三挑战及 CoreCompatible（与下文严格 JSON 固定300 profile 分开）。报告内存保留30分钟；长期历史、路径校准及质量确认仍属后续。当前合同以 docs/design/codex-proxy-maintenance-contract.md 18.0.0、operations.md 和实现测试为准。

# AetherRelay 上游模型质量监测：完整实施设计（PoC优先）

**版本：v4.0 完整设计基线（基于本次最新源码核对）**\
**日期：2026-10-20；远端HEAD再次核对至06:42 UTC（14:42 +08:00）**\
**范围：Codex OAuth 上游账号，先实现单次 Responses 任务；不是通用 ChatGPT Web/Agent 全能力验证。**\
**状态：源码与资产核对完成；PoC及产品实现待开发，未运行测试或真实账号请求。**

**产品定位：AetherRelay功能集中的一个可选功能项「上游模型质量监测」，不是独立产品或平台重写。**

## 0. 设计结论

以 ModelTrace 为轻量行为变化传感器，建设“生产元数据观测 → 独立指纹哨兵与独立客观巡检 → 异常确认实验 → 可审计告警”的持续运行系统。

**不采用：每次生产请求插入随机数提示、克隆用户上下文、用指纹最高候选直接判定降智、自动禁用账号。**

目标不是猜测真实后端身份，而是判断：

> 指定账号、模型请求、reasoning、执行配置，在指定时间窗和任务分布上的交付质量，是否相对合格参考存在超过实际影响阈值的下降。

ModelTrace 回答“行为分布是否变化”；客观任务回答“任务是否做得更差”；对照与历史基线回答“相对谁、下降多少”；证据归档回答“为什么得出这个结论”。四者不可替代。

### 0.1 最终架构决策

1. 从人工实验工具改为可部署的低优先级后台监测服务，但默认配置关闭，管理员显式启用。
2. 指纹和客观巡检并行，指纹正常不能跳过质量审计。
3. 异常只触发确认排队，不能直接触发质量结论或生产路由变更。
4. 不混用生命周期、模型声明、可用性、指纹和质量状态。
5. 给出固定的初版统计协议，而不是统计方法菜单；同时公开其保守性和成本。
6. 归档按真实上游传输记录，凭据脱敏不受生产归档开关影响。
7. 保留真实 quota 和凭据事实更新，隔离的是质量判定对生产健康/路由的影响。
8. 明确区分固定源码已有合同与拟新增合同，先用独立PoC验证收益，再接入功能集与持续监测。

## 0.2 本次源码基线与核对结论

本轮通过git取得远端默认分支HEAD并浅克隆，只读核对实际实现；不是仅阅读仓库说明。最新指**本次查询时的main HEAD**，不代表之后仓库不会变化。

| 项目 | 固定提交 | 提交时间 |
|---|---|---|
| AetherRelay | `ce4bfc8824b0cb942187e36f78703c7ce1702a3d` | 2026-10-19 16:41:46 +08:00 |
| ModelTrace | `6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2` | 2026-10-09 15:48:16 +08:00 |

AetherRelay当前提交主题为`fix(codex): observe account slots and retry lease releases`；go.mod声明Go 1.26.0/toolchain go1.26.7。Codex首要维护合同当前为17.7.0。任何改动涉及其身份、执行、归档或重试语义，先同步维护合同与验收，再改代码。

### 已核实、应直接复用

- `codexaccountpool/pkg/events/steering.go`已有`ProbeAdmissionCommand/Result`，支持严格目标预检、Acquire和已有LeaseID复查，不自动换号。
- `codexaccountpool/pkg/events/contract.go`已有`AcquireResult`、`RefreshTokenCommand`、`ReleaseCommand`，不是要重新设计一套账号池。
- `codexupstream/pkg/events/contract.go`已有`Start/Pull/Cancel`、`Complete`及WS命令，可执行原生Responses。
- `codexupstream/pkg/events/dispatch.go`已有`BindCommandLane`，读取和取消使用不同lane；新调用不能漏掉它。
- `proxyapi/biz`已有账号身份策略、真实额度合并及租约释放重试逻辑，适合把严格采集编排保留在该Biz内。
- `web/admin/index.html`已存在“功能集”和chat/search/tasks/images子页，新功能增加`#/features/model-quality`，不另做产品。

### 必须新增或修正

1. 现有Steering引擎使用固定协议报文及专门fallback策略，不是任意Prompt指纹执行器；不能把它改名当ModelTrace采集器。
2. `PullResult.Data`在当前实现中可经SSE展开、缓冲和事件补全，不能一概叫“上游原始事件”。完整原始证据需要转换前的有界观测合同扩展；PoC可以使用明确标记为派生的最终文本。
3. `AcquireResult`包含短时AccessToken/FingerprintSeed。现有受信Biz会瞬时使用，不能声称所有编排都不接触凭据；正确边界是算法、持久化、HTTP输出和管理页面不接触凭据。
4. `resolveCodexFingerprint`、`resolvedCodexClientIdentity`、`mergeCodexUsageHeaders`及释放helper是Proxy私有函数，accountquality不能直接调用。首版通过新增Proxy typed执行端口复用，不跨包引用私有实现。
5. 当前archive Recorder由调用方负责脱敏；不能把通用Recorder本身视为安全边界。
6. 当前schema路径是final create/verify，不是任意自动迁移；新表有明确Owner及升级顺序。
7. 现有功能集的`FeatureExecutor`走通用路由，不满足严格账号绑定，不能复用该转发路径执行本探针。
8. 通用Cancel当前仅移除stream并触发cancel，不等待worker退出；不能当作已排空。PoC-C新增严格流关闭/排空确认，且处理owner stopping后的有界清理入口。
9. Proxy提交Body会被上游Owner强制stream并注入client_metadata，输入digest不等于最终出站digest；必须分开记录。

### ModelTrace固定资产实测

- Guard版本：0.1.7；bank schema：`robust-number-fingerprint-bank`，18个候选模型。
- `assets/unified_bank.json`实际SHA256：`f267d0513aa06981d024ec5394746fceddf44e4e8a75fa6bf8304255f4b32d6d`。
- `scripts/fingerprint-core.mjs`实际SHA256：`83fa5bd611e18f8339122582335123c8ea168ed242298bb31f4e363abeeb6e4a`。
- 两个摘要与同一提交的provenance一致；不把此事实误说为算法准确率或本地路径校准通过。
- Guard先严格验证，再将规范JSON交给core；其单次提交调用传一个输出。core支持批量聚合，PoC批量模式属于显式扩展。
- Guard新探针计数取292≤N<333（即292–332）范围；本文PoC固定N=300便于复现，明确是新测量profile，不冒称完全复现原Guard。

本轮没有执行仓库测试、构建Go PoC或消耗真实账号请求。发现测试文件不等于运行通过。源码、资产核对已完成到本文列明的路径；统计有效性和真实路径能力仍须按PoC门槛验证。

## 0.3 本次最终实施决策

**先独立PoC，后功能集集成，最后持续能力确认。**

- PoC-A：纯Go算法、固定bank、严格解析、离线golden/parity。
- PoC-B：脱敏样本重放、批量统计和本地指纹适用性评估。
- PoC-C：AetherRelay进程内最小管理采集器，通过已有Owner取得真实指定账号样本；导出脱敏样本包，由独立PoC分析。
- 产品M1：功能集“上游模型质量监测”的手动指纹、采样历史及报告。
- 产品M2：后台指纹与客观巡检、合格基线、正式确认和告警。

独立PoC不连接生产DuckDB、不自建OAuth客户端、不把登录token当普通API Key、不自动部署或运行。PoC-C需要新管理端采集代码，但不必先实现完整accountquality调度与统计系统。详细开发任务、CLI和Go迁移门槛见第17章。

## 1. 结论与检测边界

### 1.1 监测主键

```text
account_principal_id + account_generation
+ requested_model_id + reasoning_effort
+ endpoint_profile + observed_transport
+ effective_request_profile_digest
+ suite_version + grader_version
```

账号槽位是管理入口，不是永久身份。槽位换绑账号必须产生新 generation。凭据正常刷新不自动改变账号身份；策略、模型或任务版本变化则重新判断可比性。

每次结论另绑定 baseline_id/version、protocol_version、time_window、source commits。

### 1.2 能回答什么

- 当前账号在冻结任务分布上的表现是否低于同时期参考账号。
- 相同路径的当前表现是否低于自身或参考群体的合格历史表现。
- 多个账号是否出现共同变化。
- 模型声明、请求配置、可用性或指纹是否发生异常。

### 1.3 不能保证什么

- 指纹不能证明真实模型身份，更不能证明所有生产请求的实际模型。
- 独立合成探针对长上下文、工具复杂性、特殊生产任务的代表性有限。
- 账号之间无法随机分配内部服务配置；A/B差异不自动等于账号被针对性降级。
- 无可靠参考时只能报告分数、趋势和证据不足。
- 全部账号与参考同步缓慢变差且基线同步自动更新，会隐藏问题，因此禁止自动把新数据吸入正式基线。
- 检测有延迟和统计功效限制。短暂、稀有或仅特殊请求发生的回退可能漏检。

## 2. 四层运行链路

```text
生产代理请求 ──只读、限量元数据──> L0 PassiveObserver
                                      │
后台定时器 ──> L1 FingerprintSentinel ──┤
后台定时器 ──> L2 ObjectiveCanary ──────┤
管理员复核 ────────────────────────────┤
                                      v
                            TriggerCoalescer
                                      v
                     L3 ConfirmationScheduler
                                      v
        严格槽位租约 → 独立请求 → 安全评分 → 统计决策
                                      v
                  QualityReport + Incident + Admin UI
```

### L0：生产元数据观察

只接收生产路径已允许记录的账号身份代际、模型请求/声明、策略digest、请求完成状态、错误类别、用量完整性、延迟与时间戳。默认不复制用户提示、附件、响应正文、工具内容或secret。

目的：识别配置变化、错误率/截断率变化、声明不一致及监测覆盖缺口。L0不能给生产正文自动评分，也不能单独标记降智。SLO异常归可用性诊断，必要时暂停质量比较。

实现要求：有界非阻塞队列；队列满时丢弃非关键观察并计数，不拖住生产请求。执行档案与证据写入属于质量请求自身链路，不依赖此best-effort队列。

### L1：ModelTrace 指纹哨兵

使用独立的新会话、固定提示和冻结配置，对选定账号发起小批量数字输出采样。不得向生产会话插入提示，也不fork真实用户任务。

输出分成两部分：
- 本地参考分布的漂移分数：主要用于筛查。
- 原ModelTrace候选模型排名：可选解释字段，不是身份断言。

样本解析错误、拒答、截断另行记录，不静默丢弃后宣称匹配。未校准的路径只展示原始特征和候选排名，不发“已识别替换模型”告警。

### L2：客观任务巡检

按计划轮换小批量冻结任务，独立于L1运行。第一版优先数学、结构化规则与无外部依赖的代码任务；无合格沙箱时不启用代码执行。

L2用于发现明显成绩变化及覆盖盲区，不直接复用其数据作被触发的L3确认样本。即使L1长期匹配，L2仍按计划执行。

### L3：正式确认实验

用未参与本次触发的预留任务/新随机样本、冻结配置和合格参考进行确认。按统计协议和预算执行，不因为某一时刻差异“足够显著”就随意停止。

L3是正式质量状态的唯一写入方。指纹、模型声明、usage、错误率只能成为诊断和调度输入。

## 3. 调度、触发和资源策略

### 3.1 初始建议配置

以下数值仅作为小规模部署调参起点，不是已经验证的灵敏度/误报保证。未填预算、无基线或无校准文件时拒绝启用正式确认。

```yaml
account_quality:
  enabled: false
  mode: shadow                 # shadow | alert；首版没有 auto_route
  profiles: []                 # 显式选择 account/model/reasoning，不做全笛卡尔积
  account_probe_concurrency: 1
  global_probe_concurrency: 1
  production_priority: true
  fingerprint:
    interval: 6h
    jitter_fraction: 0.20
    samples_per_batch: 3
    requested_count: 300
    calibration_id: null
  objective_canary:
    interval: 24h
    tasks_per_batch: 8
    suite_id: null
  confirmation:
    protocol_id: bounded-paired-v1
    allow_unregistered_looks: false
    max_active_per_profile: 1
    reference_policy_id: null
  budgets:
    per_profile_daily_requests: null
    global_daily_requests: null
    global_daily_token_reservation: null
    experiment_max_duration: null
    experiment_max_storage_bytes: null
  evidence:
    credential_redaction: mandatory
    retention_days: 14
  routing_impact: false
```

若每日L1为4批、每批3次，L2为8题一次，单profile为约20次合成请求/日，不含参考账号巡检和L3。100个profile即约2000次/日，不一定“轻量”。必须先测量token/延迟/额度成本，再设可承受覆盖范围；ModelTrace采样本身也有成本。

实际下一次执行时间由上次完成、jitter、预算和低优先排队共同决定，不补跑全部积压任务。展示last_success_at、due_at、staleness和跳过原因。

### 3.2 触发规则

- L1：同一profile出现达到本地冻结阈值的漂移；以滚动3个批次中2次作为初始去抖示例。此规则只是排队启发式，正式启用前应校准触发率。
- L2：预注册巡检控制线被触发，或连续任务类别得分异常。控制线只调度确认，不发布正式降智结论。
- L0：配置改变先标不可比较并使旧基线暂停；服务故障先走可用性诊断，不拿失败次数充当能力证据。
- 管理员：可指定profile手动请求确认。
- 周期兜底：即使L1/L2无异常，也按独立预算安排低频L3审计；周期由覆盖目标与成本设定，不保证所有账号每日确认。

### 3.3 合并、退避和公平性

合并键为profile+当前baseline版本+incident窗口。已有排队/执行中的确认只追加原因，不重复建任务。确认完成前不循环触发新的同profile实验。

无预算、参考不可用或生产繁忙：保留待确认标记，延期并显示STALE/PENDING，不能显示正常。按最旧成功检测优先与profile权重轮询，避免高流量账号垄断；冷账号不因零生产流量而永久不测，但受显式profile范围约束。

账号真实额度不足时不得绕过准入；不更换账号来凑齐目标样本。普通质量结论不改healthy/cooldown/priority/enabled；真实认证或额度事实仍按Owner合同处理。

## 4. ModelTrace 的适配与校准

### 4.1 复用范围

移植版本固定的数字特征计算与评分纯函数、严格解析入口及来源校验思想；不移植Hooks自动停工、常驻Codex App Server、真实任务fork或Node运行框架。

原核心包含平滑后的频数特征及有序块特征、标准化与参考向量比较，最后生成库内相对排名。Go移植先用golden fixtures验证与原实现数值一致；保留MIT许可及资产出处。此处不把库内softmax命名为真实身份概率。[M1][M2]

### 4.2 严格输入合同

使用冻结的Guard严格入口语义，而非对任意文本做数字正则提取。记录parser_version、requested_count、actual_count、format、parse_error与响应终态。初版保持其长度容忍策略以便做一致性比较；若调整为必须精确N个，另立parser版本并重校准。[M2]

固定prompt全文、语言、输出上限、reasoning及实际出站profile；fingerprint probe profile与业务objective profile分别存储，不能把两者digest相同当作前提。若目标模型不支持某参数，建立独立profile而不是静默忽略。

### 4.3 双参考层

1. vendor_bank：固定ModelTrace资产，用于候选排名的实验性展示。库的适用路径未知时标UNCALIBRATED。
2. local_bank：在AetherRelay实际直连路径上建立，用于同profile行为漂移。保存训练与独立holdout、时间窗、账号来源、prompt/parser/extractor版本和资产digest。

local_bank不要求首先知道真实物理模型身份；它代表合格时期的行为参考。它不能单独证明能力正常，资格必须由客观成绩支持。

### 4.4 漂移规则

本地库为每个profile冻结特征中心、尺度与预定义距离度量，使用独立holdout确定筛查阈值和触发成本。第一版可采用标准化特征向量到参考中心的距离；阈值按holdout经验分位校准并报告置信限制。不得把阈值更换、反复试阈值的同一数据当独立校准。

不把多个模型候选相似度混成quality_score。无适用库时输出UNCALIBRATED；预期标签不在vendor库时输出EXPECTED_LABEL_UNSUPPORTED；库外真实模型拒识未验证前，不提供确定UNKNOWN_MODEL身份判定。

### 4.5 指纹状态

```text
NOT_TESTED / UNCALIBRATED / MATCHED / DRIFT_SIGNAL
INVALID_SAMPLE / LOW_CONFIDENCE / INCONCLUSIVE
```

字段expected_label_supported单独记录。所有状态只写fingerprint与diagnostic_flags，不写quality_assessment。

## 5. 客观任务与基线

### 5.1 任务合同

每题包括：稳定task_id、suite_version、类别、权重、输入所有角色、附件digest、tools/schema、输出/时间限制、语言运行时、答案提取器版本、grader版本、固定评分规则。

首版主分数均规范到[0,1]。为降低实现歧义，可先使用二值“完整任务通过/不通过”；测试通过比例仅作次指标。类别权重事先冻结，同题多次执行先聚合，不能因重复次数多自动提高权重。

开发集用于选题与调参，巡检集用于筛查，确认集用于正式比较。题集难度须避免全对/全错；新增版本只通过独立桥接建立新基线。防题库泄漏和长期记忆效应，模型会话独立且不携带历史答案。

### 5.2 失败分类与评分

- 正常返回但答案错误、编译失败、schema不符：按任务计0，不删样本。
- 拒答、输出预算耗尽、合法incomplete：按预注册任务规则计分并单列原因；不是任意删除的理由。
- 认证/限流/网络失败：端到端成功率计失败；条件能力比较标缺失，保留全部原因和配对信息。
- 评分器/沙箱基础设施故障：不归责模型，暂停相关任务及正式判定。
- usage缺失：unknown，不记0。

始终同时展示请求可用率、端到端任务成功率、可评分样本成绩、完整配对覆盖率。统计协议决定是否有足够条件作能力结论。

### 5.3 基线资格

baseline生命周期：DRAFT → CALIBRATING → QUALIFIED → SUSPENDED/RETIRED。

资格至少要求：固定版本与配置、独立校准集、足够任务覆盖和估计精度、正常可用性、评分器验收、清晰账号来源及人工审核。人工批准不等于证明模型身份，只代表接受该参考用于指定比较。

不使用“没有告警所以正常”的循环论证，不自动将最近成功样本滚入正式基线。新基线生成新版本；旧版本保留。模型/endpoint/profile/题集/评分器改变先暂停旧基线；未知上游版本保留根因不确定性，不能靠本地版本相同证明服务端不变。

### 5.4 三种比较关系

- CURRENT_REFERENCE_GAP：与合格同时期参考账号相比的差异。
- HISTORICAL_REGRESSION：与合格历史参考相比的表现回退。
- COMMON_SHIFT：多个账号相对历史共同下降的诊断，不自动归因账号或模型厂商。

参考账号自身必须巡检；参考异常时停止把它用于正式判定，不自动切到“当次得分最高”的账号。备用参考须提前登记并独立合格，变更后开启新实验协议。

## 6. 正式统计协议：bounded-paired-v1

本节是拟实现的确定规则，不是已经在真实数据上验证的性能保证。选择保守、容易复算的有界差异界作为首版基线；以后更高效算法必须另立协议版本，独立校准后切换。

### 6.1 估计对象与抽样

冻结题库及抽样概率p_i，和为1。例如编程40%、SQL20%、结构化20%、推理20%，类别内等概率；这是配置示例，实际按用户场景确定。没有代码沙箱时使用另一个不含代码的suite，不能临时删题后仍沿用原基线。

每个区组开始前按p_i抽一题，随机选择AB或BA执行顺序；同题、同出站策略、相邻时间内比较。每对最长间隔写入协议，超时则按缺失合同处理。使用ABBA时先对各账号两次分数平均，每区组仍只有一个差值。

```text
D_j = score_B_j - score_A_j       # D_j ∈ [-1,1]
d_bar = mean(D_j)
```

按p_i抽题后直接取均值，不再乘一次权重。重复抽到同题不增加unique_task_count；报告独立题目覆盖和类别覆盖。它增加的是冻结题库下的重复观测，不是新的任务广度。

为允许运行期变化，估计对象明确定义为本次观察窗口内各区组开始前条件期望的平均值。随机化和版本冻结仍必要；这不宣称存在不随时间变化的“真实能力常数”，也不推断未来表现。

### 6.2 有界下置信界

在预先登记的第n个区组查看点：

```text
r = sqrt(2 * ln(1/alpha_look) / n)
L = d_bar - r
confirm_deficit = (L > delta)
```

D的范围固定为[-1,1]，在有界条件期望/鞅差模型下使用固定查看点的单侧界。禁止将窗口内随机数量的成功样本过滤后代入n；必须使用该查看点全部登记区组及缺失处理结果。

该方法不要求把同题重复伪装成独立题目，但也不能消除公共时间漂移、AB时间差、选择性缺失或基线不可比。可比性、评分器、覆盖与数据完整性门槛先于统计判定。实现文档及测试必须保留上述估计语义，不能泛称“适用于任意相关数据的真实模型能力估计”。

### 6.3 缺失敏感性

对未知分数保留[0,1]区间。确认目标较差时采用最不利于确认的填补：目标A缺失取1，参考B缺失取0；两者都缺失得到D_minus=-1。所有登记区组都计入n，使用D_minus的均值计算损失下界。

这只能说明对未知分数在[0,1]范围内的结论稳健，不能恢复不存在的答案，不能把网络故障归因为模型能力。另设运维门槛：任一方基础设施缺失率>5%、两方缺失差>2个百分点、关键类别缺失或评分器故障时，正式能力结论为INCONCLUSIVE。5%/2%是需要验证的初始政策，不是统计定理。

任务答案错误与格式错误不得伪装成这种缺失；按§5.2计分。只报告共同成功子集的成绩可以作为描述，但不是正式主检验。

### 6.4 全局长期误报账本

首版采用严格、保守的累计预算，不每天重置5%。一个部署的全部纳管账号、模型、reasoning、版本及正式恢复检验共用持久化ledger。

```text
lifetime_alpha_total = 0.05
baseline_interval_budget = 0.01
confirmation_and_recovery_budget = 0.04
alpha_for_hypothesis_k = 0.04 / (k * (k + 1))
```

每个假设登记后冻结，查看点分配10%/20%/70%。当前A/B差异、历史回归、恢复确认是不同假设，分别领取预算。启动/取消/改名/日期变化不返还已消耗额度，也不重置k。正式数据不得来自触发本轮的筛查样本。

这提供长期误报预算的保守控制结构，不保证校准漏洞或评分器故障下仍有效。随着假设数增加，灵敏度会降低，必须显示统计预算和预期成本；以后若改在线FDR等协议，应单独评审，不在同一实验中按结果换算法。

### 6.5 样本量与成本示例

假设delta=0.10，首个正式假设alpha=0.02，查看点为128、512、2048个区组，分配0.002、0.004、0.014。

| 区组数 | 单侧半径r | 观察损失需超过 | AB请求数 |
|---:|---:|---:|---:|
| 128 | 0.3116 | 0.4116 | 256 |
| 512 | 0.1469 | 0.2469 | 1024 |
| 2048 | 0.0646 | 0.1646 | 4096 |

这些数字由公式计算，不是实验结果。确认10个百分点以上损失并不意味着观察到10个百分点就能确认。若真实备择损失Delta1>delta，目标漏检概率beta，保守规划可参考：

```text
n >= 2 * (sqrt(ln(1/alpha)) + sqrt(ln(1/beta)))^2
     / (Delta1 - delta)^2
```

在适用的条件均值假设下，alpha=.014、beta=.20、delta=.10、Delta1=.25给出约989区组的理论充分规划值；仍须映射到已登记查看点、预算和实际缺失/漂移条件。本版三个查看点的下一个合规位置为2048，不允许989时临时查看并宣布结论。

**部署决策：没有数百/数千请求的确认预算时，系统仍可提供低成本异常筛查，但必须标“尚未确认”，不能宣传可靠实时识别10%回退。**

可设单轮最大72小时作为示例时间窗；到期证据不足则INCONCLUSIVE，不能跨配置变更无限积累。频率、窗口和阈值须经预实验校准，不能把示例当所有部署默认承诺。

### 6.6 历史比较

使用相同冻结题库抽样分布和评分器，保存历史样本数与不确定性，不将历史均值当真值。

```text
r_B = sqrt(ln(1/alpha_B) / (2*n_B))
r_C = sqrt(ln(1/alpha_C) / (2*n_C))
L_hist = (mean_baseline - r_B) - (mean_current + r_C)
```

确认历史下降时，历史侧缺失得分保守取0，当前侧缺失得分取1，并保留全部登记样本及运维缺失门槛。历史得分和当前得分均在[0,1]，使用其对应条件期望及适用性合同。基线版本j的单侧区间从0.01/[j(j+1)]领取预算；需要上下两侧则拆分该预算。当前区间从本次正式假设及查看点领取。不得遗漏历史侧误差，也不得选择最高历史均值后只计算选中者的未调整区间。

基线正式估计的数据量、查看点与区间方向必须在采集前冻结，禁止看到基线分数满意就停止。多个查看点须进一步拆分该版本预算，不能每次使用全额alpha_B；同时用于下降和恢复时，应预分配上下侧预算，不能事后在两方向重复使用同一单侧预算。

数据用于参考资格/选择与正式基线估计应分离；各版本区间同时覆盖的预算支持重复使用该冻结基线。历史版本未知、任务/策略不一致时不作同条件因果解释，必要时直接停止正式比较。

### 6.7 最终判定规则

1. 基线资格、请求可比性、评分器、缺失、覆盖、关键证据任一不满足：INCONCLUSIVE。
2. 符合预定查看点且对应损失下界>delta：确认相应范围内任务表现不足。
3. 当前A/B确认称CURRENT_REFERENCE_DEFICIT；历史比较确认称HISTORICAL_PERFORMANCE_REGRESSION。两者分别报告，不把当前差距自动叫历史下降。
4. 未满足确认条件：NO_CONFIRMED_REGRESSION，并披露区间是否仍允许≥delta损失及证据新鲜度。
5. 指纹、模型声明和usage不进入以上计算。

## 7. 正交状态与报告

```text
execution_status:
  QUEUED / RUNNING / COMPLETED / CANCELLED / INTERRUPTED / FAILED
coverage_status:
  FRESH / STALE / GAP
comparability_status:
  VERIFIED_LOCAL_REQUEST / PATH_UNVERIFIED / NOT_COMPARABLE
evidence_status:
  COMPLETE_SANITIZED / PARTIAL / MISSING
quality_assessment:
  NOT_ASSESSED / INCONCLUSIVE / NO_CONFIRMED_REGRESSION / DEGRADED
assessment_basis:
  CURRENT_REFERENCE_DEFICIT / HISTORICAL_PERFORMANCE_REGRESSION
root_cause:
  UNKNOWN                       # 默认，不由指纹或response.model赋值
```

另有fingerprint_status、model_claim_status、availability_status、termination_reason、diagnostic_flags。不设置一个把所有维度合并的“健康分”。

质量报告至少包含：profile、账号代际、suite/grader、baseline版本和资格、实际窗口、protocol和主指标、计划/派发/可评分/配对数、unique tasks、类别覆盖、缺失原因、权重、alpha账本条目、查看点、delta、Delta1、区间方向、证据manifest、上游传输、digest、usage完整性、最新结论时间及有效范围。

展示例（非真实数据）：

```json
{
  "quality_assessment": "INCONCLUSIVE",
  "fingerprint_status": "DRIFT_SIGNAL",
  "coverage_status": "FRESH",
  "confirmation_status": "DEFERRED_BUDGET",
  "diagnostic_flags": ["BEHAVIOR_DISTRIBUTION_CHANGED"],
  "confirmed_degradation": false,
  "root_cause": "UNKNOWN",
  "routing_impact": false
}
```

“confirmed_degradation=false”不代表已经证明正常。旧结论不得覆盖新鲜度不足的事实。

## 8. 基于当前源码的最小侵入集成

### 8.1 选定方案：Proxy执行端口＋Quality业务模块

```text
Admin功能集页面
  → Admin adapter（现有权限与写保护）
  → accountquality（产品阶段：监控/实验/基线/报告Owner）
  → Proxy typed QualityProbe Port（拟新增，严格采集编排）
  → Account Owner：ProbeAdmission / Refresh / Release
  → Upstream Owner：Start / Pull / Cancel
```

PoC-C阶段可由Admin直接向Proxy的有界采集端口提交任务，跳过尚未实现的accountquality产品模块；两者最终复用同一个采集端口。不能走普通FeatureExecutor或生产路由来“碰巧选中”账号。

### 8.2 为什么执行编排保留在Proxy Biz

当前身份派生、客户端身份选择、额度合并、释放重试均在`application/proxyapi/biz`内部。首版新增`pkg/events/quality_probe.go`定义Proxy拥有的typed命令，再在`biz/quality_probe.go`编排，复用同包私有能力；不复制这些规则到PoC或accountquality，不要求全平台重构。

质量算法放纯Go包；后续accountquality只接收脱敏执行结果，不接收`AcquireResult`。如将来确有多模块复用必要，可另行把纯身份策略抽成窄共享包，但不把重构当PoC前提。

### 8.3 当前合同与新端口

**已有合同，名称以固定源码为准：**

- `ProbeAdmissionCommand{AccountID, Model, Acquire, LeaseID, Request}`：预检/严格Acquire/已有租约复查。
- `ProbeAdmissionResult{Allowed, Reason, ConfigVersion, RefreshNeeded, Credential}`。
- `RefreshTokenCommand`携带`ProbeLeaseID`及`ProbeConfigVersion`；刷新发送使用`BindProbeRefreshLane`。
- `ReleaseCommand{LeaseID}`，结合现有checked release与失败重试机制。
- `StartCommand`、`PullCommand`、`CancelCommand`，经`BindCommandLane`发送。

**拟新增Proxy端口，不是现有API：**

```text
QualityProbeStartCommand {
  job_id, account_id, exact_model, reasoning_effort,
  probe_profile_id, request_template_digest,
  deadline, request_budget, output_limit, evidence_policy
}
QualityProbeSnapshotCommand { job_id }
QualityProbeCancelCommand   { job_id }
```

PoC只接收服务端登记的prompt/profile，不开放任意URL、header、脚本或用户上传执行器；以后支持自定义任务时另加校验与权限。job_id由调用方生成，幂等关联，丢失start响应时可以查询/取消而不是盲重发。

### 8.4 HTTP/SSE作为首个采集profile

第一版明确使用已有`StartCommand{PreferWebsocket:false}`的HTTP/SSE路径，不沿用生产WS优先设置，不复用Steering原生/客户端后备流程。真实身份、代理、模型准入仍由Owner解析，不绕过账号政策。完整raw事件出口不是离线PoC前提，但流排空与有效请求证据是live采集前置条件。

执行顺序：预检→预算预留→Acquire→必要的受控刷新→已有租约/配置复查→构造源请求Body→Start及Owner最终请求转换→有界Pull→终态/取消→确认worker排空→真实usage合并→checked Release。每一步记录实际结果与版本，防止获得租约后配置变化。

必须补齐当前通用流合同的两个缺口，之后才允许PoC-C live采集：

- 排空：当前handleCancel/removeStream只删除注册并调用cancel，Pull Done也不提供明确worker退出屏障。增加受控的quality stream close/drain命令或等价完成回执，绑定worker done channel并保留有界幂等状态；执行job进入FINALIZING，只有worker退出且checked Release已确认才释放本地占用。超时保留清理责任及可重试状态。当前stopping时只豁免SteeringClose，必须为新quality清理设计窄例外或明确先于Owner停止的排空顺序，不泛化放开所有生产命令。
- 有效请求：handleStart会forceStream，performURL会注入client_metadata。分别存source_request_digest与owner_effective_request_digest；通过ArchiveFullContent=true取得实际HTTPAttempt.Request.Body并强制脱敏后计算，或新增Owner安全投影/digest合同。不得把Proxy输入Body的hash冒充最终出站hash。

不调用生产选号回退，不静默切模型/Provider；401是否可安全重试必须沿用受控同账号语义并受预算限制。已发送且执行未知时不重放。429结束本样本，不调用普通生产失败反馈来惩罚探针账号。

### 8.5 凭据与版本边界

`AcquireResult`中的短时凭据只能在受信Proxy/Owner调用期间使用，禁止序列化到job、样本、日志或报告。模型core、独立PoC和HTTP adapter永不接收它。`LeaseID`不是认证token，但也不作为对外控制凭据暴露。

现有`ConfigVersion`是运行期执行配置守卫，不等于可跨重启的账号身份代际。正式持久化监测需增加boot/instance epoch以及账号身份/配置代际的明确持久化策略；正常token刷新不被当成账号替换。

### 8.6 产品Module与生命周期

功能稳定后新增`application/accountquality`拥有monitor、baseline、assessment、scheduler与存储。`cmd/aetherrelay/main.go`显式side-effect import；Setup检查依赖与自身schema，Run启动有界循环，BeginShutdown先停止接单并取消，Teardown等待终态写入、租约释放和真实排空后关闭资源。

Proxy只持有有界执行job，不拥有正式基线和统计结论；避免出现两个争夺实验状态的Owner。

### 8.7 两种Fingerprint必须分开命名

当前上游DTO已有`CodexFingerprint`，它是出站客户端/会话身份，不是ModelTrace行为指纹。新增代码建议使用`BehaviorFingerprint`或`ModelTraceAnalysis`；日志和JSON禁止把两者都简写成fingerprint而混淆身份策略与数字行为特征。

### 8.8 生产隔离

质量判定不更新路由、cooldown、账号启停或优先级；真实额度头和必要刷新仍由Owner应用。探测共享额度、并发、网络和磁盘，靠低优先、背压及预算限制，不承诺零消耗。所有样本标`origin=quality_probe`，防止触发自身。

## 9. 证据、安全和资源一致性

### 9.1 真实传输与完整性

分别保存inbound_mode、observed_upstream_transport、允许的原始事件和派生Response。当前`codexupstream/biz/biz.go`的runStream在Pull返回前会展开/缓冲/补全事件；现有PullResult.Data只能按实际处理来源标为owner_normalized/derived，不能直接标upstream_raw。PoC-C可保存最终文本用于算法验证，并声明raw_events_available=false；需要完整原生事件时，另增转换前有界证据出口，保留丢弃计数与背压策略。入站stream=false不代表上游非流式；实际上游SSE仍须归档其真实事件。[A2]

记录event_index、来源、response/item/call关联、本地接收时间、实际终态、丢弃计数、截断和错误。failed/incomplete也是有效终态事实，不伪造completed。Agent工具输出若未来支持，须关联后续客户端输入，不能假定单个Response已含完整工具轨迹。

证据分级：关键身份/有效请求/最终可评分产物/评分来源缺失阻断正式质量结论；非关键诊断事件缺失可保留独立统计结果但必须标PARTIAL，不能声称完整证据链。

### 9.2 强制凭据剔除

质量归档的credential redaction不可被`archive_unredacted_headers`放宽。[A3] 当前Recorder依赖调用方完成脱敏，且生产debug投影在该开关开启时可跳过正文metadata脱敏；新EvidenceStore必须自己构造强制脱敏输入，不能仅换目录复用Recorder。

header、query、metadata、错误、日志、导出均过滤secret；正文按合成题库与管理员数据策略保存。保存的是允许保留的脱敏证据，digest对这些字节计算并绑定redaction_version，不宣称等于原始认证字节。

禁止把用户私有正文自动收集作题库。只保存有限、必要的模型输出；报告下载需管理员授权。限制路径、符号链接、文件大小与解压行为，防工件读取逃逸。

### 9.3 安全评分

不可信代码/SQL进入独立、非特权、默认断网沙箱；限制CPU/内存/进程树/时间/输出/磁盘，固定运行时镜像摘要，不挂载凭据、宿主socket或生产DuckDB。SQL仅访问临时测试库。隐藏答案/评分控制逻辑不可被候选产物修改。

没有经过验收的沙箱就禁用相关任务，不以普通子进程timeout替代。编译阶段也受相同限制。

### 9.4 预算

账号、profile、实例、实验四层预算。指纹、日常客观巡检、确认实验各有保留额度，指纹不得耗尽客观评测预算。

派发前保守预留token；正式usage到达后结算。未知usage暂不释放其保守预留，按明确对账/到期政策处理。取消只证明本地取消，远端可能继续消耗；显示在途最大超额边界。只在端点真实支持时使用reasoning token约束。

### 9.5 持久化与恢复

拟新增表：quality_monitors、quality_episodes、quality_protocols、quality_runs、quality_scores、quality_baselines、quality_artifacts、quality_assessments、quality_alerts、quality_alpha_ledger。

唯一键约束monitor generation、episode+protocol、run+attempt、grade version和alert transition。创建幂等键绑定payload digest，同键不同payload拒绝。

派发前持久化intent；发出后记录receipt。崩溃间隙无法证明未执行则UNKNOWN_EXECUTION，不自动重放。不恢复旧租约；可恢复确定未派发任务和历史查询。涉及不确定派发的确认episode按缺失/中断规则结束，后续新episode用新ID与新预算登记。

工件临时写→校验→原子定稿→登记引用，启动检查孤儿、缺失与损坏。不能沿用生产interaction按round数量删除的保留策略；被基线或未结事件引用的证据应遵循质量档案自己的引用/到期规则。score/report版本只追加，不静默覆盖。启动schema策略遵循当前Owner合同；需要破坏性迁移时另给离线备份/迁移/回滚工具，不假定存在自动ALTER升级。[A3] 当前Documents.initializeSchema发现既有表后先verify再CREATE；不能把quality表直接加入旧verify列表而使升级启动失败。应明确新增Owner表的创建/验证顺序、旧库升级用例与兼容行为。Documents也不是裸SQL连接或任意key store，新增存储必须通过显式Owner接口扩展。

## 10. 告警、恢复与管理页面

### 10.1 告警分类

1. COVERAGE_GAP：检测过期、预算不足或失败。
2. FINGERPRINT_DRIFT：行为分布变化，等待客观复核，不标题写“降智”。
3. OBJECTIVE_SCREEN_SIGNAL：巡检成绩异常，确认中。
4. CONFIRMED_DEFICIT：正式当前参考差距或历史回归，标清依据和范围。
5. RECOVERY_CONFIRMED：新证据符合预注册恢复标准。

默认只写Admin与内部事件；外部通知目的地、权限与内容由管理员明确配置。按episode和状态迁移去重，不能每个探针重复发同一告警。

### 10.2 恢复

不能因下一轮“未显著”就关闭已确认事件。冻结恢复阈值delta_recover，例如小于原确认阈值；使用新数据、正式单侧损失上界U<delta_recover，并从同一全局ledger领取预算。若要求两个分离窗口均通过，也必须预先规定并独立登记。

恢复必须采用与下降相反的最坏缺失方向：目标A缺失取0、参考B缺失取1，形成D_plus；计算U=mean(D_plus)+sqrt(2*ln(1/alpha_look)/n)，不得复用D_minus。历史恢复则历史缺失取1、当前缺失取0，计算U_hist=(mean_baseline+r_B)-(mean_current-r_C)，并领取历史上侧/当前下侧相应预算；不能复用未覆盖该方向的基线区间。所有缺失、证据和可比性门槛仍然适用。

参考仍异常或证据不足时只显示“近期改善/恢复未确认”。恢复是当前表现证据，不删除原历史事件。

### 10.3 页面

主入口复用现有“功能集”，新增子页`#/features/model-quality`。同步增加subnav按钮、hash合法值、面板切换与刷新/返回行为；账号详情仅提供预填账号的快捷跳转。不能仅因页面属于功能集就调用通用FeatureExecutor，它会进入普通候选路由。

页面显示：纳管profile、最后指纹、最近客观巡检、确认状态、结论时间与范围、分母与缺失、预算、下一次计划/延期原因、基线资格和报告链接。

不要只显示“正常/降智”。同时展示“历史确认回退，当前证据已过期”等组合状态，避免旧绿色状态掩盖覆盖缺口。

## 11. API草案

所有路径适配现有Admin Base Path；以下为新设计，不是现有可调用API。

```http
POST /admin/api/account-quality/poc/runs
GET  /admin/api/account-quality/poc/runs/{id}
POST /admin/api/account-quality/poc/runs/{id}/cancel
GET  /admin/api/account-quality/poc/runs/{id}/bundle
POST /admin/api/account-quality/monitors
PATCH /admin/api/account-quality/monitors/{id}
POST /admin/api/account-quality/monitors/{id}/pause
POST /admin/api/account-quality/experiments
POST /admin/api/account-quality/experiments/{id}/cancel
GET  /admin/api/account-quality/experiments/{id}/report
GET  /admin/api/account-quality/experiments/{id}/evidence
GET  /admin/api/account-quality/incidents
GET  /admin/api/account-quality/baselines
POST /admin/api/account-quality/baselines/{id}/qualify
GET  /admin/api/account-quality/budgets
```

PoC端点是拟新增、默认关闭的最小collector入口；产品monitors/baselines端点在后续阶段实现。现有Steering API仅收model，不能拿来传任意prompt或reasoning。所有新写入口复用管理员mutation guard、严格DTO/body限额和Idempotency-Key政策；不复用其固定业务body。

配置写入审计并要求现有管理员授权；qualify必须附独立校准产物及适用范围，不能仅点击按钮把不合格数据变成可信基线。取消幂等，不与完成竞态产生双重终态。

## 12. PoC优先的实施顺序与收口

### P0：独立离线PoC

固定源码与资产、Go严格解析/评分、golden/parity、bundle schema、离线replay。无AetherRelay依赖、无生产数据库、无凭据、无网络默认行为。此阶段是近期优先开发范围，详细任务见第17章。

### P1：真实路径最小采集与适用性验证

在现有AetherRelay进程内增加默认关闭的受控Collector，经Proxy严格执行端口使用Account/Upstream Owner，导出脱敏样本给独立PoC。先HTTP/SSE一个profile，明确normalized证据；独立holdout验证本地指纹稳定性、误报触发率和成本。

不先建设长期调度、质量数据库和告警平台。PoC无收益或不稳定时可停止该辅助路线，客观评测目标不受其绑架。

### P2：功能集最小产品

PoC通过后迁入同一纯Go core及fixtures，加入“功能集→上游模型质量监测”的手动运行、结果、证据与历史。所有指纹结果仍是辅助诊断；先按产品证据要求完成持久化、安全和生命周期。

### P3：后台巡检与基线

纳管profile、公平低优先调度、独立客观任务、合格基线、指纹漂移触发与覆盖显示。校准未完成时只在shadow模式运行，不宣称已确认降智。

### P4：正式能力确认与告警

冻结统计协议、累计误报预算、缺失敏感性与恢复合同，完成模拟及真实受控评测后才开启正式告警。仍不自动改变生产路由。保守统计例子的高请求数不是PoC必跑量，也不是每日日常巡检要求。

每阶段交付清楚区分PASS/FAIL/NOT_RUN。源码核对不等于PoC通过，PoC算法通过不等于真实路径筛查通过，筛查通过不等于能力回退检测通过。

## 13. 强制验收矩阵

- 指纹异常、成绩不变：不改变quality结论。
- 指纹匹配、客观成绩下降：仍可触发并确认。
- 选择性难题超时：不删除后判账号更强。
- ABBA重复：只算一个区组；报告题目覆盖而非虚增广度。
- 模型/策略/题库改变：旧基线不静默继续比较。
- 参考账号下降：报告参考失效/公共变化，不硬归因目标账号。
- 真实损失为0、delta附近、明显超过delta：量化误报和功效，保留适用前提。
- 多profile、重复查看、重启/日切：统计ledger不重置。
- 无预算、队列挤压、冷账号：显示覆盖缺口，不显示正常。
- 429、401、断连、账号替换：不切号，不将未知结果盲重放。
- 非流式入站+SSE上游：事件不丢失、不伪造。
- 生产未脱敏开关开启：质量证据不泄漏凭据。
- 文件/数据库写入任意阶段崩溃：状态可解释，工件不串线。
- 取消与完成竞态：必须确认stream worker退出后再完成租约释放；owner stopping阶段的quality清理仍可有界完成。
- Proxy输入与Owner注入后的请求：两种digest分别记录，不把源Body冒充wire Body。
- 恶意代码/SQL：网络、文件、主库、进程树与输出限制有效。
- 高频异常：合并为单episode，不形成反馈风暴。
- 日常synthetic请求不会被L0再次触发自身。
- 恢复仅“未显著”：不能错误清除已确认事件。

## 14. 最终可交付与宣传边界

交付：固定版本清单、Owner合同、数据字典/API、可复算题库与grader、指纹parity与local_bank校准、统计协议及模拟、资源/安全/恢复测试、影子运行报告、告警与恢复规则、成本/覆盖/检测延迟报告、回滚方案。

只有完成P4及真实验证后，才可说：

> 系统能够在已校准的任务、配置、基线和预算范围内，对运行期交付质量回退进行持续筛查与统计确认。

不能说：

> 已实时识别所有账号降智，或已经准确识别每条请求的真实底层模型。

## 15. 固定源码证据与验证状态

本次已完成git HEAD查询、源码只读核对和ModelTrace资产摘要核验；没有执行仓库测试、构建或真实模型采样。以下链接固定到本次提交，不再使用浮动main作为实施基准。

- [A1 实际架构边界](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/docs/structure.md)
- [A2 运行合同](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/docs/operations.md)
- [A3 配置合同](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/docs/configuration.md)
- [首要维护合同17.7.0](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/docs/design/codex-proxy-maintenance-contract.md#L9)
- [严格准入DTO](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/blocks/codexaccountpool/pkg/events/steering.go#L19-L40)
- [账号Owner实际实现](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/blocks/codexaccountpool/biz/steering.go#L14-L80)
- [上游执行DTO](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/blocks/codexupstream/pkg/events/contract.go#L235-L291)
- [Pull前的流转换](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/blocks/codexupstream/biz/biz.go#L648-L699)
- [通用Cancel当前语义](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/blocks/codexupstream/biz/biz.go#L586-L601)
- [Owner停止期命令限制](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/blocks/codexupstream/biz/dispatch.go#L63-L70)
- [Owner最终请求转换](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/blocks/codexupstream/biz/biz.go#L730-L766)
- [租约释放重试](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/application/proxyapi/biz/codex_leases.go#L47-L80)
- [现有功能集UI](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/web/admin/index.html#L240-L245)
- [FeatureExecutor进入通用路由](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/application/proxyapi/service/proxy/feature_executor.go#L378-L387)
- [Recorder由调用方脱敏](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/pkg/aetherrelayarchive/recorder.go#L99-L105)
- [生产归档投影策略](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/application/proxyapi/service/proxy/debug.go#L424-L510)
- [Schema初始化顺序](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/pkg/aetherrelaystate/documents.go#L211-L243)
- [两阶段停止参考](https://github.com/muidea/AetherRelay/blob/ce4bfc8824b0cb942187e36f78703c7ce1702a3d/internal/modules/application/chatgpttemporarychat/biz/biz.go#L153-L192)
- [M1 评分core](https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/fingerprint-core.mjs)
- [M2 严格解析](https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/probe-output.mjs)
- [M3 Guard实际提交](https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/guard.mjs#L346-L347)
- [M4 provenance](https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/assets/provenance.json)
- [M5 校准评估](https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/evaluate-calibration.mjs)
- [M6 LICENSE](https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/LICENSE)

|事项|本次状态|
|---|---|
|最新默认分支与固定提交|已核对|
|上述实际代码合同与功能集入口|已读取核对|
|指纹资产/core摘要|已计算并比对|
|Go PoC源码/构建/单元测试|NOT_RUN / 待开发|
|JS-Go parity|NOT_RUN|
|真实账号采样、费用和路径稳定性|NOT_RUN|
|指纹holdout、质量评测、统计模拟|NOT_RUN|
|生产集成、监测或告警部署|未执行|

## 16. ModelTrace Go 原生实现规格

### 16.1 技术选择

本功能是AetherRelay功能集中的“上游模型质量监测”，不是独立产品。采用**Go原生移植纯算法 + 固定JSON指纹资产 + AetherRelay现有Owner执行链**。

“复用算法”不是在Go里执行JavaScript、嵌入JS解释器或调用Node sidecar。生产二进制无Node依赖；Node只允许作为开发/CI中固定版本原实现的对照工具。此章节是移植规格，不是已完成的Go实现。

分清两层：
- `modeltrace`：兼容原实现的解析/特征/候选评分，追求算法一致。
- `fingerprintpolicy`：AetherRelay本地库适用性、漂移阈值、触发规则和诊断状态，属于本项目新增策略。

后者不能借原项目名声称已有校准，更不能将原输出字段`probability`直接展示成真实模型概率。

### 16.2 建议目录

```text
internal/modules/application/accountquality/
  biz/
    fingerprint_runner.go       # 调度、租约、上游调用、结果保存
    fingerprint_policy.go       # 适用性、漂移、复核触发
  internal/modeltrace/
    parser.go                  # 严格解析，结构化错误
    bank.go                    # 资产加载及schema/维度验证
    features.go                # 纯特征计算
    scorer.go                  # 原评分语义兼容
    aggregate.go               # 批次聚合、排名及诊断
    types.go
    testdata/                  # 原版oracle生成的固定样例
  internal/assets/modeltrace/
    bank.json
    manifest.json
    LICENSE
```

这是产品集成阶段的建议布局。PoC阶段先在独立Go module中维护同一个纯core及fixtures，经过验收后整体迁入此位置，不复制成两份长期分叉算法。纯算法放accountquality内部可避免让质量业务策略侵入codexupstream；上游Block只执行请求，不负责指纹判定。若以后确有多模块复用，再提升纯包位置，不先制造公共框架。

### 16.3 原实现到Go的映射

| 源职责 | Go职责 | 必须保持的语义 |
|---|---|---|
| `probe-output.mjs`严格入口 | `ValidateSample` | 先验证完整回复，再产生可信整数序列 |
| 频数及Hellinger特征 | `BuildFeatures` | 平滑与归一化顺序保持一致 |
| ordered-block特征 | 同上 | 分块、分桶和尾数信息不可省略 |
| 标准化、投影、候选评分 | `ScoreSample` | 向量顺序和资产参数一致 |
| 批次聚合 | `AnalyzeBatch` | 逐样本评分后等权聚合 |
| 候选排序/相似度 | `FingerprintResult` | 排名、分数、相似度分别保留 |

移植以固定commit的`fingerprint-core.mjs`和`probe-output.mjs`为算法依据，不以README概述重写近似版本。[M1][M2]

固定移植输入包括同一commit的`fingerprint-core.mjs`、`probe-output.mjs`、`assets/unified_bank.json`、`assets/provenance.json`、LICENSE及提示模板。provenance中的scorerSha256校验原JS文件，不是Go代码；额外记录go_port_version、upstream_scorer_sha256和fixture_version。

特别容易出错的地方：混合权重读取`bank.robust.ordered_blocks.weight`，不硬编码导出常量；批次不是先拼接所有数字再评分；实际有效样本数决定原校准参数选择。向量计算使用float64，排序并列时保持原模型顺序。保持355维频数与74维有序特征的维度合同、总体方差除以n、四块余数分配和顺序投影；校准键按原实现将有效样本数封顶至3。

### 16.4 Go接口草案

```go
// 拟新增内部接口；不代表现有仓库已经提供。
type Sample struct {
    RunID         string
    Text          string
    ExpectedCount int
}

type ValidatedSample struct {
    RunID   string
    Numbers []int
}

type Candidate struct {
    ModelID           string
    Score             float64
    ClosedSetWeight   float64 // 不命名为真实身份概率
    ProfileSimilarity float64
}

type FingerprintResult struct {
    BankDigest      string
    AlgorithmVersion string
    ParserVersion   string
    SubmittedCount  int
    AcceptedCount   int
    Candidates      []Candidate
    Diagnostics     []SampleDiagnostic
}

func LoadBank(data []byte, manifest Manifest) (*Bank, error)
func ValidateSample(s Sample) (ValidatedSample, error)
func AnalyzeBatch(samples []Sample, bank *Bank) (FingerprintResult, error)
```

`Manifest`、`Bank`、`SampleDiagnostic`在实现时定义严格类型；上面是语义接口而非可直接编译的完整源码。

生产入口内部只将严格验证通过的整数数组送入数值核心；不对原始回复调用宽松`parseNumbers`，不从解释文字中提取数字，不自动修复坏JSON。

某样本无效时保留run_id和原因，不泄漏原文到错误日志；本批没有有效样本返回结构化INCONCLUSIVE。批次接纳门槛来自已校准policy，不能“3次只成功1次”却按3次校准或隐藏失败率。

Go解析器还须显式检查数组类型和输入EOF，不能把JSON null当作空数组；RE2不支持反向引用，需要手工验证围栏配对。JS文本长度为UTF-16 code units，不等于Go字节长度或rune数量；兼容测试应覆盖这一差异。

纯算法接口不得访问账号池、token、网络、数据库、生产日志或调度器；相同输入和资产应得到确定结果。

### 16.5 JSON资产、校准与生命周期

原有bank可作为兼容测试及实验性候选排名资产；本地直连路径的参考库另外命名、另外校准，不在不兼容时强行填入vendor schema。

```json
{
  "asset_schema_version": "1",
  "upstream_repo": "https://github.com/xqy2006/ModelTrace",
  "source_commit": "6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2",
  "algorithm_version": "<port版本>",
  "parser_version": "<严格入口版本>",
  "bank_sha256": "f267d0513aa06981d024ec5394746fceddf44e4e8a75fa6bf8304255f4b32d6d",
  "probe_profile_digest": "<适用的探针配置摘要>",
  "calibration_id": null,
  "usage": "candidate_ranking_only"
}
```

占位值不是可部署配置；真实摘要必须对取得的资产计算。复制或分发资产前核对LICENSE与库/数据来源，保留必要通知。

加载时校验：文件大小、schema版本、模型ID唯一性、数组长度与模型顺序、特征维度、有限浮点数、尺度非零、质心/基向量形状、校准键覆盖、权重范围及摘要。语法合法不等于资产数值有效。

首版优先固定资产随版本发布，避免运行时自动下载main。更新流程：导入候选资产→离线验证→parity及适用性校准→管理员激活新版本。进行中的批次持有不可变快照，不中途换库；旧报告继续引用旧digest。

vendor bank与local drift bank不是同一个东西：前者回答库内相似排名，后者定义本地正常时期的行为分布。普通Go重写不会自动训练出新库；local bank建设是单独的数据与校准交付项。

### 16.6 AetherRelay实际调用链

```text
到期的FingerprintJob
  → 冻结目标账号代际与探针profile、bank快照
  → 预留预算
  → Proxy采集端口调用Account Owner取得Strict Lease
  → 出站前复查身份与配置
  → Upstream Owner执行独立合成请求
  → EvidenceStore保存脱敏证据与实际终态
  → Extractor提取该探针允许的最终文本
  → Go modeltrace.AnalyzeBatch
  → FingerprintPolicy检查库适用性和本地漂移
  → 保存FingerprintResult + diagnostics
  → 必要时合并触发客观确认任务
```

任何路径都不把分数写入生产健康或路由，不将候选模型名替换成服务端已验证身份。只出现解析失败时报告采样/格式异常，不直接报告模型回退。

数字必须由模型直接输出，不提供本地随机数工具代替生成，也不把原fork探针的上下文声明未经修改复制到独立会话。

Prompt版本、语言、reasoning、采样参数、输出限制、实际传输、解释器/解析器版本均随结果保存。不能只移植算法而忽略生成数字的请求条件；请求条件变了，原指纹库可能不适用。

### 16.7 Go与JavaScript一致性验证

固定原项目commit作为oracle，离线生成测试输入、资产digest和预期结果。CI可以运行原Node纯算法；AetherRelay生产运行不需要Node，也不为parity测试请求真实模型。

测试分三组：

1. 解析兼容：合法数组、单一JSON代码块、边界整数、长度上下界、空数组、说明文字、多个块、截断、负数、小数、指数、非数字、超长和Unicode文本。保留原字符长度与词法检查语义；Go不能把`1.0`或`1e0`宽松接纳为整数。若使用`encoding/json`，结合原始字面量验证或UseNumber，不能仅检查浮点值为整数。
2. 数值parity：单样本/多样本、重复数字、极端分布、分块余数、缺省可选ordered块、多个家族、分数并列、最小尺度等。比较中间特征、候选分数、聚合值、相似度和排名，防最终top-1一致掩盖中间错误。
3. 资产失败：损坏JSON、缺维度、重复ID、错序、零尺度、NaN/Inf来源、错误摘要、缺校准键，必须在执行前拒绝。

浮点容差可从绝对/相对1e-10开始，先以平台与fixture实测验证后冻结；不是承诺所有环境天然满足。近似并列须验证稳定排序规则，不以任意顺序当真实分类差异。任何主动算法改进另立版本，先过兼容实现验收，再验证效果，不混在首版移植里。

生产安全可对未知schema或非有限数值更严格；明确记录为输入校验收紧，不宣称对所有非法输入与JS行为完全相同。

### 16.8 交付与验收拆分

- G1 算法移植完成：纯Go实现、固定资产、严格解析、golden/parity、许可文件。只能声明与固定原实现一致。
- G2 AetherRelay集成完成：严格账号执行、证据关联、预算、异常与取消、管理展示。只能声明探针可审计地运行。
- G3 行为筛查可用：本地路径独立校准、明确适用范围、触发成本和误报评估。可以发行为漂移信号。
- G4 能力回退监测可用：客观巡检、合格基线、独立确认及恢复协议通过验收。才可给指定范围的质量回退结论。

G1/G2成功不等于G3/G4成功。整体功能仍是AetherRelay中的一个可选功能项，而不是引入另一套代理、Agent或调度产品。

## 17. 独立ModelTrace Go PoC开发与验证规格

### 17.1. 已核实的源码基准

2026-10-20约06:36 UTC通过git ls-remote、depth-1 clone及再次ls-remote实际核对：
- ModelTrace HEAD/main：`6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2`。
- Commit时间：`2026-10-09T15:48:16+08:00`，标题“更新haiku 5.5”。
- Guard package版本0.1.7，Node要求>=18；生产Go PoC不依赖Node，Node只用作以后离线oracle。
- 仓库工作树未修改，未运行下载的JS、测试、hook或真实模型请求。

实际计算摘要，且bank/core与provenance声明匹配：
- `assets/unified_bank.json`：`f267d0513aa06981d024ec5394746fceddf44e4e8a75fa6bf8304255f4b32d6d`
- `scripts/fingerprint-core.mjs`：`83fa5bd611e18f8339122582335123c8ea168ed242298bb31f4e363abeeb6e4a`
- `scripts/probe-output.mjs`：`f35e72b5fe82ce5037980d890fbdcfb3979ee31357f195f5fafc1e099a02c5d6`

指纹库schema为`robust-number-fingerprint-bank`，18模型；robust.model_order与models顺序一致；Hellinger特征355维，有序特征74维，当前ordered权重0.25。资产中的交叉验证准确率不能移作AetherRelay实际路径性能保证。provenance的sameContextCalibrated与multilingualCalibrated均为false。

### 17.2. 必须明确的三种验证结论

- ALGORITHM_PARITY：Go对固定源码/资产的数值与解析行为一致。
- PATH_FINGERPRINT_VALIDATED：在AetherRelay真实严格账号采样路径上，已独立验证指纹稳定性和筛查阈值。
- QUALITY_REGRESSION_VALIDATED：客观任务、合格参考、统计协议能够确认指定范围的能力回退。

前两项不自动得到第三项。PoC首先完成第一项，避免先搭建全套后台监测再发现指纹算法/路径不适用。

### 17.3. 推荐PoC交付形态

先建立独立工作区与Go module，不接AetherRelay生产数据库，不新增常驻服务：

```text
modeltrace-go-poc/
  go.mod
  cmd/modeltrace-poc/main.go
  internal/modeltrace/{parser,bank,features,scorer,aggregate,types}.go
  internal/bundle/{schema,validate,redact}.go
  internal/report/
  assets/vendor/modeltrace/<commit>/
    unified_bank.json
    provenance.json
    LICENSE
  manifests/upstream.lock.json
  fixtures/{parser,numeric,invalid-bank,replay}/
  oracle/{README.md,generate-fixtures.mjs}
  schemas/sample-bundle.schema.json
  testdata/golden/
  docs/{protocol,limits,results}.md
```

拟开发CLI，不代表目前已存在：

```sh
modeltrace-poc verify-assets --manifest manifests/upstream.lock.json
modeltrace-poc score --bundle fixtures/replay/example.json --out report.json
modeltrace-poc replay --dataset samples/index.jsonl --out replay-report.json
modeltrace-poc parity --fixtures testdata/golden --out parity-report.json
```

v0.1不提供collect-live，也不提供OAuth登录/刷新。离线命令禁止隐式网络、自动下载资产或运行hook。PoC完成后可将同一个Go纯包迁入AetherRelay，不重新写第二份算法。

### 17.4. 版本清单与资产约束

lock manifest包括：upstream repo、commit、Guard版本、bank/core/parser SHA256、port版本、Go工具链版本、oracle Node版本、fixture版本、prompt版本、输出schema版本。

provenance.scorerSha256用于核对原JS，不用于核对Go源文件。Go包另记录代码版本或构建摘要。优先标准库，不引入数值库改变计算顺序。Node离线oracle固定来源，运行前审查依赖；不执行Guard lifecycle或hook。

Bank加载必须拒绝维度/模型顺序不匹配、重复ID、非有限数、零尺度、负counts、无有效总量、缺校准键及缺启用分支所需结构。一次分析绑定不可变资产快照。

### 17.5. 与实际Guard行为的差异记录

- `guard.mjs:346–347`：先validateNumbers，再JSON.stringify后调用analyzeGlobalOutputs，提交路径每次传一个输出。
- `fingerprint-core.mjs:158–211`支持多输出聚合；因此PoC提供batch评分是复用core支持，不应描述为原Guard每次提交都聚合3条。
- `state.mjs:205–206`新样本数量随机292≤N<333（即292–332），确认沿用原count。PoC建议先固定N=300以便复现，独立记录count_policy；它不等同于完整Guard采样策略。
- 直接上游探针与Guard的ephemeral fork是不同测量条件。新prompt不能复制“你在原任务分叉”之类不真实声明。
- 数字必须由模型直接输出；本地RNG生成的数据只能用于标记为synthetic_fixture的数值测试，不能冒充live_model_sample。

### 17.6. 离线验证路线

#### POC-0：来源与schema
固定commit，校验摘要、许可证与资产结构。任何失败返回ASSET_INVALID，停止评分；不能更新到未知main修复。

#### POC-1：解析和数值parity
严格验证器与原oracle比对：数组类型/EOF、围栏配对、UTF-16长度、字面整数、1..355范围、count区间及错误诊断。

数值fixtures覆盖1/2/3/4条样本、分块余数、ordered分支缺失/关闭/开启、零/近零范数、极端分布、分数并列和多家族。先逐样本评分再等权平均；校准键使用min(valid_count,3)。最终候选顺序、完整分数向量、softmax和profile_similarity都比较。

建议绝对/相对1e-10作为起点，实测跨平台行为后冻结；近似并列须记录并稳定排序。纯函数私有中间量可由审查过的oracle测试wrapper导出，不修改上游源资产摘要，也不宣称wrapper原本属于上游。

#### POC-2：真实证据离线replay
导入脱敏采样bundle，验证实际目标、请求配置、终态、文本、digest与资产匹配。不能只有手工粘贴整数而无采样来源就声称验证了实际账号路径。

### 17.7. Codex真实路径采样：最小集成collector，而非独立PoC直连账号

独立Go进程不能跨进程调用EventHub，也不能同时打开生产DuckDB获取凭据、复刻OAuth生命周期或将Codex登录token当API key。

建议在AetherRelay已有进程内做**显式启用、管理员限定、预算封顶的最小Collector**（这是后续新增代码，不是已存在API）：

Admin请求 → collector编排 → 账号Owner严格槽位租约/刷新 → 上游Owner独立采样 → 脱敏工件 → 管理员导出bundle → standalone PoC离线评分。

Collector不提供任意endpoint、任意token或任意代码执行；只接受固定probe profile、account slot、模型、reasoning、样本数及硬性预算。样本失败不切号、不切模型，不盲重放未知结果。是否可复用既有strict execution入口由AetherRelay源码合同决定；若现有Steering入口仅允许固定WS任务，就必须补窄合同，不能把它当通用Prompt执行口。

此路线复用真正Owner与账号代理路径，避免PoC的成功只说明另一条路径能请求。对通用官方API-key端点可以将来另做实验adapter，但其证据标签必须是OTHER_ENDPOINT，不可用于认证Codex OAuth路径。

当前用户要求设计与PoC规划，不应在未取得账号、预算和执行范围前发live请求。

### 17.8. Sample bundle最低字段

```text
schema_version, collection_id, collected_at
source_kind: synthetic_fixture | sanitized_live_capture
collector_commit, profile_digest, prompt_version, count_policy
account_principal_ref, account_generation, requested_model, reasoning
endpoint_profile, observed_transport, actual_request_digest
request_started_at, terminal_status, remote_outcome
response_model_claim, output_text, expected_count, actual_count
usage, usage_complete, archive_complete, dropped_event_count
redaction_version, content_sha256, artifact_references
```

不导出Authorization、cookie、access/refresh token、客户正文、内部密钥或完整凭据存储。账号用本地稳定匿名引用；不将response.model当真实身份标签。所需响应正文仅为合成探针输出。导出工件加大小上限/路径校验。

### 17.9. 实际路径适用性验证

固定语言、prompt、count策略、model/effort、出站策略及传输。训练/阈值选择与holdout分开；按账号和时间窗分组，防相邻同批样本泄漏到两边。先报告有效率、同路径重复稳定性、跨配置敏感性、成本和未识别覆盖；无真实身份锚点时不得输出模型识别准确率。

可用受控端点/明确请求不同模型作为对照，但它支持的只是该控制条件下区分能力，不证明生产请求真实后端身份。公开bank仅可候选排名；local drift bank的构建/阈值属于新增实验，不能把provenance的CV准确率移植成live误报率。

### 17.10. 结果矩阵与集成门槛

|阶段结果|允许结论|后续动作|
|资产或parity失败|移植未通过|停止live；修正解析/算法/版本|
|parity通过，live未跑|Go兼容实现通过|可准备Collector，不宣布检测有效|
|live有效率低/不稳定|当前路径不适用或证据不足|保持PoC，调整独立profile并重新验证|
|live稳定但无独立holdout|有初步可用信号|继续校准，不发布可靠身份/质量结论|
|路径holdout通过|行为筛查在指定范围可用|集成可选fingerprint功能，只触发客观复核|
|客观评测与统计也通过|指定条件质量回退可确认|可启用正式质量告警，仍不自动路由|

交付必须分别列PASS/FAIL/NOT_RUN。源码阅读与摘要核验不算parity通过；本次只完成读取与摘要核验，没有执行PoC、JS测试或真实模型验证。

### 17.11 固定提交证据链接

- https://github.com/xqy2006/ModelTrace/tree/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2
- https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/guard.mjs#L18-L29
- https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/guard.mjs#L346-L347
- https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/fingerprint-core.mjs#L158-L211
- https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/probe-output.mjs#L12-L40
- https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/state.mjs#L205-L206
- https://github.com/xqy2006/ModelTrace/blob/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/codex-plugin/modeltrace-guard/scripts/evaluate-calibration.mjs#L5-L32

## 18. 最终执行清单与下一步

立即可以开发的范围是PoC-A/B：固定资产、纯Go parser/core、golden oracle、离线CLI、脱敏bundle及报告。无需先改生产代理、开数据库表或建长期调度。

PoC-C是后续受控代码集成：新增Proxy采集端口与管理员限定Collector。必须指定测试账号/profile、次数/额度、输出限制和允许时间窗后才发真实请求；本设计交付不代表这些运行参数已经获批。

验收以三个独立问题收口：Go是否与原算法一致；指纹在本地真实路径是否稳定有用；客观任务是否能确认指定范围的质量回退。任何一层失败，报告其真实状态，不用下一层的漂亮界面掩盖。

本文件为基于本次固定源码的完整设计交付，不是已实现功能声明。后续开发按P0→P4推进，功能入口始终是AetherRelay“功能集”中的一个可选功能项。
