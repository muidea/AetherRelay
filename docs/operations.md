# 运行、观测与发布

## 启动与客户端接入

```bash
make run
AI_PROXY_CONFIG=/etc/ai-proxy/config.yaml make run
```

默认服务地址为 `127.0.0.1:8080`。客户端使用标准入站地址：

```text
OpenAI API base:    http://127.0.0.1:8080/v1
Anthropic API base: http://127.0.0.1:8080
```

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/metrics
curl http://127.0.0.1:8080/stats
```

`/metrics` 与 `/stats` 默认仅允许 loopback。若启用远程访问，应同时设置 `metrics_allowed_cidrs` 限制采集端来源。

## Admin 登录安全（可选）

默认 Admin 仅 loopback 可访问。需要远程运维时，启用账号密码登录：

```bash
# 1. 交互式生成哈希（密码不进入 argv / 环境变量 / 日志）
ai-proxy admin password-hash
export AI_PROXY_ADMIN_PASSWORD_HASH='...'

# 或直接创建/重置 Admin 登录凭据（自动启用 admin_auth_enabled）
ai-proxy admin set-credentials --username ops-admin --config config.yaml

# 2. 配置 server.admin_auth_enabled=true 与账号，或使用环境变量
#    AI_PROXY_ADMIN_AUTH_ENABLED / AI_PROXY_ADMIN_USERNAME / AI_PROXY_ADMIN_PASSWORD_HASH

# 3. 通过 HTTP 或 HTTPS 对外暴露 <admin_base_path>（默认 /admin）
#    生产环境推荐 HTTPS；若要浏览器仅在 HTTPS 携带会话，设置 admin_session_cookie_secure=true。
#    代理应保留外部 Host；应用不信任 X-Forwarded-*。
```

运维注意：

- 启用后任意来源都必须登录；不再保留 loopback 特权旁路。
- 修改密码哈希、账号或开关并成功热更新后，全部内存会话立即失效。
- `admin_base_path` 是启动期路由；变更后必须重启进程，并同步反向代理路径规则。
- 连续 5 次登录失败会按对端 IP 锁定 15 分钟（不信任 forwarded IP）。
- Provider Key、客户端 Key 哈希、Admin 密码哈希与 DuckDB 文件仍需主机权限保护。

设计细节见 [Admin 登录安全设计](admin-login-security-design-2026-07-23.md)。

## ChatGPT Web 内建 Provider

启用 `chatgpt_web.enabled` 后，进程自动注入只读内建 Provider `chatgptweb`（不写 YAML）。模型来自账号池发现结果；运维入口是 ChatGPT Web 账号池，而不是 Provider 编辑表单。禁止在 `providers` 中再声明 `protocol: chatgptweb`。

## ChatGPT Web 管理页运维注意

Admin 管理台一级页签「ChatGPT Web」提供账号池、临时对话、图片任务与图片库操作入口，调用既有 `/api/chatgpt/**` 管理 API。页面与 API 共用 Admin 会话、CSRF 与 `X-AI-Proxy-Admin` 写保护。

### 临时对话正文保留与删除

- 临时对话的会话、消息与上游续聊锚点持久化在 `state.database`（DuckDB）专用表中，不进入 interaction archive，也不写入浏览器 localStorage/sessionStorage。
- 保留期由 `chatgpt_web.temporary_chat.retention_days` 控制（默认 30 天）。清理任务只删除已到期且没有活跃流的会话；管理员在页面删除为永久删除，不进回收站。
- 达到 `max_conversations` 时拒绝新建，需要先删除旧记录，系统不会为腾地方静默删历史。
- 服务重启时，任何 `streaming` 消息会被标记为 `interrupted`，会话进入 `recovery_required`：历史仍可读，但不得在原上游分支继续发送；需新建会话。
- 备份/恢复 `state.database` 即包含临时对话正文；共享或外发该文件等同于泄露管理员调试输入输出，需按主机权限与备份策略保护。
- research / deep_research 专用模型不会进入临时对话模型选择器或公开 `/v1/models`。

- **账号导出**：`POST .../api/chatgpt/accounts/export` 是唯一有意返回明文 token 的接口。必须二次确认；响应带 `Cache-Control: no-store`。不要把导出内容写入日志、工单、浏览器 localStorage/sessionStorage 或截图。下载后立即销毁本地副本。
- **OAuth 导入**：授权 URL、callback 与 session id 只应停留在管理员当前浏览器会话的内存中；不要把它们写进 URL 书签、共享剪贴板记录或监控日志。
- **图片删除**：图片库删除不可恢复；批量删除前确认路径列表。图片内容通过 Admin 鉴权同源端点 `GET .../api/chatgpt/images/content?path=` 读取（可选 `thumb=1`），路径经严格校验，不提供通用 `/files/**`。
- **owner_id**：图片任务以 `owner_id` 为隔离边界。运维代提任务时必须显式指定，不要共用一个长期固定 owner 混放不同业务方的任务。
- **失败处理**：失败任务已有 `conversation_id` 时，可使用“恢复轮询”继续读取同一上游任务；该操作不会重新提交生成，适用于轮询超时及历史版本误记为 `"<nil>"` 的记录。`bootstrap` 阶段的 TLS/超时失败尚未建立上游会话，页面会有限退避重试一次；仍失败时显示“重新提交”，以原任务参数重新发起。其它失败不提供盲目重试，避免重复生成或重复扣除额度。
- 组件未装配时相关 API 返回 `503`；页面会显示不可用状态，这不代表“没有账号/图片”。

设计与页面合同见 [ChatGPT Web 管理页收口设计](chatgpt-web-admin-closure-design-2026-07-26.md)。

## 指标与统计

Prometheus 指标均以 `ai_proxy_` 为前缀：

- `ai_proxy_requests_total{provider,model,route,status,outcome}`：请求完成数。
- `ai_proxy_request_duration_seconds_{sum,count}`：请求耗时。
- `ai_proxy_input_tokens_total`、`ai_proxy_output_tokens_total`、缓存 Token 与命中率：Provider/模型维度 Token 数据。
- `ai_proxy_client_requests_total{api_key_id}` 与 `ai_proxy_client_*_tokens_total{api_key_id}`：客户端 Key 维度累计数据。
- `ai_proxy_usage_store_*`：DuckDB 写入、查询、恢复、checkpoint 与健康状态。

`/stats` 返回进程统计、延迟分位数、缓存、上游错误与 all-time `usage` 视图。DuckDB 是用量最终 authority；Prometheus 与 `/stats` 的 Key 累计镜像在启动时由 DuckDB 初始化，并在成功结算请求后更新。

请求 outcome 用于表示流式首包写出后的真实结束态：

| outcome | 含义 |
| --- | --- |
| `success` | 正常完成。 |
| `client_canceled` | 客户端取消。 |
| `idle_timeout` | SSE 空闲超时。 |
| `limit_exceeded` | 本地体或流限制。 |
| `upstream_truncated`、`upstream_failed` | 上游中断或显式失败。 |
| `capability_drift` | Provider 声明的直连端点或模型能力与上游响应不一致。 |
| `incomplete` | 上游未完成。 |
| `client_write`、`protocol`、`conversion`、`error` | 客户端写入、协议、转换或其它错误。 |

统计查询、筛选与导出以管理页和当前实现为准；持久化工作区配置见[配置参考](configuration.md#统一状态工作区)。

## SLO webhook

配置 `slo_violation_webhook` 后，服务只在 SLO 状态变化时异步 POST `entered` / `resolved` 事件。事件带有 `instance_id`、递增 `seq`、`generation` 与稳定 `event_id`。

- 消费方应按 `event_id` 幂等，且只在同一 `instance_id` 内比较 `seq`。
- 投递为有界队列与单 worker；网络、408、425、429、5xx 最多重试三次，429 优先遵循 `Retry-After`。
- shutdown 会取消在途投递，并将剩余队列计入 `ai_proxy_slo_webhook_dropped_total`。

相关指标：`ai_proxy_slo_webhook_dropped_total`、`ai_proxy_slo_webhook_queue_length`、`ai_proxy_slo_webhook_requests_total{result}`。

## 用量、导出与归档

每个已接受请求会先写入 DuckDB `started` 事件，随后结算为 `completed`。管理页或 `<admin_base_path>/api/usage/export.csv`（默认 `/admin/api/usage/export.csv`）可导出安全元数据；单次导出最大范围为 31 天、最大 100,000 行。

旧 `usage.csv` 只可显式一次性导入：

```bash
go run ./cmd/ai-proxy-usage-import \
  -source usage.csv \
  -database var/state.duckdb \
  -api-key-id default
```

将示例中的 `var/state.duckdb` 替换为实际的 `state.database` 完整路径。交互归档位于 `state.dir/interactions/{round_id}/`，包含脱敏请求元数据、上游请求/响应摘要、客户端响应与 `metadata.json`。`archive_full_content: false` 可禁止请求与响应正文落盘。归档中的敏感 Header 会脱敏，原始客户端/Provider Key 不会写入。

## 备份与维护

不要直接复制正在写入的 DuckDB 文件。建议流程：停止接收新请求、等待当前写入完成、执行 checkpoint、复制 `state.database`，并将需要保留的 `state.dir/interactions/`、`state.dir/images/` 与 `state.dir/image_thumbnails/` 一并复制，随后恢复服务。数据库与整个 `state.dir` 必须由同一个实例独占。

## Provider live probe

Probe 不会在服务启动时运行，可用于验证某个已配置 Provider 的 direct capability：

```bash
go run ./cmd/ai-proxy-probe -config config.yaml \
  -provider <route-owner> -capability chat_completions -model <exact-model-id>
```

输出会脱敏，结论为 `success`、`credential_issue`、`capability_drift` 或 `environment_undetermined`。带日期的现场审计仅保留当时证据，不能替代对当前配置的重新探测。

Admin 的 Provider 页面还会显示配置启用状态之外的运行期可用性，并提供“检查”按钮。该按钮只对当前
Provider 执行一次最小非流式探测，记录结果但不会改写配置。状态含义如下：

| 状态 | 含义 |
| --- | --- |
| `disabled` | 配置已禁用。 |
| `unknown` | 尚无请求或手动检查结果。 |
| `healthy` | 最近一次记录为成功。 |
| `degraded` | 存在失败，但连续失败少于三次。 |
| `unavailable` | 连续失败至少三次。 |
| `credential_error` | 最近失败为 401 或 403。 |
| `capability_drift` | 最近探测表明端点或模型能力与上游不一致。 |

Provider 表的“来源”仅为展示分类：运行时内建 Provider 显示 `builtin`，官方 Base URL 显示 `official`，其余显示 `third_party`。该值不写入 YAML，也不参与路由或安全判断。

## 构建与发布

```bash
make check
make build
make release-package VERSION=v1.2.3
make release VERSION=v1.2.3
```

普通提交 CI 只执行 Linux amd64 的格式、依赖、vet、全量测试与构建。推送 `vX.Y.Z` tag 后，Release workflow 会统一验证源码一次，并在 Linux amd64/arm64、macOS arm64、Windows amd64 原生 runner 上打包 `.tar.gz` 与 SHA-256 文件，然后创建 GitHub Release。

不要从 amd64 强制交叉编译 Linux arm64：DuckDB Go bindings 需要相应的原生目标 runner。手动重跑 Release workflow 时，输入的版本必须是已有 tag。
