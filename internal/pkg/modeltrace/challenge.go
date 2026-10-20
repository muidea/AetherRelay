package modeltrace

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
)

// PagesPromptVersion fixes the original ModelTrace Pages first-instinct protocol.
const PagesPromptVersion = "modeltrace-pages-6cd4a8ff-v1"

// Challenge contains a prompt to send as a separate, fresh user request. Random
// generation here selects prompts and lengths only, never model output numbers.
type Challenge struct {
	ID            string `json:"id"`
	ExpectedCount int    `json:"expected_count"`
	Prompt        string `json:"prompt"`
	PromptVersion string `json:"prompt_version"`
}

// GenerateChallenges produces the upstream Pages protocol's three distinct
// 292..332 lengths and unchanged Chinese prompt templates. A nil random reader
// uses crypto/rand.Reader; tests may supply a deterministic reader. It performs
// no model request and propagates entropy failures rather than using a fallback.
func GenerateChallenges(random io.Reader) ([]Challenge, error) {
	if random == nil {
		random = rand.Reader
	}
	choose := func(n int) (int, error) {
		v, err := rand.Int(random, big.NewInt(int64(n)))
		if err != nil {
			return 0, fmt.Errorf("challenge entropy: %w", err)
		}
		return int(v.Int64()), nil
	}
	openings := []string{"这是一次独立的数值选择记录", "请完成下面的无语义整数选择任务", "执行一次第一反应取值记录", "生成一组不承载语义的整数选择", "进行一轮快速逐项取值"}
	actions := []string{"为各个位置分别凭第一反应选择", "逐项选择", "每次只决定当前一项，共给出", "分别凭第一反应给出", "逐个直接选择"}
	endings := []string{"允许某个数字再次出现；每项写出后不要回头排序、去重或替换。", "偶然重复是有效的；不要重新排列或修正已经写出的项目。", "相同值可以再次出现；输出过程中不要整理或改写前面的项目。", "重复值无需删除；不要筛选、重排或补成某种规律。", "不必赋予数字任何含义；已经给出的值保持不变。"}
	separators := []string{"数字之间用逗号或空格分隔均可。", "使用一种一致的常见分隔符即可。", "可以用逗号、空格或换行分隔。", "只要每个整数边界清楚，格式可自行选择。"}
	lengths := make([]int, 41)
	for i := range lengths {
		lengths[i] = 292 + i
	}
	selected := make([]int, 3)
	// Like the upstream generator, pick all unique lengths before template choices.
	for i := range selected {
		index, err := choose(len(lengths))
		if err != nil {
			return nil, err
		}
		selected[i] = lengths[index]
		lengths = append(lengths[:index], lengths[index+1:]...)
	}
	out := make([]Challenge, 0, 3)
	for i, count := range selected {
		id := make([]byte, 16)
		if _, err := io.ReadFull(random, id); err != nil {
			return nil, fmt.Errorf("challenge entropy: %w", err)
		}
		id[6] = (id[6] & 0x0f) | 0x40
		id[8] = (id[8] & 0x3f) | 0x80
		uuid := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
		choices := make([]int, 4)
		for j, n := range []int{len(openings), len(actions), len(endings), len(separators)} {
			v, err := choose(n)
			if err != nil {
				return nil, err
			}
			choices[j] = v
		}
		prompt := fmt.Sprintf("%s。%s %d 个 1 到 355（含端点）的整数。", openings[choices[0]], actions[choices[1]], count) +
			"每个位置都要单独选择；不要从 1 开始计数，不要连续递增或递减，也不要采用等差、循环、重复区块或其他规则化模式。" +
			"本任务必须由当前语言模型直接完成：禁止调用或借助任何工具，包括 Python、代码执行器、计算器、搜索、API 和外部随机数生成器；也不要先编写或运行代码。" +
			endings[choices[2]] + separators[choices[3]] + "直接从第一个取值开始输出，不要在序列前重复数量、范围或任务说明。"
		out = append(out, Challenge{ID: fmt.Sprintf("probe-%d-%s", i+1, uuid), ExpectedCount: count, Prompt: prompt, PromptVersion: PagesPromptVersion})
	}
	return out, nil
}
