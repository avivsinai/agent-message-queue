//go:build darwin || linux

package terminalrelay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Run relays the calling process's terminal to target and returns its shell
// exit status. The calling process keeps its PID, session and controlling
// terminal; output is consumed only by the supplied writer.
func Run(binaryPath string, argv, env []string, output io.Writer) (code int, err error) {
	if len(argv) == 0 {
		return 1, fmt.Errorf("empty relay target argv")
	}
	executable, err := os.Executable()
	if err != nil {
		return 1, err
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	defer func() { _ = controlRead.Close() }()
	defer func() { _ = controlWrite.Close() }()
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	defer func() { _ = statusRead.Close() }()
	defer func() { _ = statusWrite.Close() }()
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP)
	defer signal.Stop(signals)
	continued := make(chan os.Signal, 1)
	signal.Notify(continued, syscall.SIGCONT)
	defer signal.Stop(continued)
	stdin := int(os.Stdin.Fd())
	if err := awaitForeground(stdin, continued); err != nil {
		return 1, err
	}
	saved, err := term.MakeRaw(stdin)
	if err != nil {
		return 1, fmt.Errorf("put terminal in raw mode: %w", err)
	}
	defer func() {
		if restoreErr := term.Restore(stdin, saved); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore outer terminal: %w", restoreErr))
		}
	}()
	args := append([]string{HelperCommand, binaryPath}, argv...)
	cmd := exec.Command(executable, args...)
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{controlRead, statusWrite}
	size, _ := pty.GetsizeFull(os.Stdin)
	master, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return 1, fmt.Errorf("start terminal relay helper: %w", err)
	}
	_ = controlRead.Close()
	_ = statusWrite.Close()
	pollMaster, err := pollableMaster(master)
	if err != nil {
		_ = controlWrite.Close()
		_ = master.Close()
		_ = cmd.Wait()
		return 1, fmt.Errorf("make relay PTY cancellable: %w", err)
	}
	master = pollMaster
	commands := json.NewEncoder(controlWrite)
	events := make(chan event)
	go func() {
		defer close(events)
		decoder := json.NewDecoder(statusRead)
		for {
			var update event
			if decoder.Decode(&update) != nil {
				return
			}
			events <- update
		}
	}()
	go func() { _, _ = io.Copy(master, os.Stdin) }()
	drained := make(chan struct{})
	var outputErr error
	go func() { defer close(drained); _, outputErr = io.Copy(output, terminalOutput{master}) }()
	code, exited := 1, false
	defer func() {
		_ = master.Close()
		// No writer/parser call can outlive Run or race the caller's sink close.
		<-drained
		if outputErr != nil && !errors.Is(err, outputErr) {
			err = errors.Join(err, fmt.Errorf("relay output: %w", outputErr))
		}
		if err == nil && exited {
			// Only this explicit normal completion preserves background children.
			// EOF without it orders bounded group termination, even if the
			// original group leader has already exited.
			if releaseErr := commands.Encode(syscall.Signal(0)); releaseErr != nil {
				err = fmt.Errorf("release terminal relay helper: %w", releaseErr)
			}
		}
		_ = controlWrite.Close()
		waitErr := cmd.Wait()
		if err == nil && (cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != code) {
			err = fmt.Errorf("terminal relay helper status differs from target: %v", waitErr)
		}
	}()
	outputReady := (<-chan struct{})(drained)
	for {
		select {
		case <-outputReady:
			outputReady = nil
			if outputErr != nil {
				return 1, fmt.Errorf("relay output: %w", outputErr)
			}
		case sig := <-signals:
			switch sig {
			case syscall.SIGWINCH:
				_ = pty.InheritSize(os.Stdin, master)
			default:
				if err := commands.Encode(sig.(syscall.Signal)); err != nil {
					return 1, fmt.Errorf("signal relay target: %w", err)
				}
			}
		case update, ok := <-events:
			if !ok {
				if !exited {
					return 1, fmt.Errorf("terminal relay helper exited without target status")
				}
				return 1, fmt.Errorf("terminal relay helper exited before normal release")
			}
			if update.Error != "" {
				return 1, fmt.Errorf("%s", update.Error)
			}
			switch update.State {
			case "stopped":
				if err := term.Restore(stdin, saved); err != nil {
					return 1, err
				}
				// SIGSTOP cannot be discarded because of an inherited ignored
				// disposition or an orphaned outer process group.
				if err := suspendJob(continued); err != nil {
					return 1, err
				}
				if err := awaitForeground(stdin, continued); err != nil {
					return 1, err
				}
				if _, err := term.MakeRaw(stdin); err != nil {
					return 1, err
				}
				_ = pty.InheritSize(os.Stdin, master)
				if err := commands.Encode(syscall.SIGCONT); err != nil {
					return 1, err
				}
			case "exit":
				code, exited = update.Code, true
				select {
				case <-drained:
				case <-time.After(200 * time.Millisecond):
				}
				return code, nil
			}
		}
	}
}

func awaitForeground(fd int, continued <-chan os.Signal) error {
	for {
		foreground, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		if err != nil {
			return fmt.Errorf("read terminal foreground group: %w", err)
		}
		if foreground == syscall.Getpgrp() {
			return nil
		}
		// A bg continuation must not change the shell's terminal or resume
		// the inner job. Stop again until the shell gives this job the tty.
		if err := suspendJob(continued); err != nil {
			return err
		}
	}
}

func suspendJob(continued <-chan os.Signal) error {
	for {
		select {
		case <-continued:
			continue
		default:
		}
		break
	}
	if err := syscall.Kill(-syscall.Getpgrp(), syscall.SIGSTOP); err != nil {
		return err
	}
	// Self group-stop delivery can finish after Kill returns on another Go
	// thread. Only SIGCONT permits terminal changes and target continuation.
	<-continued
	return nil
}

type terminalOutput struct{ file *os.File }

func pollableMaster(master *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	// NewFile registers an already nonblocking descriptor with Go's poller,
	// so Close can interrupt a pending Read even while a slave stays open.
	file := os.NewFile(uintptr(fd), master.Name())
	_ = master.Close()
	return file, nil
}

func (r terminalOutput) Read(data []byte) (int, error) {
	n, err := r.file.Read(data)
	// PTY masters report EIO when the last slave closes; local close also
	// ends the bounded output drain. Writer errors remain untouched.
	if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
		err = io.EOF
	}
	return n, err
}
