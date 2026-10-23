package launch

import (
	"fmt"
	"slices"
	"strings"
)

// IdentityEnvKeys are the variables that make up an AMQ participant
// identity.
var IdentityEnvKeys = []string{"AM_ROOT", "AM_BASE_ROOT", "AM_SESSION", "AM_ME", "AM_ROOT_ID", "AM_BASE_ROOT_ID"}

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
