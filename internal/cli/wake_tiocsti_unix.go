//go:build darwin || linux

package cli

import (
	"os"
	"runtime"
	"strings"
	"time"
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

var readTIOCSTILegacySysctl = func() ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(tiocstiLegacySysctlPath)
}

type tiocstiInjectionError struct {
	Err      error
	Progress int
}

func (err *tiocstiInjectionError) Error() string {
	return err.Err.Error()
}

func (err *tiocstiInjectionError) Unwrap() error {
	return err.Err
}

func (err *tiocstiInjectionError) wakeAcceptedBytes() int {
	return err.Progress
}

func tiocstiLegacyDisabledHint() bool {
	data, err := readTIOCSTILegacySysctl()
	return err == nil && strings.TrimSpace(string(data)) == "0"
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

	return t.InjectFD(tty.Fd(), text)
}

// InjectFD injects text through an already-retained controlling-terminal
// descriptor. Callers own the descriptor and its lifecycle.
func (t tiocstiFuncs) InjectFD(fd uintptr, text string) error {
	// Inject each character using TIOCSTI
	for progress, ch := range []byte(text) {
		if err := ioctlTIOCSTI(fd, ch); err != nil {
			return &tiocstiInjectionError{Err: err, Progress: progress}
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

func waitForTTYInputQuiet(cfg *wakeConfig) bool {
	if cfg.inputMaxHold <= 0 {
		return true
	}

	queueFD, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		if cfg.debug {
			_ = writeWakeDiagnostic(cfg, "amq wake [debug]: input deferral unavailable: open /dev/tty: %v\n", err)
		}
		return true
	}
	defer func() { _ = unix.Close(queueFD) }()

	// Atime via /dev/tty is unreliable: on macOS the alias inode does not track
	// reads on the underlying ttysNNN, and on Linux a freshly opened /dev/tty fd
	// may not be in the tty's open-file list when tty_update_time runs. Stdin
	// (when a TTY) was inherited from the launching shell and tracks reads on
	// both platforms, so prefer it for atime sampling.
	atimeFD := uintptr(queueFD)
	atimeSource := "/dev/tty"
	if stdinFD := os.Stdin.Fd(); term.IsTerminal(int(stdinFD)) {
		atimeFD = stdinFD
		atimeSource = "stdin"
	}
	if cfg.debug {
		_ = writeWakeDiagnostic(cfg, "amq wake [debug]: input deferral atime_source=%s\n", atimeSource)
	}

	allowInjection, activeReason, sampleErr := waitForInputQuiet(
		func() (ttyInputState, error) {
			return sampleTTYInputState(uintptr(queueFD), atimeFD)
		},
		time.Now,
		func(delay time.Duration, state ttyInputState, reason string) {
			if cfg.debug {
				_ = writeWakeDiagnostic(
					cfg,
					"amq wake [debug]: deferring injection for %s (%s, pending_bytes=%d)\n",
					delay,
					reason,
					state.pendingBytes,
				)
			}
			time.Sleep(delay)
		},
		cfg.inputQuietFor,
		cfg.inputMaxHold,
		cfg.inputPollInterval,
	)
	if sampleErr != nil && cfg.debug {
		_ = writeWakeDiagnostic(cfg, "amq wake [debug]: input deferral unavailable: %v\n", sampleErr)
	}
	if !allowInjection && cfg.debug {
		_ = writeWakeDiagnostic(cfg, "amq wake [debug]: input deferral max hold reached (%s)\n", activeReason)
	}
	return allowInjection
}

func waitForTTYInputDrain(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
	queueFD, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = unix.Close(queueFD) }()

	return waitForInputQueueDrain(func() (int, error) {
		return ttyInputPendingBytes(uintptr(queueFD))
	}, time.Now, time.Sleep, timeout, pollInterval)
}

func sampleTTYInputState(queueFD, atimeFD uintptr) (ttyInputState, error) {
	// TIOCINQ/FIONREAD only sees bytes still buffered by the kernel. Raw-mode
	// TUIs usually consume keystrokes quickly, so recent tty reads are the more
	// useful signal for active composition.
	pending, err := ttyInputPendingBytes(queueFD)
	if err != nil {
		return ttyInputState{}, err
	}

	readAt, ok, err := ttyLastReadTime(atimeFD)
	if err != nil {
		return ttyInputState{}, err
	}
	return ttyInputState{
		pendingBytes: pending,
		lastRead:     readAt,
		hasLastRead:  ok,
	}, nil
}

func ttyInputPendingBytes(fd uintptr) (int, error) {
	var n int32
	for {
		//nolint:staticcheck // unix.SYS_IOCTL is used for terminal ioctls without wrappers.
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(ttyInputQueueRequest), uintptr(unsafe.Pointer(&n)))
		if errno == 0 {
			if n < 0 {
				return 0, nil
			}
			return int(n), nil
		}
		if errno == unix.EINTR {
			continue
		}
		return 0, errno
	}
}

func ttyLastReadTime(fd uintptr) (time.Time, bool, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(fd), &st); err != nil {
		return time.Time{}, false, err
	}
	if st.Atim.Sec == 0 && st.Atim.Nsec == 0 {
		return time.Time{}, false, nil
	}
	return time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec)), true, nil
}
