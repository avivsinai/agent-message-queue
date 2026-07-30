//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const envWakeAttentionFD = "AMQ_WAKE_ATTENTION_FD"

// attachWakeAttentionFD gives the wake child an isolated attention destination
// without disturbing capabilities already assigned through ExtraFiles.
func attachWakeAttentionFD(cmd *exec.Cmd, attention *os.File) error {
	if cmd == nil {
		return fmt.Errorf("wake attention command is missing")
	}
	if attention == nil {
		return fmt.Errorf("wake attention file is missing")
	}

	childFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, attention)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = setEnvVar(
		unsetEnvVar(cmd.Env, envWakeAttentionFD),
		envWakeAttentionFD,
		strconv.Itoa(childFD),
	)
	return nil
}

// wakeAttentionFromEnv adopts the descriptor transferred by the coop parent.
// A present environment value is authoritative: malformed or unavailable
// descriptors fail closed instead of falling back to the diagnostic stream.
func wakeAttentionFromEnv() (*os.File, error) {
	raw, present := os.LookupEnv(envWakeAttentionFD)
	if !present {
		return nil, nil
	}
	if err := os.Unsetenv(envWakeAttentionFD); err != nil {
		return nil, fmt.Errorf("clear %s after ingestion: %w", envWakeAttentionFD, err)
	}

	fd, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("%s is invalid", envWakeAttentionFD)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return nil, fmt.Errorf("inspect %s fd: %w", envWakeAttentionFD, err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("make %s fd close-on-exec: %w", envWakeAttentionFD, err)
		}
	}
	sealedFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("verify %s fd flags: %w", envWakeAttentionFD, err)
	}
	if sealedFlags&unix.FD_CLOEXEC == 0 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%s fd is not close-on-exec", envWakeAttentionFD)
	}

	attention := os.NewFile(uintptr(fd), "amq-wake-attention")
	if attention == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%s fd is unavailable", envWakeAttentionFD)
	}
	return attention, nil
}

func wakeAttentionFileIsTerminal(attention *os.File) bool {
	return attention != nil && term.IsTerminal(int(attention.Fd()))
}
