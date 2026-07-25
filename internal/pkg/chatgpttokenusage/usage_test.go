package tokenusage

import "testing"

func TestEstimateTextTokens(t *testing.T) {
	if got := EstimateTextTokens("hello world"); got != 4 {
		t.Fatalf("ascii tokens=%d", got)
	}
	if got := EstimateTextTokens("你好，world!"); got != 6 {
		t.Fatalf("mixed tokens=%d", got)
	}
}

func TestEstimateChatTextUsage(t *testing.T) {
	usage := EstimateChatTextUsage([]string{"hello", "世界"}, "done")
	if usage.PromptTokens != 4 || usage.CompletionTokens != 1 || usage.TotalTokens != 5 {
		t.Fatalf("usage=%#v", usage)
	}
}
