//go:build darwin

package cli

import (
	"fmt"
	"os"
)

func observeAuthoritativeWakeOwnerPlatform(owner wakeOwner) (wakeOwnerObservation, error) {
	first := inspectWakeProcess(owner.PID)
	firstSessionID, firstSessionErr := getWakeProcessSID(owner.PID)
	second := inspectWakeProcess(owner.PID)
	secondSessionID, secondSessionErr := getWakeProcessSID(owner.PID)
	state, reason := classifyStableAuthoritativeWakeOwner(
		owner,
		first, firstSessionID, firstSessionErr,
		second, secondSessionID, secondSessionErr,
	)
	return wakeOwnerObservation{State: state, Reason: reason}, nil
}

func captureAuthoritativeCurrentWakeOwnerPlatform() (wakeOwner, error) {
	pid := os.Getpid()
	first := inspectWakeProcess(pid)
	firstSessionID, firstSessionErr := getWakeProcessSID(pid)
	if !first.Running || firstSessionErr != nil {
		return wakeOwner{}, fmt.Errorf("current process identity is unavailable")
	}
	owner := wakeOwner{
		PID:          pid,
		ProcessStart: first.StartToken,
		BootID:       first.BootID,
		SessionID:    firstSessionID,
	}
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		return wakeOwner{}, err
	}
	second := inspectWakeProcess(pid)
	secondSessionID, secondSessionErr := getWakeProcessSID(pid)
	state, reason := classifyStableAuthoritativeWakeOwner(
		owner,
		first, firstSessionID, firstSessionErr,
		second, secondSessionID, secondSessionErr,
	)
	if state != wakeOwnerSame {
		return wakeOwner{}, fmt.Errorf("current process identity is %s: %s", state, reason)
	}
	return owner, nil
}
