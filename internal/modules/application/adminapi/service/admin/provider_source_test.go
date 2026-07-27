package admin

import "testing"

func TestClassifyProviderSource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		builtin bool
		baseURL string
		want    string
	}{
		{name: "builtin wins over official host", builtin: true, baseURL: "https://api.openai.com/v1", want: ProviderSourceBuiltin},
		{name: "builtin empty base", builtin: true, baseURL: "", want: ProviderSourceBuiltin},
		{name: "openai official", baseURL: "https://api.openai.com/v1", want: ProviderSourceOfficial},
		{name: "openai official no scheme", baseURL: "api.openai.com/v1", want: ProviderSourceOfficial},
		{name: "openai subdomain", baseURL: "https://us.api.openai.com", want: ProviderSourceOfficial},
		{name: "anthropic official", baseURL: "https://api.anthropic.com", want: ProviderSourceOfficial},
		{name: "deepseek official", baseURL: "https://api.deepseek.com", want: ProviderSourceOfficial},
		{name: "deepseek with port", baseURL: "https://api.deepseek.com:443/v1", want: ProviderSourceOfficial},
		{name: "third party gateway", baseURL: "https://onlycode.shop/v1", want: ProviderSourceThirdParty},
		{name: "third party bluetron", baseURL: "https://aiapi.bluetron.cn", want: ProviderSourceThirdParty},
		{name: "third party krill", baseURL: "https://api.krill-ai.com/v1", want: ProviderSourceThirdParty},
		{name: "internal loopback", baseURL: "http://127.0.0.1:8080/v1", want: ProviderSourceThirdParty},
		{name: "private lan", baseURL: "http://192.168.19.233:8010", want: ProviderSourceThirdParty},
		{name: "lookalike host not official", baseURL: "https://api.openai.com.evil.example/v1", want: ProviderSourceThirdParty},
		{name: "empty base", baseURL: "", want: ProviderSourceThirdParty},
		{name: "dashscope official", baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", want: ProviderSourceOfficial},
		{name: "xai official", baseURL: "https://api.x.ai/v1", want: ProviderSourceOfficial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyProviderSource(tc.builtin, tc.baseURL)
			if got != tc.want {
				t.Fatalf("classifyProviderSource(%v, %q) = %q, want %q", tc.builtin, tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestProviderBaseHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{"https://api.openai.com/v1", "api.openai.com"},
		{"http://API.OpenAI.COM:443/v1", "api.openai.com"},
		{"api.deepseek.com", "api.deepseek.com"},
		{"https://[2001:db8::1]:8080/v1", "2001:db8::1"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := providerBaseHost(tc.raw); got != tc.want {
			t.Fatalf("providerBaseHost(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
