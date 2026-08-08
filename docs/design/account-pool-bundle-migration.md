# 账号池整体导入导出格式（v2）

## 目标

账号池由 ChatGPT Web 和 Codex OAuth 两个独立 Store 组成。整体迁移格式只表达“一个统一账号包含哪些凭据槽位”，不把两个 Store 合并成一个存储，也不包含 Provider、客户端 API Key、用量、图片、interactions 或运行态。

账号池对外只提供整体迁移格式；不提供针对 ChatGPT Web 或 Codex 单个槽位的导出接口。槽位仍在整体包内部独立表达，并分别交给对应 Store 导入。

## 为什么不能使用 identity_key 作为迁移主键

运行时 `identity_key` 的生成规则是 `account_id` 优先、邮箱 fallback：

```text
accountidentity.Key(account_id, email)
```

因此同一邮箱在两个上游返回不同 `account_id` 时会得到两个不同的 `identity_key`。`identity_key` 是本地展示聚合用的不可逆摘要，不适合作为跨实例、跨槽位的业务主键。

整体迁移包使用独立的 `account_ref` 表达统一账号，并在每个槽位中保留原始 `identity_key` 作为诊断信息。

## 文件格式

```json
{
  "format": "ai-proxy.account-pool-bundle",
  "schema_version": 2,
  "exported_at": "2026-08-08T00:00:00Z",
  "accounts": [
    {
      "account_ref": "acct_01J...",
      "identity": { "email": "user@example.com" },
      "slots": {
        "chatgpt_web": {
          "credential_type": "chatgpt_web",
          "account_id": "chatgpt-account-id",
          "identity_key": "acct_...",
          "access_token": "...",
          "refresh_token": "...",
          "id_token": "...",
          "expired": "2026-08-08T12:00:00Z",
          "proxy": "http://proxy.example"
        },
        "codex_cli": {
          "credential_type": "codex_oauth",
          "account_id": "codex-account-id",
          "identity_key": "acct_...",
          "email": "user@example.com",
          "access_token": "...",
          "refresh_token": "...",
          "id_token": "...",
          "expired": "2026-08-08T12:00:00Z",
          "proxy": "http://proxy.example"
        }
      }
    }
  ]
}
```

### 字段约束

- `format` 必须精确为 `ai-proxy.account-pool-bundle`。
- `schema_version` 当前为 `2`，整体格式不承诺旧版本兼容。
- `account_ref` 是文件内统一账号的稳定引用；导入目标实例不得把它直接当作本地账号 ID，可重新生成本地引用。
- `identity.email` 明文保留，用于展示和跨槽位匹配；邮箱为空时允许仅有一个槽位，但不能据此强行合并两个不同账号。
- `slots` 的键只允许 `chatgpt_web`、`codex_cli`；至少一个槽位存在。
- 槽位的 `account_id`、`identity_key` 保留上游/源实例信息，但不作为目标实例的本地 ID。
- 凭据字段只允许出现在明确的凭据导出接口响应中；导出文件必须按凭据文件处理，禁止写入日志。
- 不导出状态、额度、刷新错误、冷却、模型快照、用量、Provider、API Key、图片和交互记录。

## 导出分组规则

导出时先建立统一账号组：

1. 有邮箱的槽位按 `trim + strings.ToLower(email)` 分组。这样即使 ChatGPT Web 与 Codex 的上游 `account_id` 不同，只要邮箱相同，仍会进入同一个 `account_ref`。
2. 无邮箱的槽位按其 `identity_key` 分组。
3. 同一组内每种槽位最多一个；发现重复槽位时导出失败并报告冲突，不静默覆盖。
4. `account_ref` 使用随机生成的 `acct_` 引用，不能使用邮箱，也不能使用任一上游账号 ID。

该分组规则仅用于整体迁移，不改变运行时 `identity_key` 的生成和路由行为。

## 导入匹配规则

每个槽位仍交给现有对应 Store 的导入流程，两个槽位不会写入同一张账号表。统一账号关系按以下顺序恢复：

1. 文件内 `account_ref` 将同一文件中的槽位归为一组。
2. 目标实例已有账号优先按槽位 `account_id` 匹配。
3. 无法按 ID 匹配时，按槽位邮箱匹配；再按 `identity.email` 匹配。
4. 匹配不到时创建新槽位，并将其挂载到该 `account_ref` 对应的统一账号组。
5. 同一个统一账号已有同类型槽位时，默认返回冲突；只有显式 `replace=true` 才允许替换。
6. 不能因为邮箱相同而合并两个已有不同 `account_id` 且已有不同 `account_ref` 的账号，冲突必须交给管理员处理。

两个 Store 不具备跨 Store 事务。导入结果必须分别返回 `chatgpt_web`、`codex_cli` 的 added/updated/skipped/conflicts，并标明统一账号是否完整或部分成功。

## 对外接口

账号池整体迁移接口：

```text
POST /admin/api/account-pool-bundle/export
POST /admin/api/account-pool-bundle/import
```

整体导出一次导出当前完整账号池；整体导入先完成文件和槽位级校验，再按槽位调用内部导入逻辑，最后返回逐账号、逐槽位结果。ChatGPT Web 和 Codex 的槽位导出按钮、槽位导出路由以及槽位导出响应均不属于最终对外合同。

## Web 管理端设计

### 账号池页面

账号池页面以“统一账号”为行，以凭据槽位为列：

```text
统一账号（邮箱） | ChatGPT Web 槽位 | Codex CLI 槽位 | 状态 | 操作
```

- 一行代表一个 `account_ref` 逻辑账号，而不是某个 Store 的本地账号 ID。
- `chatgpt_web` 和 `codex_cli` 槽位分别显示存在/缺失、状态和刷新操作。
- 槽位操作使用各自 Store 的本地 `id`，不能使用 `account_ref` 直接调用底层账号接口。
- 页面不显示访问令牌、刷新令牌或 ID Token。
- 页面提供一个“整体导出”按钮，导出当前选择的统一账号及其全部槽位；不提供“导出 ChatGPT Web 槽位”和“导出 Codex 槽位”按钮。

### 分槽位导入

现有两个导入入口继续保留，便于凭据来源不同或仅补充一个槽位：

- “导入 ChatGPT Web 账号”：只接受 `chatgpt_web` 凭据，调用 ChatGPT Web 内部导入流程；
- “导入 Codex 账号”：只接受 `codex_oauth`/`codex_cli` 凭据，调用 Codex OAuth 内部导入流程；
- “导入整体账号池”：只接受 `ai-proxy.account-pool-bundle` 文件，并按文件中的槽位分别导入。

分槽位导入成功后，页面重新加载两个 Store 的列表并重新计算统一账号行。为了弥补两个上游 `account_id` 不同导致的运行时 `identity_key` 不同，Web 展示聚合采用以下顺序：

1. 槽位邮箱和统一账号邮箱的规范化值（去首尾空格并转小写）；
2. 槽位 `identity_key`；
3. 槽位本地 `id`（仅用于避免无邮箱账号碰撞）。

因此先导入 ChatGPT Web、后导入同邮箱 Codex 时，会在页面上补齐同一行的 Codex 槽位；邮箱缺失或不一致时不会强行合并，显示为独立账号并提示需要人工确认。

### 导入反馈

分槽位导入和整体导入都需要显示：

- 新增、更新、跳过数量；
- 按账号和槽位的冲突列表；
- 统一账号是否为双槽位完整账号或单槽位账号；
- 部分成功时明确提示另一个 Store 未完成，不回滚已成功的槽位。

导入文件大小、条目数量和凭据字段校验沿用现有管理端限制；错误提示不得回显任何 Token 内容。

## 安全与审计

- 整体导入导出需要 Admin 权限。
- 邮箱可以明文出现在文件和管理页面；访问令牌、刷新令牌、ID Token 不做脱敏，但不得进入普通列表 API、日志、usage 或 interactions。
- 导入错误只返回字段级原因，不回显 Token 内容。
- 导出文件下载响应设置 `Cache-Control: no-store`，并提示管理员妥善保管。
