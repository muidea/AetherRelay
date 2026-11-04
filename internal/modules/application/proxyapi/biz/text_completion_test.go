package biz

import completion "aetherrelay/internal/pkg/aetherrelaycompletion"

func textEvidence(text string) completion.Text {
	return completion.Text{Completed: true, Reason: completion.Stop, Terminal: "done", TextBytes: len(text), OutputBytes: len(text), TextSHA256: completion.Hash(text)}
}
