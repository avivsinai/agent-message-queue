package cli

import (
	"path/filepath"
	"strings"
)

// absoluteSessionRoot resolves root against the current working directory at
// session start. This is the single point where a coop session's mailbox
// identity is frozen; everything downstream must see an absolute path.
func absoluteSessionRoot(root string) (string, error) {
	return filepath.Abs(root)
}

func coopSessionIdentity(root, requestedSession, requestedRoot string) string {
	if requestedSession != "" {
		return requestedSession
	}
	if requestedRoot == "" {
		return defaultSessionName
	}
	return inferredSessionIdentity(root)
}

func buildCoopExecEnvironment(base []string, root, me, session string) []string {
	// AMQ_WAKE_OWNER is internal process-identity metadata. Never pass an
	// ambient token into a new coop process; deferred launch replaces it with
	// the exact identity captured immediately before exec.
	env := unsetEnvVar(base, envWakeOwner)
	env = setEnvVar(env, envRoot, root)
	env = setEnvVar(env, envMe, me)
	baseRoot := root
	if session != "" {
		baseRoot = filepath.Dir(root)
	}
	env = setEnvVar(env, envBaseRoot, baseRoot)
	env = setEnvVar(env, envSession, session)
	rootID, baseRootID := treeIdentityTokens(root, baseRoot)
	env = setOrUnsetEnvVar(env, envRootID, rootID)
	return setOrUnsetEnvVar(env, envBaseRootID, baseRootID)
}

func setOrUnsetEnvVar(env []string, key, value string) []string {
	if value == "" {
		return unsetEnvVar(env, key)
	}
	return setEnvVar(env, key, value)
}

// setEnvVar sets or replaces an environment variable in a slice.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func unsetEnvVar(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}
