package modeltrace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPagesProtocolTemplates(t *testing.T) {
	challenges, e := GenerateChallenges(bytes.NewReader(make([]byte, 512)))
	if e != nil {
		t.Fatal(e)
	}
	source, e := os.ReadFile("testdata/challenge-browser.mjs")
	if e != nil {
		t.Fatal(e)
	}
	const opening = "这是一次独立的数值选择记录"
	const action = "为各个位置分别凭第一反应选择"
	for i, c := range challenges {
		if c.ExpectedCount != 292+i || c.PromptVersion != PagesPromptVersion {
			t.Fatal("wrong count policy/version")
		}
		prefix := fmt.Sprintf("%s。%s %d 个 1 到 355（含端点）的整数。", opening, action, c.ExpectedCount)
		if !strings.HasPrefix(c.Prompt, prefix) {
			t.Fatal("different prompt prefix")
		}
		// Check every remaining literal segment against the reviewed upstream source.
		for _, piece := range []string{"每个位置都要单独选择；不要从 1 开始计数，不要连续递增或递减，也不要采用等差、循环、重复区块或其他规则化模式。", "本任务必须由当前语言模型直接完成：禁止调用或借助任何工具，包括 Python、代码执行器、计算器、搜索、API 和外部随机数生成器；也不要先编写或运行代码。", "允许某个数字再次出现；每项写出后不要回头排序、去重或替换。", "数字之间用逗号或空格分隔均可。", "直接从第一个取值开始输出，不要在序列前重复数量、范围或任务说明。"} {
			if !strings.Contains(c.Prompt, piece) || !strings.Contains(string(source), piece) {
				t.Fatal("prompt differs from upstream")
			}
		}
	}
	if len(challenges) != 3 {
		t.Fatal("expected three challenges")
	}
}
func TestChallengeEntropyFailure(t *testing.T) {
	if _, e := GenerateChallenges(bytes.NewReader(nil)); !errors.Is(e, io.EOF) {
		t.Fatal(e)
	}
}
