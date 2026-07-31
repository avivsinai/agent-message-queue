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
	wakeTerminalAuthorityLossForegroundPGRPChanged
)

type wakeTerminalAuthorityLossError struct {
	Kind   wakeTerminalAuthorityLossKind
	Reason string
}

type wakeTerminalTransientError struct {
	Reason string
	Err    error
}

func (err *wakeTerminalTransientError) Error() string {
	if err.Err != nil {
		return fmt.Sprintf("wake terminal temporarily unavailable: %s: %v", err.Reason, err.Err)
	}
	return "wake terminal temporarily unavailable: " + err.Reason
}

func (err *wakeTerminalTransientError) Unwrap() error {
	return err.Err
}

func (err *wakeTerminalAuthorityLossError) Error() string {
	return "wake terminal authority lost: " + err.Reason
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

func isWakeTerminalForegroundPGRPChanged(err error) bool {
	var loss *wakeTerminalAuthorityLossError
	return errors.As(err, &loss) &&
		loss.Kind == wakeTerminalAuthorityLossForegroundPGRPChanged
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
		return nil, fmt.Errorf("bind wake terminal authority: control-stop capability is missing")
	}
	select {
	case <-controlStop:
		return nil, fmt.Errorf("bind wake terminal authority: control-stop capability is already closed")
	default:
	}
	if !generation.Exists ||
		generation.fileInfo == nil ||
		generation.Lock.Generation == "" ||
		generation.Root == "" ||
		generation.Agent == "" {
		return nil, fmt.Errorf("bind wake terminal authority: exact wake generation is unavailable")
	}
	current := inspectWakeTerminalGeneration(generation.Root, generation.Agent)
	if !current.Exists {
		return nil, fmt.Errorf("bind wake terminal authority: wake generation disappeared")
	}
	if current.fileInfo == nil {
		return nil, fmt.Errorf(
			"inspect wake generation before terminal binding: %w",
			wakeTerminalGenerationInspectionError(current),
		)
	}
	if !sameWakeLockGeneration(generation, current) {
		return nil, fmt.Errorf("bind wake terminal authority: wake generation changed")
	}

	tty, err := openWakeControllingTerminal()
	if err != nil {
		return nil, fmt.Errorf("open controlling terminal for wake binding: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = tty.Close()
		}
	}()

	info, err := tty.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect retained controlling terminal for wake binding: %w", err)
	}
	identity, ok := captureWakeTerminalIdentity(info)
	if !ok {
		return nil, fmt.Errorf("capture retained controlling-terminal identity for wake binding")
	}
	fd := tty.Fd()
	foregroundPGRP, err := wakeTerminalForegroundPGRP(fd)
	if err != nil {
		return nil, fmt.Errorf("inspect controlling-terminal foreground process group for wake binding: %w", err)
	}
	if foregroundPGRP <= 0 {
		return nil, fmt.Errorf("bind wake terminal authority: controlling-terminal foreground process group is invalid")
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
		return newWakeTerminalAuthorityLoss("terminal capability is missing")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.validateLocked()
}

func (authority *wakeTerminalAuthority) Inject(text string) error {
	if authority == nil {
		return newWakeTerminalAuthorityLoss("terminal capability is missing")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if err := authority.validateLocked(); err != nil {
		return err
	}
	if err := injectWakeTerminalFD(authority.fd, text); err != nil {
		var injectionErr *tiocstiInjectionError
		if errors.As(err, &injectionErr) {
			// Every TIOCSTI error is ambiguous until the retained authority is
			// revalidated. Once authority still holds, preserve unclassified
			// errors and accepted-byte progress for the loop's bounded retry
			// policy instead of relabeling an effect failure as ownership loss.
			if validateErr := authority.validateLocked(); validateErr != nil {
				return validateErr
			}
		}
		if injectionErr != nil &&
			injectionErr.Progress == 0 &&
			(errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EPERM)) {
			return newWakeInjectorUnsupportedError(err)
		}
		return err
	}
	return nil
}

func (authority *wakeTerminalAuthority) validateLocked() error {
	if authority.closed || authority.tty == nil {
		return newWakeTerminalAuthorityLoss("retained controlling terminal is closed")
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
	if !currentGeneration.Exists {
		return newWakeTerminalAuthorityLoss("wake generation disappeared")
	}
	if currentGeneration.fileInfo == nil {
		return newWakeTerminalTransientFailure(
			"inspect current wake generation",
			wakeTerminalGenerationInspectionError(currentGeneration),
		)
	}
	if !sameWakeLockGeneration(authority.generation, currentGeneration) {
		return newWakeTerminalAuthorityLoss("wake generation changed")
	}
	if authority.tty.Fd() != authority.fd {
		return newWakeTerminalAuthorityLoss("retained controlling-terminal descriptor changed")
	}
	retainedInfo, err := authority.tty.Stat()
	if err != nil {
		return newWakeTerminalTransientFailure("inspect retained controlling terminal", err)
	}
	if !matchesWakeTerminalIdentity(authority.identity, retainedInfo) {
		return newWakeTerminalAuthorityLoss("retained controlling-terminal identity changed")
	}

	currentTTY, err := openWakeControllingTerminal()
	if err != nil {
		return newWakeTerminalTransientFailure("re-open current controlling terminal", err)
	}
	currentInfo, statErr := currentTTY.Stat()
	closeErr := currentTTY.Close()
	if statErr != nil {
		return newWakeTerminalTransientFailure("inspect current controlling terminal", statErr)
	}
	if closeErr != nil {
		return newWakeTerminalTransientFailure("close current controlling-terminal check", closeErr)
	}
	if !matchesWakeTerminalIdentity(authority.identity, currentInfo) {
		return newWakeTerminalAuthorityLoss("current controlling-terminal identity changed")
	}

	foregroundPGRP, err := wakeTerminalForegroundPGRP(authority.fd)
	if err != nil {
		return newWakeTerminalTransientFailure(
			"recheck controlling-terminal foreground process group",
			err,
		)
	}
	if foregroundPGRP != authority.foregroundPGRP {
		return newWakeTerminalForegroundPGRPChangedLoss(
			authority.foregroundPGRP,
			foregroundPGRP,
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

func wakeTerminalGenerationInspectionError(inspection wakeLockInspection) error {
	if inspection.Reason != "" {
		return errors.New(inspection.Reason)
	}
	return fmt.Errorf(
		"wake lock status %q has no readable file identity",
		inspection.Status,
	)
}

func newWakeTerminalAuthorityLoss(reason string) error {
	return &wakeTerminalAuthorityLossError{
		Kind:   wakeTerminalAuthorityLossUnknown,
		Reason: reason,
	}
}

func newWakeTerminalTransientFailure(reason string, err error) error {
	return &wakeTerminalTransientError{
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

func newWakeTerminalForegroundPGRPChangedLoss(expected, current int) error {
	return &wakeTerminalAuthorityLossError{
		Kind: wakeTerminalAuthorityLossForegroundPGRPChanged,
		Reason: fmt.Sprintf(
			"controlling-terminal foreground process group changed from %d to %d",
			expected,
			current,
		),
	}
}
