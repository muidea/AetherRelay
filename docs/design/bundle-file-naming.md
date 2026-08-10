# 管理面迁移 Bundle 文件命名

## 范围

本规范统一以下管理面迁移导出接口产生的文件名：

- Provider 配置：`POST /admin/api/providers/export`
- 账号池整体迁移：`POST /admin/api/account-pool-bundle/export`

不涵盖已不属于最终对外合同的单槽位账号导出；不引入旧文件名迁移或重写流程，导入仍不依赖文件名。

## 命名格式

```text
ai-proxy-{artifact}-bundle-v{schema}-{profile}-{YYYYMMDDTHHMMSSZ}.json
```

其中时间戳使用导出服务端生成的 UTC `exported_at`，精确到秒，且采用文件系统安全格式。HTTP 响应的 `Content-Disposition` 与管理页触发的浏览器下载必须使用完全相同的名称。

| 字段 | 含义与可选值 |
| --- | --- |
| `artifact` | `provider`（自定义 Provider 配置）或 `account-pool`（整体账号池） |
| `schema` | JSON 中对应的 `schema_version`，当前 Provider 为 `1`，账号池为 `2` |
| `profile` | `safe` 表示不含密钥；`complete` 表示包含敏感凭据 |
| `YYYYMMDDTHHMMSSZ` | 由服务端 `exported_at` 转换而来，例如 `20260810T123456Z` |

当前有效文件名示例：

```text
ai-proxy-provider-bundle-v1-safe-20260810T123456Z.json
ai-proxy-provider-bundle-v1-complete-20260810T123456Z.json
ai-proxy-account-pool-bundle-v2-complete-20260810T123456Z.json
```

## Profile 与安全边界

- Provider 的默认导出为 `safe`，仅在请求体指定 `include_api_keys: true` 后导出 `complete` 文件。
- 账号池整体迁移始终含 OAuth 凭据，因此固定为 `complete` 文件，并继续设置 `Cache-Control: no-store`。
- `complete` 文件必须按凭据文件保管，不能写入日志、浏览器持久化存储或普通归档。

## 导入判定

文件名只用于管理员识别、整理和审计，不参与导入兼容性判定。导入端始终以 JSON 内的 `format` 与 `schema_version` 为准，并继续执行各自的内容、凭据和冲突校验；重命名文件不会改变其可导入性。
