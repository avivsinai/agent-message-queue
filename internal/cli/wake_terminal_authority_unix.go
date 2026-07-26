//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type wakeTerminalAuthorityLossKind uint8

const (
	wakeTerminalAuthorityLossUnknown wakeTerminalAuthorityLossKind = iota
	wakeTerminalAuthorityLossControlStopped
)

type wakeTerminalAuthorityLossError struct {
	Kind   wakeTerminalAuthorityLossKind
	Reason string
	Err    error
}

func (err *wakeTerminalAuthorityLossError) Error() string {
	if err.Err != nil {
		return fmt.Sprintf("wake terminal authority lost: %s: %v", err.Reason, err.Err)
	}
	return "wake terminal authority lost: " + err.Reason
}

func (err *wakeTerminalAuthorityLossError) Unwrap() error {
	return err.Err
}

func isWakeTerminalAuthorityLoss(err error) bool {
	var loss *wakeTerminalAuthorityLossError
	return errors.As(err, &loss)
}

func isWakeTerminalControlStopped(err error) bool {
	var loss *wakeTerminalAuthorityLossError
	return errors.As(err, &loss) &&
		loss.Kind == wakeTerminalAuthorityLossControlStopped
}

var (
	openWakeControllingTerminal = func() (*os.File, error) {
		fd, err := unix.Open(
			"/dev/tty",
			unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC,
			0,
		)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), "/dev/tty"), nil
	}
	wakeTerminalForegroundPGRP = func(fd uintptr) (int, error) {
		return unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	}
	inspectWakeTerminalGeneration = inspectWakeLock
	injectWakeTerminalFD          = func(fd uintptr, text string) error {
		return tiocsti.InjectFD(fd, text)
	}
	bindWakeTerminalAuthorityForWake = bindWakeTerminalAuthority
)

type wakeTerminalAuthority struct {
	mu sync.Mutex

	tty            *os.File
	fd             uintptr
	identity       wakeTerminalIdentity
	foregroundPGRP int
	generation     wakeLockInspection
	controlStop    <-chan struct{}
	closed         bool
}

type wakeTerminalIdentity struct {
	Device uint64
	Inode  uint64
}

func captureWakeTerminalIdentity(info os.FileInfo) (wakeTerminalIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return wakeTerminalIdentity{}, false
	}
	return wakeTerminalIdentity{
		Device: uint64(stat.Dev),
		Inode:  uint64(stat.Ino),
	}, true
}

func matchesWakeTerminalIdentity(identity wakeTerminalIdentity, info os.FileInfo) bool {
	current, ok := captureWakeTerminalIdentity(info)
	return ok && identity == current
}

func bindWakeTerminalAuthority(
	generation wakeLockInspection,
	controlStop <-chan struct{},
) (*wakeTerminalAuthority, error) {
	if controlStop == nil {
		return nil, newWakeTerminalAuthorityLoss("control-stop capability is missing", nil)
	}
	select {
	case <-controlStop:
		return nil, newWakeTerminalAuthorityLoss("control-stop capability is already closed", nil)
	default:
	}
	if !generation.Exists ||
		generation.fileInfo == nil ||
		generation.Lock.Generation == "" ||
		generation.Root == "" ||
		generation.Agent == "" {
		return nil, newWakeTerminalAuthorityLoss("exact wake generation is unavailable", nil)
	}
	current := inspectWakeTerminalGeneration(generation.Root, generation.Agent)
	if !sameWakeLockGeneration(generation, current) {
		return nil, newWakeTerminalAuthorityLoss("wake generation changed before terminal binding", nil)
	}

	tty, err := openWakeControllingTerminal()
	if err != nil {
		return nil, newWakeTerminalAuthorityLoss("open controlling terminal", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = tty.Close()
		}
	}()

	info, err := tty.Stat()
	if err != nil {
		return nil, newWakeTerminalAuthorityLoss("inspect retained controlling terminal", err)
	}
	identity, ok := captureWakeTerminalIdentity(info)
	if !ok {
		return nil, newWakeTerminalAuthorityLoss("capture retained controlling-terminal identity", nil)
	}
	fd := tty.Fd()
	foregroundPGRP, err := wakeTerminalForegroundPGRP(fd)
	if err != nil {
		return nil, newWakeTerminalAuthorityLoss("inspect controlling-terminal foreground process group", err)
	}
	if foregroundPGRP <= 0 {
		return nil, newWakeTerminalAuthorityLoss("controlling-terminal foreground process group is invalid", nil)
	}

	keep = true
	return &wakeTerminalAuthority{
		tty:            tty,
		fd:             fd,
		identity:       identity,
		foregroundPGRP: foregroundPGRP,
		generation:     generation,
		controlStop:    controlStop,
	}, nil
}

func (authority *wakeTerminalAuthority) BeforeWrite() error {
	if authority == nil {
		return newWakeTerminalAuthorityLoss("terminal capability is missing", nil)
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.validateLocked()
}

func (authority *wakeTerminalAuthority) Inject(text string) error {
	if authority == nil {
		return newWakeTerminalAuthorityLoss("terminal capability is missing", nil)
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if err := authority.validateLocked(); err != nil {
		return err
	}
	if err := injectWakeTerminalFD(authority.fd, text); err != nil {
		var injectionErr *tiocstiInjectionError
		if errors.As(err, &injectionErr) &&
			injectionErr.Progress == 0 &&
			(errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EPERM)) {
			// EIO and EPERM are ambiguous: the kernel may reject TIOCSTI, or
			// the terminal authority may have changed. Only classify the
			// injector after every retained/current identity and PGRP check
			// still passes after the failed zero-progress ioctl.
			if validateErr := authority.validateLocked(); validateErr != nil {
				return validateErr
			}
			return newWakeInjectorUnsupportedError(err)
		}
		return newWakeTerminalAuthorityLoss("inject through retained controlling terminal", err)
	}
	return nil
}

func (authority *wakeTerminalAuthority) validateLocked() error {
	if authority.closed || authority.tty == nil {
		return newWakeTerminalAuthorityLoss("retained controlling terminal is closed", nil)
	}
	select {
	case <-authority.controlStop:
		return newWakeTerminalControlStoppedLoss()
	default:
	}

	currentGeneration := inspectWakeTerminalGeneration(
		authority.generation.Root,
		authority.generation.Agent,
	)
	if !sameWakeLockGeneration(authority.generation, currentGeneration) {
		return newWakeTerminalAuthorityLoss("wake generation changed", nil)
	}
	if authority.tty.Fd() != authority.fd {
		return newWakeTerminalAuthorityLoss("retained controlling-terminal descriptor changed", nil)
	}
	retainedInfo, err := authority.tty.Stat()
	if err != nil {
		return newWakeTerminalAuthorityLoss("inspect retained controlling terminal", err)
	}
	if !matchesWakeTerminalIdentity(authority.identity, retainedInfo) {
		return newWakeTerminalAuthorityLoss("retained controlling-terminal identity changed", nil)
	}

	currentTTY, err := openWakeControllingTerminal()
	if err != nil {
		return newWakeTerminalAuthorityLoss("re-open current controlling terminal", err)
	}
	currentInfo, statErr := currentTTY.Stat()
	closeErr := currentTTY.Close()
	if statErr != nil {
		return newWakeTerminalAuthorityLoss("inspect current controlling terminal", statErr)
	}
	if closeErr != nil {
		return newWakeTerminalAuthorityLoss("close current controlling-terminal check", closeErr)
	}
	if !matchesWakeTerminalIdentity(authority.identity, currentInfo) {
		return newWakeTerminalAuthorityLoss("current controlling-terminal identity changed", nil)
	}

	foregroundPGRP, err := wakeTerminalForegroundPGRP(authority.fd)
	if err != nil {
		return newWakeTerminalAuthorityLoss("recheck controlling-terminal foreground process group", err)
	}
	if foregroundPGRP != authority.foregroundPGRP {
		return newWakeTerminalAuthorityLoss(
			fmt.Sprintf(
				"controlling-terminal foreground process group changed from %d to %d",
				authority.foregroundPGRP,
				foregroundPGRP,
			),
			nil,
		)
	}
	return nil
}

func (authority *wakeTerminalAuthority) Close() error {
	if authority == nil {
		return nil
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return nil
	}
	authority.closed = true
	if authority.tty == nil {
		return nil
	}
	err := authority.tty.Close()
	authority.tty = nil
	return err
}

func newWakeTerminalAuthorityLoss(reason string, err error) error {
	return &wakeTerminalAuthorityLossError{
		Kind:   wakeTerminalAuthorityLossUnknown,
		Reason: reason,
		Err:    err,
	}
}

func newWakeTerminalControlStoppedLoss() error {
	return &wakeTerminalAuthorityLossError{
		Kind:   wakeTerminalAuthorityLossControlStopped,
		Reason: "wake control stopped",
	}
}
