//go:build darwin

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const darwinWakeRestartControlTimeout = 2 * time.Second

func configureWakeRestartAdvertisementPlatform(lock *wakeLock, root, me string) {
	if lock == nil {
		return
	}
	lock.ResumeSignal = ""
	lock.ControlSocket = wakeControlSocketPath(root, me, lock.Generation)
}

func validateWakeRestartTransportPlatform(lock wakeLock, root, me string) error {
	if lock.ResumeSignal != "" {
		return fmt.Errorf("darwin wake restart refuses direct signal delivery")
	}
	if !validWakeReloadTransportGeneration(lock.Generation) {
		return fmt.Errorf("darwin wake restart generation is malformed")
	}
	expected := wakeControlSocketPath(root, me, lock.Generation)
	if lock.ControlSocket == "" || lock.ControlSocket != expected {
		return fmt.Errorf("darwin wake restart requires the exact generation control socket")
	}
	return nil
}

func notifyWakeRestartPlatform(
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	record wakeRestartRecord,
) error {
	if agentDir == nil {
		return fmt.Errorf("wake restart agent directory capability is missing")
	}
	if record.Status != wakeRestartPending ||
		record.Root != expected.Root || record.Agent != expected.Agent ||
		record.Generation != expected.Lock.Generation {
		return fmt.Errorf("wake restart request does not match the expected generation")
	}
	if expected.Lock.ResumeOwner == nil ||
		!sameWakeOwner(expected.Lock.ResumeOwner, &record.Owner) {
		return fmt.Errorf("wake restart owner does not match the advertised owner")
	}
	if err := validateWakeRestartTransportPlatform(
		expected.Lock,
		expected.Root,
		expected.Agent,
	); err != nil {
		return err
	}

	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(
			dirfd,
			agentDir,
			expected.Root,
			expected.Agent,
		)
		if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
			return fmt.Errorf("wake changed before restart control request")
		}
		if err := validateWakeRestartTransportPlatform(
			current.Lock,
			expected.Root,
			expected.Agent,
		); err != nil {
			return err
		}
		observed, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists || record.Status != wakeRestartPending ||
			observed.Status != wakeRestartPending ||
			observed.RequestID != record.RequestID ||
			observed.Generation != record.Generation ||
			!sameWakeRestartRecord(observed, record) {
			return fmt.Errorf("wake restart request changed before control notification")
		}
		return nil
	})
	if err != nil {
		return err
	}

	name, err := darwinControlSocketName(agentDir, expected.Lock.ControlSocket)
	if err != nil {
		return fmt.Errorf("wake restart control endpoint unavailable: %w", err)
	}
	conn, err := dialDarwinUnixAt(agentDir, name, darwinWakeRestartControlTimeout)
	if err != nil {
		return fmt.Errorf("wake restart control endpoint unavailable: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(darwinWakeRestartControlTimeout))

	request := wakeControlOwnerRequest{
		Generation: record.Generation,
		RequestID:  record.RequestID,
		Owner:      &record.Owner,
		Operation:  wakeControlRestartOperation,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode wake restart control request: %w", err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("send wake restart control request: %w", err)
	}
	line, err := bufio.NewReader(
		io.LimitReader(conn, wakeControlInjectViaACKMaxBytes+1),
	).ReadString('\n')
	if err != nil || len(line) > wakeControlInjectViaACKMaxBytes || strings.TrimSpace(line) != "ACK" {
		return fmt.Errorf("wake restart control request refused")
	}
	return nil
}
