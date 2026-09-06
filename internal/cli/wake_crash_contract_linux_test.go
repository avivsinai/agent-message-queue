//go:build linux

package cli

import (
	"syscall"
	"testing"
	"time"
)

func TestCrashContractReloadEndpointRejectsGenerationMismatch(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()

	request := fixture.request()
	request.Generation = "1123456789abcdef0123456789abcdef"
	response, err := sendLinuxWakeReloadTransportRequest(t, fixture, endpoint, request)
	requireLinuxWakeReloadSilentRefusal(t, response, err)
	current := inspectWakeLock(fixture.root, fixture.agent)
	if !sameWakeLockGeneration(fixture.expected, current) {
		t.Fatalf("generation-mismatch request changed lock: %#v", current)
	}
	if err := syscall.Kill(fixture.owner.PID, 0); err != nil {
		t.Fatalf("generation-mismatch request signaled owner: %v", err)
	}
}
