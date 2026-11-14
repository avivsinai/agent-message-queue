package launch

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const (
	codexIdentitySetPrefix   = "shell_environment_policy.set."
	codexIdentityPlaceholder = "__AMQ_IDENTITY__"
)

// CodexIdentityOverride is the value of the Codex -c override that sets key
// to value in the environment of Codex's shell tool. Codex reads the value
// as TOML, so it is always a basic string: an unquoted handle such as 7
// would become an integer.
func CodexIdentityOverride(key, value string) string {
	return codexIdentitySetPrefix + key + "=" + tomlBasicString(value)
}

func tomlBasicString(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// codexIdentitySlots returns the -c pairs a managed Codex plan carries for
// the identity, one dynamic slot per variable, which the launch wrapper
// fills from the environment coop exec built (ResolveExecutionArgv). The
// values stay out of the plan, so a changed root or token changes no trust
// digest.
func codexIdentitySlots(argv []string) ([]string, []DynamicArg) {
	var dynamic []DynamicArg
	for _, key := range IdentityEnvKeys {
		argv = append(argv, "-c", codexIdentitySetPrefix+key+"="+codexIdentityPlaceholder)
		dynamic = append(dynamic, DynamicArg{Index: len(argv) - 1, Kind: DynamicArgCodexIdentity})
	}
	// Mailboxes slots: one DynamicArg per literal satisfied at exec time.
	for i := 1; i < len(argv); i++ {
		if argv[i-1] == "-c" && argv[i] == codexMailboxesArgPrefix+codexMailboxesToken {
			dynamic = append(dynamic, DynamicArg{Index: i, Kind: DynamicArgCodexMailboxesPath})
		}
	}
	return argv, dynamic
}

// codexIdentitySlotKey returns the identity variable an unfilled slot names.
func codexIdentitySlotKey(argv []string, index int) (string, bool) {
	if index < 1 || index >= len(argv) || argv[index-1] != "-c" {
		return "", false
	}
	rest, ok := strings.CutPrefix(argv[index], codexIdentitySetPrefix)
	if !ok {
		return "", false
	}
	key, value, ok := strings.Cut(rest, "=")
	if !ok || value != codexIdentityPlaceholder || !slices.Contains(IdentityEnvKeys, key) {
		return "", false
	}
	return key, true
}

// fillCodexIdentitySlot returns the filled override for one slot from env.
// A seat whose environment lacks an identity variable does not start: its
// amq commands would have no identity, or a guessed one.
func fillCodexIdentitySlot(argv []string, index int, env []string) (string, error) {
	key, ok := codexIdentitySlotKey(argv, index)
	if !ok {
		return "", fmt.Errorf("codex identity slot %d is invalid", index)
	}
	prefix := key + "="
	for _, entry := range env {
		if value, found := strings.CutPrefix(entry, prefix); found {
			return CodexIdentityOverride(key, value), nil
		}
	}
	return "", fmt.Errorf("this Codex seat has no %s, so its amq commands would have no AMQ identity", key)
}

// fillCodexMailboxesSlot returns the writable_roots array whose one basic
// string is <session root>/agents. Nothing else may fill the slot: the
// ticket's session root is the root the launch was planned and validated
// for; an AM_ROOT from the process environment is never trusted to point
// somewhere else. A missing session root refuses; there is no fallback.
func fillCodexMailboxesSlot(argv []string, index int, sessionRoot string) (string, error) {
	if index < 1 || index >= len(argv) || argv[index-1] != "-c" {
		return "", fmt.Errorf("codex mailbox slot %d is invalid", index)
	}
	if argv[index] != codexMailboxesArgPrefix+codexMailboxesToken {
		return "", fmt.Errorf("codex mailbox slot %d does not carry the %s token", index, codexMailboxesToken)
	}
	if sessionRoot == "" {
		return "", fmt.Errorf("this Codex seat's execution ticket names no session root, so the mailboxes path cannot be filled")
	}
	resolved, err := resolvedPath(sessionRoot)
	if err != nil {
		return "", fmt.Errorf("resolve the queue root for codex mailboxes slot %d: %v", index, err)
	}
	return codexMailboxesArgPrefix + "[" + tomlBasicString(filepath.Join(resolved, "agents")) + "]", nil
}
