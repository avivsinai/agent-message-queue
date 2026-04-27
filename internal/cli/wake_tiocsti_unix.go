//go:build darwin || linux

package cli

import (
	"os"
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

func waitForTTYInputQuiet(cfg *wakeConfig) {
	if cfg.inputMaxHold <= 0 {
		return
	}

	queueFD, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NOCTTY, 0)
	if err != nil {
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: input deferral unavailable: open /dev/tty: %v\n", err)
		}
		return
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
		_ = writeStderr("amq wake [debug]: input deferral atime_source=%s\n", atimeSource)
	}

	deadline := time.Now().Add(cfg.inputMaxHold)
	for {
		now := time.Now()
		state, err := sampleTTYInputState(uintptr(queueFD), atimeFD)
		if err != nil {
			if cfg.debug {
				_ = writeStderr("amq wake [debug]: input deferral unavailable: %v\n", err)
			}
			return
		}

		active, reason := state.active(now, cfg.inputQuietFor)
		if !active {
			return
		}
		if !now.Before(deadline) {
			if cfg.debug {
				_ = writeStderr("amq wake [debug]: input deferral max hold reached (%s)\n", reason)
			}
			return
		}

		delay := inputDeferralDelay(state, now, deadline, cfg.inputQuietFor, cfg.inputPollInterval)
		if delay <= 0 {
			return
		}
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: deferring injection for %s (%s, pending_bytes=%d)\n", delay, reason, state.pendingBytes)
		}
		time.Sleep(delay)
	}
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
	return time.Unix(st.Atim.Sec, st.Atim.Nsec), true, nil
}
