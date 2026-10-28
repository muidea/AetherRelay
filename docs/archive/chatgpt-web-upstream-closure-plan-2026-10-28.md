# ChatGPT Web 浏览器访问一致性完善方案（2026-10-28）

状态：**本轮代码实现、本地检查与独立账号实测通过；未部署到 x800**。

当前 GPT Web 整体功能正常。本方案旨在保留现有功能与通信流程的基础上，适度补充和完善请求，使其尽量符合真实浏览器访问的语义。HAR 中的差异是核对线索，不直接构成缺陷，也不证明调整后能减少 403 或提高执行成功率。功能无回退是首要验收条件。

本次修订取代此前将全部 GW 项目顺序实施的安排：取消跨操作 Cookie/连接共享；请求体字段、账号选择和文件域调整改为有证据后单独确认的候选项。用户随后已授权按本方案代码收口，并允许必要时使用 x800 的 `aleng@mulife.vip` Web 凭据验证；验证期间禁止调整 x800 已部署服务和配置。

依据为用户提供的 HAR、此前 x800 核对记录及 AetherRelay `6cbc042` 源码。后续实施与验证进度见第 14 节；原 HAR 证据保持不变。带日期的方案按目录约定放在 `docs/archive/`，不表示已完成；当前行为仍以[能力设计](../design/chatgpt-web.md)、[配置参考](../configuration.md)和代码为准。

## 1. 已确认边界

| 项目 | 本轮决策 |
| --- | --- |
| 基线 | 当前整体功能正常，以保持文本、搜索、图像、文件及账号刷新能力为前提 |
| UA / Client Hints / TLS | 保留当前 Windows Chrome 133 UA、现有对应提示及 `Chrome_133` TLS 档位，不升级或动态替换 |
| 管理浏览器采集 | 不采集管理页面 UA、Client Hints、ClientHello、HTTP/2 特征 |
| 时区 | 使用新加坡 `Asia/Singapore`，UTC+08:00；按字段语义表达日期、偏移和时区名称 |
| Cookie | 延续每次独立操作新建 Client/Jar；不跨操作共享、不新增 Cookie 落盘；操作内部保留现有 Cookie 传递 |
| Session ID | 保持每次 Client 创建时新建，不改成账号级长期会话 |
| Device ID | 保留随机 UUIDv4，已实施账号级持久化，不复制 HAR 的值 |
| 调用链 | 保留接口顺序、生成路线、重试预算、并发策略与账号状态规则 |
| 本次授权 | 已授权代码收口及必要账号验证；验证过程不得调整 x800 已部署服务与配置 |

“不复用 Cookie”指独立操作之间不复用，不能解释为禁用 Jar、移除 Cookie 或每发一个 HTTP 请求就清空 Cookie。上传、准备、生成、轮询及下载按照现有 Client 边界传递身份；独立恢复任务仍是新的操作，不强行复原旧 Jar。

本轮不修改 thought-flow、不调模型容量/路由/账号并发、不新增真实浏览器运行架构。服务端 HTTP 客户端与真实浏览器仍存在 Cookie 生命周期、TLS、HTTP/2 和运行环境差异，本方案不追求或宣称完整复制浏览器。

## 2. 证据与结论边界

### 2.1 浏览器 HAR

| 项目 | 记录 |
| --- | --- |
| 原始文件 | 用户提供的 `/mnt/d/log.txt`，原文保持不变 |
| 文件大小 | 14,873,852 字节 |
| SHA-256 | `953d10f1e36ec9e4e6b118a63dd6bc38a201db319fb3c5eff7ad53815d0ac753` |
| HAR 条目 | 298 条 |
| 最早请求开始 | `2026-10-28T01:48:31.576Z`，北京时间 09:48:31.576 |
| 最晚请求开始 | `2026-10-28T01:50:44.979Z`，北京时间 09:50:44.979；不是全部请求的结束时刻 |
| 浏览器 | Windows / Microsoft Edge 156 |
| 前端标识 | `Originator: Codex Browser`、`X-OpenAI-Web-Frontend: codex_webview` 等 |
| HTTP 状态 | 200：255；403：11；202：18；101：5；304：2；204：5；504：1；0：1 |

HAR 没有保存任何请求的 Authorization/Cookie 头，`request.cookies` 为空，所有响应 `content.text` 均未保存。这可能与导出设置或浏览器集成方式有关，不能推断浏览器实际未发送认证/Cookie，也不能据此还原完整挑战过程。状态 0 不当作上游实际返回的 HTTP 状态。

11 条 403 均有 `cf-mitigated: challenge` 和 HTML Content-Type；其中 6 组同 URL、同方法的请求随后返回 200，可见请求头及请求体未变。它们证明静态可见头不能完整解释成功/失败切换，不能证明恢复的具体机制。

设备 ID `25a08396-ca1b-46a2-9ba6-6072b486732f` 为 UUIDv4，84 个请求复用该值。此值仅为证据，不得成为项目默认设备 ID。HAR 只能证明样本格式与复用行为，不能证明官方全部生成算法与实现细节。

### 2.2 可对照接口

以下路径均以 `https://chatgpt.com` 为基址；仅按路径匹配不代表查询参数和请求体完全相同。

| 方法与路径 | HAR 成功条目索引（从 0 开始） | 当前用途 |
| --- | --- | --- |
| `GET /` | 0 | bootstrap、接收 Cookie、提取页面资源信息 |
| `GET /backend-api/me` | 55 | 用户信息 |
| `GET /backend-api/accounts/check/v4-2023-04-27` | 41 | 账号与套餐 |
| `GET /backend-api/models` | 279 | 可用模型 |
| `POST /backend-api/conversation/init` | 280 | 初始化与额度信息 |
| `POST /backend-api/sentinel/chat-requirements/prepare` | 96 | Sentinel 准备 |
| `POST /backend-api/sentinel/chat-requirements/finalize` | 155 | Sentinel 完成 |

HAR **没有**实际文本、图像或搜索生成请求，没有生成 SSE，也没有本项目文件上传/结果下载链路的样本。`conversation/init` 不等于模型执行。不能以此 HAR 决定所有生成阶段 token、conduit 或路由规则。

### 2.3 x800 历史记录

此前现场核对保留的 65 条 GPT Web 记录中，47 条成功、17 条本地 400、1 条上游 502，均没有 `upstream_request*.json`。这些历史归档不能重建当时实际发送的 Cookie 和完整出站头，也不能直接作为当前服务可用率分母。

历史 `bootstrap: HTTP 403` 发生在首页阶段；finalize 字段差异位于后续阶段，不能据此认定前者的根因。HAR 中 403 的挑战分类也不能直接回填为所有历史 x800 403 的已知原因。

## 3. 目标与不变量

完善目标是：请求头符合导航/API/文件各自的语义；同一次操作的环境与身份表达一致；只对证据充分的差异做可独立验证的小范围调整。

必须保持：

1. 当前成功链路可继续完成；不为追求 HAR 字面相同而批量替换请求头或请求体。
2. 认证 API 保留所需认证，HAR 缺少 Authorization/Cookie 不是删除依据。
3. 同一次执行尝试内身份与临时状态固定；既有安全重试按新尝试处理，可刷新凭据或安全切换账号并重新准备 token。同账号设备值延续，Session/Jar 按 Client 新建，重试预算不扩大。
4. 不新增自动换设备、换 UA、循环挑战重试；已提交、部分输出或执行状态未知的生成不因本次改造重放。
5. 保留临时对话/搜索的独立消息根、禁用历史标识及本地历史重建规则。
6. 身份持久化不额外清空额度、冷却和模型数据；**保留现有 token 轮换时模型快照失效与重新发现规则**，不把快照永久保留当作迁移要求。
7. 不以统一 Cookie 会话为由改变账号刷新、模型发现、图像恢复等独立操作的生命周期。

## 4. 范围分级与建议批次

保留 GW 编号以便对照旧评审，但不再作为必须全部执行的清单。本轮实施选择和字段清单见第 14 节；未选候选不随本轮启用。

| 编号 | 项目 | 新定位 | 实施条件 |
| --- | --- | --- | --- |
| GW-01 | 对照与验证记录 | 基线支撑 | 优先复用现有日志和受控测试；不默认新建生产原始报文归档 |
| GW-02 | Sentinel finalize 字段 | 待验证候选 | 当前链路正常，取得适用契约与完整准备流程验证后单独确认 |
| GW-03 | 语言和时间表达 | 本轮已实现 | 新加坡环境与完整文本准备/执行链已验证 |
| GW-04 | 分接口请求头 | 本轮已实现所选字段 | 实际字段清单及验证见第 14 节 |
| GW-05 | 设备 ID 持久化 | 本轮已实现 | 账号生命周期、凭据复制/数据库恢复及失败路径已验证；发布回退要求保留 |
| GW-06 | 上游账号/工作区头 | 待验证候选 | 可靠来源、明确作用域和多 workspace 语义齐备 |
| GW-07 | Cookie 与会话边界 | 保持现状的约束 | 不新增共享 Jar、连接池缓存或会话代次管理 |
| GW-08 | 文件域请求头/跳转 | 待补证据，独立评估 | HAR 无文件样本，不与通用头调整捆绑 |

建议步骤：B0 固定现有行为与对照记录；B1 逐项确认并验证 GW-03/GW-04 的适用小改动；B2 单独评审 GW-05。GW-02/GW-06/GW-08 无固定实施排期，证据不足则保持现状，不能因排在清单中就默认实施。

## 5. 请求头与固定环境

### 5.1 按场景对照，不照搬浏览器集成字段

下表是评估方向，具体删除或添加的字段必须在实施前形成清单；不是立即切换的模板。

| 场景 | 评估基线差异 | 完善方向与边界 |
| --- | --- | --- |
| 首页导航 `GET /` | 公共构造器带 API Bearer、Origin、oai-*；HAR 首页不含这些可见字段 | 评估拆分导航模板、导航 Priority 和 HTML Accept；HAR 凭据缺失不足以证明应删除认证，必须验证 bootstrap 与后续完整链路 |
| JSON GET | 当前统一 Origin、API Accept、缓存及 target 头与 HAR 有差异 | 逐接口核验 Origin、Accept、缓存语义和额外 target 头；保留认证及 Jar 行为 |
| JSON POST | 已有 JSON Content-Type、Origin、Referer | 核验同源 API 语义；不把 HAR 所有集成标记当成通用必需项 |
| SSE 生成 | 本 HAR 无样本 | 保留 SSE Accept、Sentinel、conduit、trace 及现有路由，不批量删除头 |
| 上传与下载 | 本 HAR 无样本，可能涉及跨域与签名 URL | 保留当前可用流程，按 GW-08 单独核验目标域 |

首页 HAR 是带 Referer 的同源再次导航，而 bootstrap 可模拟初次访问；`Sec-Fetch-Site=none` 与 `same-origin` 的差异不能脱离场景判断。缓存字段也不能仅因样本缺少就全部删除。

固定规则：

- 不加入 `Originator: Codex Browser`、`X-OpenAI-Web-Frontend: codex_webview`、Codex 窗口或工作区加入标记。
- 不复制 HAR 的账号 ID、设备 ID、Edge 156 高熵提示、客户端版本或构建号。
- `OAI-Did` 虽与样本设备值一致，其通用适用性未证实，暂不补充。
- 不新增 zstd 声明；Content-Length、编码和 HTTP/2 伪头由实际传输能力决定。
- Session ID 仍按 Client 创建；没有生成样本时不批量改变其发送范围。
- 需要观察 Cookie 时区分构造层头与 Jar/Transport 实际附加结果，不能只用 `Header.Set` 推断出站报文。

### 5.2 语言与时间

评估基线的头以 `zh-CN` 为主，requirements 环境使用 `en-US` 系列，时间有 UTC 字符串拼接 `GMT-0500`，上传另有英文 Accept-Language。它们是语义核验项，不等于已证实导致失败。

本轮以现有中文环境统一表达；时区已确定为新加坡 `Asia/Singapore`，UTC+08:00，全年不实行夏令时。日期字符串必须先将实际时刻转换到新加坡当地时间，再按协议格式输出；若字段包含偏移或时区名称，应与 `GMT+0800` 和新加坡时区语义一致，不能保留 UTC 时钟值仅替换后缀。IANA 时区字段使用 `Asia/Singapore`，具体日期格式仍需按对应字段契约核实。

Unix 时间戳保持同一绝对时刻，不额外加八小时；性能时间按其自身语义处理。若字段采用 JavaScript `getTimezoneOffset()` 约定，新加坡对应 `-480` 分钟；其他偏移字段必须先确认正负号约定。同一准备/执行链使用一致环境，UA/TLS 保持原值。该决策适用于 GPT Web 模拟访问环境，不要求修改宿主机时区或历史审计时间。

HAR accounts/check 的 `OAI-Language=en` 说明接口间可以存在差异，不要求所有语言字段字面完全一致。先明确差异是否合理，再调整实际矛盾。环境修改需与请求头批次分开记录，以便定位回归。

## 6. 设备身份与 Cookie 边界

### 6.1 设备 ID 作用域

| 身份 | 目标作用域 | 生命周期 |
| --- | --- | --- |
| Device ID | 同一持久本地账号记录 | 缺失时生成随机 UUIDv4并持久化，同主体 token 刷新及重启保持 |
| Session ID | 每次 Client | 保持现状，每次创建时生成 |
| Cookie Jar | 每次独立操作的 Client | 保留链内自动接收/发送，操作结束后不供其他操作复用 |
| trace/message ID | 现有逻辑执行/消息 | 保持原语义，不因设备持久化改为账号共享 |

UUIDv4 与样本格式相符，但不能据此宣称完全还原官方生成算法。每账号一个设备是本服务隔离策略，不代表官方浏览器按账号生成设备。

设备持久化不承诺减少挑战；稳定 ID 配合新 Jar 仍不同于持续登录的浏览器。本轮接受这一差异，不为弥补它新增跨操作 Cookie 共享。

### 6.2 初始化与兼容

- 在账号 owner 内原子初始化；并发首次读取获得同一值，持久化成功后才发布。
- 已有设备值不因 token、UA 或重启变化而重置；不能用 token/邮箱/IP 哈希代替随机设备 ID。
- 持久化失败应明确报告，不能静默用另一个随机值继续；该可用性影响须纳入 GW-05 实施确认。
- 非空设备值格式非法或字段类型异常时，只隔离该账号，保留原始数据与明确诊断，不自动轮换，不阻断健康 Web/Codex 账号。缺失或空字符串仍按上述规则初始化；共享数据库故障不降级为无持久化的临时身份。
- 新旧 token 别名沿用现有稳定账号绑定；不根据相同邮箱合并不同主体。
- 身份迁移不额外重置账号状态；现有凭据轮换导致模型快照失效的逻辑保留。
- 不新增自动设备轮换、Cookie 落盘、全局可变 Client 或账号级 Session ID。

### 6.3 已采用的导入、导出与恢复合同

- 普通凭据导出及现有账号 bundle 不携带 `web_device_id`，不增加迁移模式或管理界面。
- 新账号首次持久化时生成本实例设备；同主体凭据更新沿用已有目标设备。
- 删除后普通重导入视作新账号；复制到并行运行实例也生成新设备。
- 完整数据库备份恢复包含加密账号文档，因此保留设备；普通凭据 bundle 不声称恢复源设备。
- 旧账号缺少字段时在启动阶段完成迁移，保存失败则启动明确失败；不会发布未提交的设备值。非法已有设备字段明确报错，不自动重置。
- 上游 `Set-Cookie` 中的设备值按原 Jar 规则保留，不自动写回持久设备、不改写 Cookie，也不新增 `OAI-Did` 头。二者含义未证实相同时，不声称强制一致。

### 6.4 保持现有操作边界

模型发现、账号刷新、文本、搜索、图像等继续独立创建 Client；同一 Client 内 Cookie 按域和属性管理。编辑上传、搜索准备与 bootstrap 的既有顺序保持，不为追求统一而重排。独立图片恢复任务不强制沿用原操作 Cookie。

不新增共享资源键、缓存淘汰、bootstrap single-flight、跨操作 Cookie 版本或共享连接生命周期。现存取消/排空覆盖不足可以另立问题评估，本次不声称已经解决，也不引入依赖它们的共享资源设计。

## 7. 需额外证据的候选项

### 7.1 Sentinel finalize（GW-02）

当前源码使用 `prepare_token`、`proof_token`、`turnstile_token`；成功 HAR 使用 `prepare_token`、`proofofwork`、`turnstile`。该差异有直接证据，但当前链路正常，新字段成功不证明旧别名无效。

本轮保留现有请求体。后续需核实适用流程、字段类型、可选/空值行为，并验证完整 prepare → finalize → 执行，才考虑独立替换。不同 token 必须来自同次准备状态，不同时发送两套猜测字段，不新增自动协议降级或生成重放。此前 bootstrap 403 不能归因于后续 finalize 字段。

### 7.2 上游账号/工作区（GW-06）

HAR 的账号选择头可能与当前 Codex 集成及选定 workspace 有关。补充前必须证明其适用于本项目访问场景，且值可靠绑定当前凭据及明确选择的 workspace。不能使用本地账号 ID、slot ID、邮箱、token 哈希或样本常量替代。

来源缺失或选择含糊时保持现状；不得为了补齐一个头改变默认账号范围。多 workspace、同邮箱不同主体、凭据刷新和模型/额度范围需独立验证。

### 7.3 文件域与跳转（GW-08）

当前动态下载 URL 构造会附加 Bearer 和同源元数据，这需要按实际目标域核验；现有 HAR 无文件证据，不能推断已经泄漏凭据或直接删除认证。

后续先取得上传、下载及跳转的域和认证契约，再区分受信认证端点、签名 URL 及其他文件服务。Fetch Metadata 按实际来源关系设置，认证逐跳验证，保留 MIME、Blob 头和签名参数。不得扩大未知域信任，也不得盲目移除正常下载必需身份。该工作单独评审，不作为浏览器通用头完善的附带修改。

## 8. 基线与诊断边界（GW-01）

优先复用现有 event/request/round ID、阶段、HTTP 状态、耗时、错误类别及受控测试接收端。将“当前构造值”“实际发送值”“HAR 可见值”和“未观测”分开记录。保留用户提供的原始 HAR，不修改原证据、不用猜测补缺项。

如既有信息不足，只提出定位本批次差异所需的最小诊断补充；完整生产请求/响应归档、Cookie/凭据持久化和新的日志基础设施不默认纳入。本次不增加信息脱敏改造，也不把用户要求保留原始资料解释为持续记录全部生产凭据。

403 有 `cf-mitigated: challenge` 才可基于该证据细分；无证据的 403 不强行归为挑战。现有代码并未将全部 403 当作无效 token。本轮不修改冷却、刷新、熔断、重试或账号淘汰映射。

## 9. 实现边界与拟涉及位置

本节仅定位候选改动，不表示所有位置都需要修改。沿用现有 owner 和 typed EventHub 合同，不新建运行单元。

| 位置 | 可能职责 |
| --- | --- |
| `blocks/chatgptwebupstream/internal/client` | 经确认的分阶段头、环境表达；维持 Client/Jar 隔离 |
| `application/chatgptaccountpool/internal/store` | 仅 GW-05 获准后：设备状态、原子初始化与迁移兼容 |
| 账号池及上游 `pkg/events`、`biz` | 仅身份持久化需要时传递不可变 Device ID 等纯值 |
| 现有代理、临时对话及图像调用方 | 获准后传递同一账号身份，不自建身份权威 |
| 测试与相关文档 | 记录当前基线、批准字段和无回退验证 |

不能形成账号池同步等待上游、上游再同步回调账号池的事件等待回环。Client/Jar/连接指针不跨 owner 传递；不引入共享可变资源池。独立 OAuth 登录/刷新及 Codex 身份机制不套用 Web API 模板。

## 10. 风险与回滚

| 风险 | 控制与回退 |
| --- | --- |
| 正常请求因删头失败 | 每接口小补丁；保留修改前模板，回退该补丁 |
| 环境表达改变影响 Sentinel | 语言/时间独立验证；在途链不切换，后续链回到原环境 |
| 设备持久化新增存储失败路径 | 独立批次、原子初始化、失败测试；实施前明确可用性影响 |
| 设备迁移改变身份 | 先确定导入导出及目标优先级；回退不删除设备数据或恢复旧 token |
| 候选字段改变账号或协议语义 | 证据不足不启用；验证及批准后独立发布 |
| 诊断增加开销或扩大范围 | 复用既有观测，新增内容单独限定；不能影响正常业务结果 |

请求头/环境补丁可以独立回退。设备持久化补丁需要**保留读取持久设备能力的兼容回退版本**：基线 `6cbc042` 每次 Client 随机生成设备，即使保留新增字段也不会使用它，因此不能声称直接回退基线仍保持设备稳定。

每个获准批次在实现评审中记录基线、变更提交、回退提交或镜像及状态兼容验证；当前尚无实现版本，不虚构回退标识。没有这些信息的设备持久化批次不得进入发布。未决定使用配置开关，不在生产 YAML 提前添加候选项。

## 11. 验证与验收

### 11.1 验证矩阵

| 范围 | 核心验证 |
| --- | --- |
| 固定身份声明 | UA、Client Hints、TLS 未变，无管理浏览器采集 |
| 已出现接口 | 修改字段具备场景依据，认证及 Cookie 自动行为正常 |
| 同操作/跨操作 | 链内 Cookie 可传递；独立操作 Jar 不共享；Session ID 按 Client 创建 |
| 环境候选 | `Asia/Singapore` / UTC+08:00；当地日期与跨日换算正确、Unix 时间戳不平移、偏移符号符合字段契约；同次准备链环境一致，完整执行不回归 |
| 设备候选 | UUIDv4、并发一次初始化、重启/同主体 token 轮换延续、账号隔离、存储失败明确 |
| 状态与迁移 | 不额外重置状态；保留 token 轮换导致模型快照失效；验证导入导出及兼容回退 |
| 现有功能 | 模型发现、账号刷新、普通/流式文本、临时对话、搜索、图片生成/编辑/恢复、附件上传及下载正常 |
| 运行行为 | 既有并发、取消、重试预算及消息隔离无回归，无重复生成 |
| 暂缓候选 | 没有批准时不改变 finalize、workspace、文件域规则或生成路由 |

测试验证协议语义和结果，避免仅复制实现常量。HAR 缺失内容可用明确标注的合成 fixture 测试，但不能称为真实上游成功证据。

本轮按影响范围运行包测试、必要 race 检查、构建、`go vet ./...` 与 `go test ./...`；实际结果记录在第 14 节，同时检查文档差异格式、链接和内部一致性。

### 11.2 线上验收

后续经授权发布与真实调用时，记录实际镜像 revision、配置和观察窗口。先验证当前正常功能无回退，再判断批准字段是否符合对应浏览器语义。比较应区分操作、账号和代理条件，关注完成情况、延迟、并发、重复执行及账号状态。

真实样本不足标记“待验证”；不把 HTTP 200 单独等同于业务完成，不将偶发成功作为挑战已解决的依据。本轮不设虚构的成功率提升指标，也不承诺减少 403。

发生新增认证错误、功能失败、身份混用、重复执行或明显并发退化时停止扩大批次并回退。当前功能正常，允许将无法证明收益且存在兼容风险的候选项继续保留现状。

## 12. 本次实施边界与后续候选

本轮采用 GW-03/GW-04 中第 14 节列明的字段完善及 GW-05 设备持久化，GW-07 为保持现状的约束。GW-01 复用既有观测与受控测试，没有新增生产报文归档。

GW-02/GW-06/GW-08 继续待补证据。跨操作 Cookie/连接共享、动态浏览器身份采集、TLS 升级、zstd、高熵提示补齐、Codex 专用头、生成路由重写及挑战求解改造不在本轮范围。

代码收口和指定账号验证已获授权；未执行发布。x800 验证只允许必要的只读核对、凭据读取及独立验证，不调整已部署服务与配置。验证结果及实际限制见第 14 节。

## 13. 主要源码定位

- [基础配置、设备创建、bootstrap 和请求头](../../internal/modules/blocks/chatgptwebupstream/internal/client/client.go)：Config、New/newWithDoer、requestContext。
- [Sentinel 契约和环境](../../internal/modules/blocks/chatgptwebupstream/internal/client/requirements.go)：ChatRequirementsContext、finalizePayload、requirementsConfig。
- [错误分类](../../internal/modules/blocks/chatgptwebupstream/internal/client/errors.go)：classifyStatusResponse、classifyStatus。
- [模型发现](../../internal/modules/blocks/chatgptwebupstream/internal/client/models.go)、[文本执行](../../internal/modules/blocks/chatgptwebupstream/internal/client/text.go)、[搜索](../../internal/modules/blocks/chatgptwebupstream/internal/client/search.go)。
- [图像执行](../../internal/modules/blocks/chatgptwebupstream/internal/client/image.go)、[上传](../../internal/modules/blocks/chatgptwebupstream/internal/client/upload.go)、[轮询和下载](../../internal/modules/blocks/chatgptwebupstream/internal/client/poll.go)。
- [上游 owner](../../internal/modules/blocks/chatgptwebupstream/biz/biz.go)、[上游合同](../../internal/modules/blocks/chatgptwebupstream/pkg/events/contract.go)。
- [账号持久化及 token 轮换](../../internal/modules/application/chatgptaccountpool/internal/store/store.go)、[账号合同](../../internal/modules/application/chatgptaccountpool/pkg/events/contract.go)。

源码定位针对评估基线；实现后应以最终代码和实际验收结果更新，不能仅保留设计承诺。

## 14. 本轮实现与验证记录

### 14.1 具体字段及行为

| 接口/位置 | 字段或行为 | 修改前 → 本轮实现 | 依据与验证 |
| --- | --- | --- | --- |
| 首页 bootstrap | 导航头 | 移出 Bearer、Origin、Referer、oai-*；Priority `u=0, i`，Cache-Control `max-age=0`，无 Pragma；保留导航 Fetch Metadata、HTML Accept、UA/提示 | 初次直接导航语义；受控 HTTP 接收端验证实际请求 |
| JSON GET | Origin / Accept | 不再统一发送 Origin；Accept 改为 `*/*`；认证、设备、会话、target 及现有缓存头保留 | 已有 API HAR 与同源 GET 场景；受控请求和现有链路回归 |
| JSON POST | Origin / Content-Type / Accept | 保留同源 Origin、JSON Content-Type；Accept 改为 `*/*` | 已有 API HAR；准备和账号刷新测试 |
| requirements 环境 | 日期、语言 | UTC 字符串拼接美东后缀 → 新加坡当地日期及 `GMT+0800 (Singapore Standard Time)`；语言为 `zh-CN` / `zh-CN,zh,en,en-US` | 环境语义；跨日、冬夏日期和时间戳测试 |
| 文本/搜索/图片 | timezone / timezone_offset_min | `Asia/Shanghai` → `Asia/Singapore`；偏移保持 `-480` 并集中定义 | 用户明确指定；所有相关请求体测试 |
| 上传 | Accept-Language | 独立英文常量 → 现有中文语言列表 | 同一模拟环境；不改变文件域认证与签名逻辑 |
| 账号池/上游命令 | Device ID | 每 Client 随机 → 加密文档 `web_device_id` 持久 UUIDv4，事件传递纯值 | 并发、迁移、刷新、复制/恢复及失败路径测试 |
| Cookie / Session | 生命周期 | 每尝试新 Client/Jar/Session；当前操作内沿用；不共享、不落盘、不强制改写设备 Cookie | 真实本地 HTTP 客户端/Jar 隔离测试及现有重试回归 |

独立生成 SSE 模板、finalize 字段、workspace 与文件域认证策略保持现有实现。没有新增生产配置项、管理浏览器采集或诊断基础设施。

### 14.2 验证进度

最终检查均通过：

- `go test ./...`。
- `go test -race`：ChatGPT Web 上游、账号池，以及代理、临时对话、图像任务的受影响包。
- `go vet ./...`。
- `go build -buildvcs=false ./cmd/aetherrelay`（产物仅写入本地临时目录）。
- `git diff --check`、相关文档链接检查和变更文件格式检查。

独立真实账号验证（2026-10-28）：只读取得 x800 数据库与 WAL 的本地副本，在本地解密并仅选取 `aleng@mulife.vip` Web access token；未刷新或回写凭据。验证进程通过本地 CONNECT/SSH 转发使用 x800 网络出口，直接访问 ChatGPT，不经过已部署的 AetherRelay 服务。

| 项目 | 实测结果 |
| --- | --- |
| 账号信息 | 成功，返回邮箱匹配；耗时 5.335 秒 |
| 模型列表 | 成功，返回 12 个模型 |
| 文本最小调用 | `gpt-6` 成功，响应模型 `gpt-6`，正文 `OK`，耗时 6.881 秒 |

以上是首批最小调用结果。随后补验已通过流式文本、临时多轮对话、文本附件、图片生成/编辑/下载，以及本地重启后的图片任务恢复和设备身份保持。首轮账号搜索与图片识别未成功，补充搜索诊断确认上游 SSE `usage_limit`，并定位现有错误解析遗漏；改用用户指定的 `alexg@mulife.vip` 后，图片识别、正常联网搜索、来源及历史落库均通过。完整矩阵、错误及后续收口项见[补充验收记录](chatgpt-web-upstream-validation-2026-10-28.md)。正常流程通过不能替代限额等异常场景验收，不能据此推断成功率提升。

验收后已完成 SSE 明确错误的本地代码收口：文本/搜索/图片统一识别限额并保留错误详情，搜索在明确失败后停止轮询，保留部分结果和防重投边界。完整测试、race、vet、构建及指定账号的搜索/文本最小调用通过，详见补充验收记录第 8 节；未部署 x800。

### 14.3 发布与回退

本轮未提交、未部署。后续发布前需记录实际 revision、镜像及设备身份兼容回退版本；直接回退 `6cbc042` 会重新按 Client 生成设备，不能保持持久设备行为。验证过程不调整 x800 已部署服务与配置。
