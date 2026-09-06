//go:build darwin || linux

package cli

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func newOwnerAcquisitionPublicationFixture(t *testing.T) (string, wakeTarget, wakeOwner) {
	t.Helper()
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-publication-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner

	originalObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if !sameWakeOwner(&got, &owner) {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		return liveWakeOwnerObservationForTest(), nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = originalObserve })
	return root, target, owner
}
