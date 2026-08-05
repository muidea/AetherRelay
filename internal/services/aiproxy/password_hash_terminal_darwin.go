//go:build darwin

package aiproxy

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// isTerminal 与 readPassword 的 macOS 实现。darwin 的 termios ioctl 常量
// 是 TIOCGETA/TIOCSETA（Linux 的 TCGETS/TCSETS 不存在于 darwin）。

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	return err == nil
}

func readPassword(fd int) ([]byte, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Lflag &^= unix.ECHO
	raw.Lflag |= unix.ICANON | unix.ISIG
	raw.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &raw); err != nil {
		return nil, err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TIOCSETA, old) }()

	reader := bufio.NewReader(os.NewFile(uintptr(fd), "/dev/stdin"))
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	return []byte(line), nil
}
