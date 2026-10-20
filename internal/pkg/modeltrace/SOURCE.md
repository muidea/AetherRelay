# ModelTrace 来源

直接复制自 `/home/fedquery/codespace/agentSuite/poc/modeltrace-go-poc` v0.2.0（2026-10-20）。
上游 https://github.com/xqy2006/ModelTrace 固定提交 `6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2`。

复制范围：`pkg/modeltrace`、内嵌 assets、对应测试及挑战模板 oracle。只调整 import 和测试资产相对路径；数值算法、提示模板、参考资产字节不变。不复制 CLI、网络实验脚本、凭据或账号采样逻辑。许可见 assets/vendor/modeltrace 下 LICENSE 和 NOTICE.md。

本包是 focused package，无 framework 生命周期、EventHub、网络、数据库或后台任务。`application/modeldetection` 负责检测任务与评分；Proxy/账号/上游 Owner 负责实际采样。
