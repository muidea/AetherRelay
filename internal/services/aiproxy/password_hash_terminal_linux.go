//go:build linux

package aiproxy

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// isTerminal 与 readPassword 的 Linux 实现（TCGETS/TCSETS）。
// darwin 见 password_hash_terminal_darwin.go，Windows 见
// password_hash_terminal_windows.go。

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

func readPassword(fd int) ([]byte, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Lflag &^= unix.ECHO
	raw.Lflag |= unix.ICANON | unix.ISIG
	raw.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }()

	reader := bufio.NewReader(os.NewFile(uintptr(fd), "/dev/stdin"))
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	return []byte(line), nil
}
