# ModelTrace 集成与验收

日期：2026-10-20。维护合同：18.0.0。真实模型请求：未执行。

## 结构决策

遵循 go-multi-module-dev：完整检测闭环选择 Application Module，纯算法选择 focused package。`cmd/aetherrelay/main.go` 显式加载 `application/modeldetection`；`module.go` 仅桥接 Setup/Run/BeginShutdown/Quiesce/Teardown，Biz 内嵌共享 Base。检测 Module 拥有挑战、任务、评分和报告；Proxy 拥有有界采样执行，账号和上游 Block 保持各自资源权威。HTTP adapter 复用 Admin，故不额外建立无职责的 service 层。Setup 建立合同订阅，不靠 Weight 隐藏运行期依赖。

所有跨单元调用通过 owner 自有 typed EventHub command/result；无 service/repository/回调注入，无资源句柄、map/any 或 JSON 兼容桥。原生 Responses JSON 字节仅作为既有上游协议载荷。周期巡检和采样 worker 挂 framework BackgroundRoutine。停止先拒绝新任务、等待在途提交完成，再取消采样、确认 worker 真实退出和租约检查式释放；超时保留清理责任，可再次排空。

## 直接复制范围

`internal/pkg/modeltrace` 直接复制 PoC v0.2.0 的公开算法包和测试；assets 内嵌 bank/provenance/LICENSE。生产算法文件与源 PoC 字节一致，资产包只调整 import 路径和格式；测试调整相对路径，并直接复用 oracle fixtures 和测试用 parity runner。无新 Go module、replace、网络依赖或运行时 Node。来源与通知见该目录 SOURCE.md、NOTICE.md。

协议固定 Pages 原版中文三挑战，独立新会话，292–332 不同长度，CoreCompatible；不是严格 JSON 固定300 profile。固定 bank 仍为18候选，不能将库内权重解释为身份概率。报告保留失败样本位置、实际接纳数量、模型声明及不完整 usage 状态；输出标记 derived_completed_response，源请求与最终出站请求摘要分开。

评审收口：采样绕过普通 HTTP 请求归一化入口，因此在 Proxy 构造请求时显式发送 `instructions: ""`，遵循 `CP-REQ-003`。回归测试区分字段缺失与空字符串，覆盖三次采样提交及上游注入 `client_metadata` 后的最终 HTTP 正文；新增断言在修复前可复现失败。

额度观测收口：旧 Steering native/fallback 链路已删除；现行采样快照从 HTTP attempt 分别取得请求开始及响应头接收时间。Proxy 以真实请求时间参与 Store 窗口排序，以响应头接收时间计算相对重置时刻，不使用任务开始、采样调度或轮询时间替代。回归使用真实账号 owner 和 Store，验证连续 99%→100% 更新、最新额度限制准入及重置时间；修复前可复现重置时间错误。

## 产品与资源

原账号行“检测上游能力”直接替换为“检测模型指纹”。旧 `/steering-probes`、专用 Steering 引擎、fallback、旧页面与测试均已移除；共享账号准入/刷新代码保留并改名 probe_admission。新 API 为 `/api/codex/accounts/{slot}/modeltrace-probes`：POST 创建，GET 查询（含 latest），DELETE 取消。写操作沿用 Admin 认证、写保护与幂等键。

全局一个执行任务、最多三次推理、从创建起180秒、单次60秒/1 MiB 接收正文及完成响应；输出文本最多16 KiB。没有换号、模型回退、未知执行重放或自动补采。报告内存保留30分钟/最多100份，重启丢失；下载为 AetherRelay 报告 schema，不冒充独立 PoC 的 samplebundle v1。报告中的合成文本可通过同一个 Engine 离线复算。

## 本地验收

| 检查 | 状态 |
| --- | --- |
| 原 PoC oracle 数值/解析兼容 | PASS，66/66；最大绝对误差 1.1102230246251565e-16 |
| 原算法生产文件字节与 bank 摘要 | PASS |
| 全量 go test ./... | PASS |
| 受影响包 race：检测/Proxy/账号/上游/Admin/生命周期/算法 | PASS |
| 管理页面测试：账号/模型/用量/新检测 | PASS |
| go vet ./...、构建、gofmt、diff whitespace | PASS |
| 结构扫描：Base Biz、typed EventHub、入口装配、旧链路清零 | PASS |
| 真实 Codex OAuth 路径及 holdout 校准 | NOT_RUN |
| 客观质量回退确认 | NOT_RUN |

本地模拟覆盖严格准入零出站、三独立挑战、幂等冲突、跨槽隔离、部分失败诊断、不可变报告、无凭据输出、健康隔离、取消与清理失败重试、接收上限、停止后的清理入口、管理写保护及页面转义。当前只交付手动行为指纹检测；长期持久历史、定时检测、漂移阈值和客观质量确认均为后续工作。
