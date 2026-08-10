# 设计文档索引

本文档目录按**功能结构**组织 `AetherRelay` 的最终设计。每份设计文档只描述当前实现的最终决策与关键合同，不再包含开发过程中的中间方案；早期设计、收口计划与现场审计记录已归档至 [`docs/archive/`](../archive/)，仅作历史追溯。

## 文档体系

| 层级 | 文档 | 职责 |
| --- | --- | --- |
| 运行时合同 | [配置参考](../configuration.md)、[功能说明](../features.md)、[运维与发布](../operations.md)、[代码结构](../structure.md) | 当前行为与配置的权威；设计发生冲突时以正式合同与自动化测试为准 |
| 最终设计 | 本目录 | 各功能域的设计背景、核心机制与关键合同，供后续实现与评审参考 |
| 历史归档 | [`docs/archive/`](../archive/) | 带日期的中间过程设计、收口计划、审计与现场记录，非运行时合同 |

## 功能结构

| 功能域 | 设计文档 | 覆盖内容 |
| --- | --- | --- |
| 核心代理与路由 | [proxy-core.md](proxy-core.md) | 入站端点白名单、模型路由与候选链、转发矩阵、协议转换边界、typed error 与 envelope、统一流式 SSE |
| 安全与认证 | [security.md](security.md) | 客户端 API Key 身份与用量归属、Admin 账号密码登录、会话与 CSRF、访问控制边界 |
| ChatGPT Web 能力 | [chatgpt-web.md](chatgpt-web.md) | 账号池与内建 Provider、文本/图片代理、图片任务与图片库、临时对话、在线搜索、管理页 |
| Codex OAuth 账号池 | [codex-oauth.md](codex-oauth.md) | 独立账号域、模型发现、原生 Responses 代理、额度观察与账号韧性 |
| 账号池整体迁移 | [account-pool-bundle-migration.md](account-pool-bundle-migration.md) | ChatGPT Web 与 Codex 双槽位整体导入导出（仅整体导出）、跨上游账号关联与冲突处理 |
| 迁移 Bundle 文件命名 | [bundle-file-naming.md](bundle-file-naming.md) | Provider 与账号池整体迁移的统一导出文件名、敏感性标识与导入判定边界 |

## 设计总则

- **配置是唯一 authority**：路由、模型、账号只由配置与进程内状态决定；客户端请求不携带任何 provider 选择指令。
- **请求期只读**：启动期完成全部静态校验，请求期在只读快照上生成传输方案，不得改写路由归属。
- **失败不越界**：回退只在响应未提交时发生；一次已写出的响应绝不切换上游。
- **敏感信息不落地**：密钥、token、账号凭据不写入日志、DuckDB、归档或浏览器持久化。
