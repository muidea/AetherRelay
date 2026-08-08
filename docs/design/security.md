# 安全与认证设计

功能域：客户端 API Key 身份与用量归属、Admin 账号密码登录、访问控制边界。对应正式合同见[配置参考](../configuration.md)与[功能说明](../features.md)。

客户端 Key 与 Provider/模型访问范围的最终合同见[客户端 API Key、Provider 与模型联动收口设计](client-api-key-provider-access.md)。

## 设计目标

- 客户端 Key 是数据端点的必需认证，也是用量归属的唯一身份来源；明文只在创建/轮换响应中出现一次，DuckDB 只保存摘要。
- Admin 默认仅本机可访问；开启登录后任意来源都必须认证，不保留 loopback 旁路。
- 敏感信息（Key、token、账号凭据、密码哈希）不写入日志、归档、错误信息或浏览器持久化。

## 客户端 API Key

### 配置合同

- DuckDB 是客户端 Key 的唯一持久化权威，不接受 YAML 中的 Key 定义或摘要。
- Key 摘要仅接受 `sha256:` + 64 位小写十六进制；原始 Key 与摘要在所有条目间均唯一。
- Key ID 匹配 `[a-z0-9][a-z0-9._-]{0,63}`；`default` 为历史用量保留 ID，不能配置。
- 服务端生成 Key 统一 `sk_` + base64.RawURLEncoding(crypto/rand 32 bytes)，至少 256 bit 熵；明文仅在创建/轮换成功响应中返回一次，响应 `Cache-Control: no-store`。

### 身份解析与 401 不变量

- 运行时认证索引只从 DuckDB 读取摘要 → ClientIdentity 映射。
- OpenAI 用 `Authorization: Bearer <key>`，Anthropic 用 `X-API-Key: <key>`；两种 Header 可兼容，同时出现必须为同一 Key。
- 缺失、空白、未知、禁用、格式错误或冲突 Header 均返回 401，且**不产生用量记录**（401 发生在 `UsageStore.Start` 之前）。
- 所有持久化、指标、日志、归档与 Admin 查询只出现 `api_key_id`，原始 Key 绝不出现。
- `ClientIdentity` 携带不可变 ProviderAccess 快照；零值为 deny-all。`selected` 只允许明确 Provider，`all` 允许当前和未来 Provider。模型发现、能力投影、路由候选和 fallback 必须先执行同一权限过滤，未授权模型统一返回 `model_not_found`。

### Admin 管理

- 创建与轮换不接受客户端提供的明文或摘要（服务端生成）；创建必须提供完整 `provider_access`，PUT 替换访问范围，PATCH 只允许改 `enabled`；轮换保持同一 `api_key_id` 和 ProviderAccess，激活后新请求只接受新 Key，在途旧快照请求允许完成，无宽限期；禁用与撤销均使新请求 401，不删除历史 usage；删除 `default` 返回 400。
- 列表暴露策略、当前有效/不可用 Provider ID 和去重模型数，但不暴露摘要、base URL、凭据来源或账号 ID；有效模型接口只返回模型、候选 Provider ID、客户端端点与容量。

### 配置写入与激活事务

客户端 Key 管理直接使用 DuckDB 事务并刷新运行时认证索引，不修改配置文件，也不依赖配置文件 revision。写入顺序固定为 prospective records → prepare immutable index → Store transaction → atomic activate；任何准备失败都不能写库或返回一次性明文。

## Admin 登录（可选）

### 配置合同

- `server.admin_auth_enabled` 是唯一开关，默认 `false`（保持 loopback-only）。
- 开启时必须配置单管理员账号与 Argon2id PHC 哈希（唯一允许参数 `m=65536,t=3,p=1`、salt ≥ 16 bytes、输出 ≥ 32 bytes）；缺失、非法或弱参数使进程在监听前启动失败。只接受哈希，拒绝明文与可逆加密。
- 提供 `ai-proxy admin password-hash`（交互式，TTY 两次读密码）与 `admin set-credentials`（直接创建/重置凭据，自动写入配置）两个子命令；密码不进入 argv / 环境变量 / 日志。
- `admin_base_path` 是启动期路由（默认 `/admin`）；页面、认证端点、业务 API 与 Cookie Path 均从其派生，变更必须重启。

### 会话模型

- 服务端内存 SessionStore：32 bytes 随机 session ID 与 CSRF token；Cookie 名 `ai_proxy_admin_session`，`HttpOnly` + `SameSite=Strict`，`Max-Age` 等于 TTL，`Secure` 由 `admin_session_cookie_secure` 决定（默认 false，HTTPS 反向代理部署应开启）。
- 绝对 TTL 默认 28800 秒（范围 300~86400），不因访问续期；会话上限 64，满时新登录 503，不驱逐活跃会话。

### CSRF 与滥用防护

- 认证开启时，所有状态变更请求须会话 Cookie + `X-AI-Proxy-CSRF` + Origin 精确匹配（非浏览器缺失 Origin 允许）；`X-AI-Proxy-Admin` 保留但不作为身份凭据。
- 登录失败统一返回 `invalid username or password`（不泄露账号存在性）；按实际对端地址内存限速：连续 5 次失败后 15 分钟内 429 + `Retry-After`，成功登录清零。
- 认证相关配置热更新成功激活后清空全部会话与登录限速记录；仅热更新无关配置时已登录会话保持有效。

## 访问控制边界

- **默认 loopback-only**：`/admin`、`/metrics`、`/stats` 在认证关闭时仅 loopback；远程访问分别由 `admin_auth_enabled` 与 `metrics_remote_access` + `metrics_allowed_cidrs` 控制。
- 未登录写接口仍需 `X-AI-Proxy-Admin: 1` 意图头；它只是浏览器请求意图的表达，可被本机进程伪造，不作身份凭据。
- **不信任任何 forwarded header**（`X-Forwarded-For` / `X-Forwarded-Proto` 等）作身份或协议判断；不基于 RemoteAddr/CIDR 跳过认证；反向代理部署须保留外部 `Host`。
- Provider Key 只显示“已配置”，不回显明文；日志与归档脱敏 `Authorization` / `X-API-Key` / `Cookie` 等 Header。

## 可恢复凭据存储

- Provider 完整目录、ChatGPT Web 账号和 Codex OAuth 账号以 owner-scoped 安全文档保存到 DuckDB；access token 不再作为数据库主键。
- 安全文档使用 AES-256-GCM 和随机 nonce 加密，并将 scope 与稳定记录 ID 作为附加认证数据，防止密文跨记录替换。
- 主密钥 `AI_PROXY_CREDENTIAL_KEY` 必须是 Base64 编码的 32 字节随机值，只从进程环境或编排 secret 注入。它不得写入 `config.yaml`、DuckDB、日志或版本库。
- 主密钥缺失时账号池启动失败；尚无 Provider 目录的新实例只读，已有 Provider 密文的实例启动失败。密钥错误时解密明确失败，不得回退为空目录或读取明文。

## 演进记录

- 2026-07-20：客户端 API Key 管理设计 → 归档 `docs/archive/client-api-key-management-design-2026-07-20.md`
- 2026-07-23：Admin 登录安全设计 → 归档 `docs/archive/admin-login-security-design-2026-07-23.md`
