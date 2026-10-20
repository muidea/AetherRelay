# 来源

Go 算法移植自 xqy2006/ModelTrace 固定 commit `6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2`，bank、provenance 与 MIT 许可证保留在 `assets/vendor/modeltrace/<commit>/`；用于开发核对的原始 JS 保留在 `testdata/challenge-browser.mjs`（仅保留挑战生成器）。原作者 Copyright (c) 2026 xqy2006；适用 MIT 许可全文见该目录 LICENSE。

Go port、离线命令、schema、测试 wrapper、synthetic fixtures 和文档为本 PoC 新增。wrapper 新增私有导出不属于上游原始 API。

v0.2.0 将 Go 移植实现整理到公开 pkg/modeltrace，补充了原版 Pages 挑战模板的 Go 生成器；原始 JS 生成器字节保留在 testdata 快照，适用许可证见上述 assets 路径。assets.Load 将 bank/provenance/LICENSE 内嵌供离线消费，不执行 vendored JS。
