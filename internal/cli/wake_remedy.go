package cli

import (
	"fmt"
	"strings"
)

// wakeRemedyArgv is the single representation for a runnable wake remedy.
// Keep raw arguments here; shell quoting belongs only to String.
type wakeRemedyArgv []string

func newWakeRemedy(args ...string) wakeRemedyArgv {
	return append(wakeRemedyArgv(nil), args...)
}

func (argv wakeRemedyArgv) String() string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, shellQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

func (argv wakeRemedyArgv) actionCommand() *wakeCheckCommand {
	if len(argv) == 0 || argv[0] != "amq" {
		return nil
	}
	return wakeCheckActionCommand(argv[1:]...)
}

func withWakeDiagnostic(err error, root, agent string) error {
	if err == nil {
		return nil
	}
	remedy := wakeCheckRemedy(root, agent).String()
	if strings.Contains(err.Error(), remedy) {
		return err
	}
	return fmt.Errorf("%w; inspect with %s", err, remedy)
}

func wakeStartRemedy(root, agent string) wakeRemedyArgv {
	return newWakeRemedy("amq", "wake", "--root", root, "--me", agent)
}

func wakeRepairRemedy(root, agent string) wakeRemedyArgv {
	return newWakeRemedy("amq", "wake", "repair", "--root", root, "--me", agent)
}

func wakeRecoverOwnerRemedy(root, agent string) wakeRemedyArgv {
	return newWakeRemedy("amq", "wake", "recover-owner", "--root", root, "--me", agent)
}

func wakeRestartRemedy(root, agent string) wakeRemedyArgv {
	return newWakeRemedy("amq", "wake", "restart", "--root", root, "--me", agent)
}

func wakeCheckRemedy(root, agent string) wakeRemedyArgv {
	return newWakeRemedy(
		"amq", "wake", "check", "--root", root, "--me", agent,
		"--json", "--json-schema=2",
	)
}

func wakeDoctorStaleWakeRemedy(root string) wakeRemedyArgv {
	return newWakeRemedy("amq", "doctor", "--root", root, "--ops", "--fix-wake-locks")
}
