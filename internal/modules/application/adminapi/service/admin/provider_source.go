package admin

import (
	"net"
	"net/url"
	"strings"
)

// Provider 列表展示用的来源标签。仅用于 Admin 投影，不进入配置合同、不参与路由。
// 识别规则：builtin 优先；否则按 base_url 主机是否命中官方 vendor 主机表；其余为第三方。
const (
	ProviderSourceBuiltin    = "builtin"
	ProviderSourceOfficial   = "official"
	ProviderSourceThirdParty = "third_party"
)

// officialProviderHosts 是官方厂商 API 主机后缀表（小写、无端口）。
// 匹配时允许精确或子域（如 api.openai.com、us.api.openai.com）。
// 仅覆盖公开官方文档中的 API 入口；网关/中继/自建地址一律视为第三方。
var officialProviderHosts = []string{
	"api.openai.com",
	"api.anthropic.com",
	"api.deepseek.com",
	"api.moonshot.cn",
	"api.moonshot.ai",
	"api.minimax.chat",
	"api.minimaxi.com",
	"api.groq.com",
	"api.mistral.ai",
	"api.cohere.ai",
	"api.cohere.com",
	"generativelanguage.googleapis.com",
	"api.x.ai",
	"api.together.xyz",
	"api.fireworks.ai",
	"api.perplexity.ai",
	"dashscope.aliyuncs.com",
	"api.siliconflow.cn",
	"open.bigmodel.cn",
	"api.lingyiwanwu.com",
	"api.baichuan-ai.com",
	"ark.cn-beijing.volces.com",
	"api.hunyuan.cloud.tencent.com",
}

// classifyProviderSource 根据 builtin 标志与 base_url 自动判定来源。
// 不读配置字段；未知/无法解析的 base_url 归为第三方。
func classifyProviderSource(builtin bool, baseURL string) string {
	if builtin {
		return ProviderSourceBuiltin
	}
	if isOfficialProviderBaseURL(baseURL) {
		return ProviderSourceOfficial
	}
	return ProviderSourceThirdParty
}

func isOfficialProviderBaseURL(raw string) bool {
	host := providerBaseHost(raw)
	if host == "" {
		return false
	}
	for _, official := range officialProviderHosts {
		if host == official || strings.HasSuffix(host, "."+official) {
			return true
		}
	}
	return false
}

// providerBaseHost 从 base_url 提取小写主机名（去端口、去方括号）。
// 兼容缺 scheme 的写法（如 api.openai.com/v1），解析失败返回空串。
func providerBaseHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		// url.Parse 对部分输入可能把 host 放在 Path；再试 net.SplitHostPort / 裸 host。
		host = fallbackHost(raw)
	}
	host = strings.ToLower(strings.Trim(host, "."))
	if host == "" || host == "localhost" {
		return host
	}
	if ip := net.ParseIP(host); ip != nil {
		return host
	}
	return host
}

func fallbackHost(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(raw, "[]")
}
