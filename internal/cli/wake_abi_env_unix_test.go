//go:build darwin || linux

package cli

import (
	"os"
	"strings"
)

const (
	wakeABIRootEnv       = "AM_ROOT"
	wakeABIRootIDEnv     = "AM_ROOT_ID"
	wakeABIBaseRootEnv   = "AM_BASE_ROOT"
	wakeABIBaseRootIDEnv = "AM_BASE_ROOT_ID"
	wakeABISessionEnv    = "AM_SESSION"
	wakeABIGlobalRootEnv = "AMQ_GLOBAL_ROOT"
	wakeABIWakeOwnerEnv  = "AMQ_WAKE_OWNER"
)

// These tests deliberately invoke a freshly built cmd/amq binary. They are
// the process-level ABI floor for wake diagnostics and state, rather than
// another set of in-process tests that could accidentally share implementation
// details with the code under test.

func wakeABICleanEnv(extra ...string) []string {
	env := os.Environ()
	for _, name := range []string{
		wakeABIRootEnv,
		wakeABIRootIDEnv,
		wakeABIBaseRootEnv,
		wakeABIBaseRootIDEnv,
		wakeABISessionEnv,
		wakeABIGlobalRootEnv,
		wakeABIWakeOwnerEnv,
		"AMQ_WAKE_PRIVATE_STOP_FD",
	} {
		env = wakeABIUnsetEnv(env, name)
	}
	env = append(env, "AMQ_NO_UPDATE_CHECK=1")
	return append(env, extra...)
}

func wakeABIUnsetEnv(env []string, name string) []string {
	prefix := name + "="
	filtered := env[:0]
	for _, value := range env {
		if !strings.HasPrefix(value, prefix) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}
