//go:build windows

package aetherrelay

import "errors"

// Windows 目前不实现 TTY 密码读取：admin password-hash / set-credentials
// 返回 "requires an interactive TTY" 错误，服务本身不受影响。
// 若需要，可改用 golang.org/x/sys/windows 的 console API 实现。

func isTerminal(fd int) bool {
	return false
}

func readPassword(fd int) ([]byte, error) {
	return nil, errors.New("password input is not supported on windows")
}
