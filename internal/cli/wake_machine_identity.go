package cli

import (
	"os"
	"strings"
	"sync"
)

// currentWakeMachineID returns a stable identity for this machine, cached for
// the life of the process. It is best-effort evidence: an empty string means
// no stable identity is available and callers must fall back to weaker
// same-machine signals. Overridable for tests.
var currentWakeMachineID = sync.OnceValue(func() string {
	return readWakeMachineIDPlatform()
})

// currentWakeBootID reports the boot identity of the running system, empty
// when unavailable. Overridable for tests.
var currentWakeBootID = func() string {
	return inspectWakeProcess(os.Getpid()).BootID
}

// currentWakeHostname reports the kernel hostname. Overridable for tests.
var currentWakeHostname = os.Hostname

type wakeMachineComparison int

const (
	// wakeMachineSame: the lock was proven to be written by this machine, so
	// local process inspection is meaningful.
	wakeMachineSame wakeMachineComparison = iota
	// wakeMachineDifferent: the lock records a stable machine identity that
	// differs from this machine's; local PID state says nothing about the
	// writer's process.
	wakeMachineDifferent
	// wakeMachineUnknown: no decisive same-machine or different-machine
	// evidence exists.
	wakeMachineUnknown
)

// classifyWakeLockMachine decides whether the lock was written by this
// machine. Evidence order, strongest first:
//
//  1. Stable machine identity recorded in the lock versus the current
//     machine's. Decisive in both directions.
//  2. Same-boot bridge: a lock whose boot session UUID equals the current
//     boot session UUID was written during this boot on this machine, because
//     boot session UUIDs are globally unique. Locks predating the machine_id
//     field get same-machine proof this way even after the hostname drifts
//     mid-boot. Only proves sameness; a differing boot UUID may be an older
//     boot of this same machine.
//  3. Hostname equality, the legacy signal. It is network-derived on macOS
//     and drifts, so it can only ever be the last resort.
func classifyWakeLockMachine(lock wakeLock) (wakeMachineComparison, string) {
	if recorded := strings.TrimSpace(lock.MachineID); recorded != "" {
		if current := currentWakeMachineID(); current != "" {
			if strings.EqualFold(recorded, current) {
				return wakeMachineSame, ""
			}
			return wakeMachineDifferent, "machine id mismatch"
		}
	}
	if isDarwinBootUUID(lock.BootID) {
		if boot := currentWakeBootID(); isDarwinBootUUID(boot) && strings.EqualFold(lock.BootID, boot) {
			return wakeMachineSame, ""
		}
	}
	if lock.Hostname == "" {
		return wakeMachineSame, ""
	}
	current, err := currentWakeHostname()
	if err != nil || current == "" {
		return wakeMachineUnknown, inspectionReason("hostname unavailable", err)
	}
	if lock.Hostname != current {
		return wakeMachineUnknown, "hostname mismatch"
	}
	return wakeMachineSame, ""
}
