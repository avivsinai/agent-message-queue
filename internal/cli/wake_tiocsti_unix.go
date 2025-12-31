//go:build darwin || linux

package cli

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// tiocsti provides TIOCSTI terminal injection on Unix systems.
var tiocsti = tiocstiFuncs{
	available: true,
}

type tiocstiFuncs struct {
	available bool
}

// Available returns true if TIOCSTI is supported on this platform.
func (t tiocstiFuncs) Available() bool {
	return t.available
}

// IsTTY returns true if stdin is a terminal.
func (t tiocstiFuncs) IsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// Inject injects text into the terminal input buffer using TIOCSTI.
// This makes the text appear as if the user typed it.
func (t tiocstiFuncs) Inject(text string) error {
	// Open /dev/tty for the controlling terminal
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = tty.Close() }()

	fd := tty.Fd()

	// Inject each character using TIOCSTI
	for _, ch := range []byte(text) {
		if err := ioctlTIOCSTI(fd, ch); err != nil {
			return err
		}
	}

	return nil
}

func ioctlTIOCSTI(fd uintptr, ch byte) error {
	// TIOCSTI expects a pointer to a single byte
	// No clean wrapper exists in unix package, so use Syscall directly
	// Retry on EINTR (interrupted system call)
	for {
		//nolint:staticcheck // unix.SYS_IOCTL is deprecated but no alternative for TIOCSTI
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, unix.TIOCSTI, uintptr(unsafe.Pointer(&ch)))
		if errno == 0 {
			return nil
		}
		if errno == unix.EINTR {
			continue // retry on interrupt
		}
		return errno
	}
}
