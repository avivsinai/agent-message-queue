//go:build darwin

package cli

import (
	"os"
	"testing"
	"time"
)

const (
	darwinWakeOwnerDescendantHelperEnv  = "AMQ_TEST_DARWIN_WAKE_OWNER_DESCENDANT"
	darwinWakeOwnerDescendantProbeFDEnv = "AMQ_TEST_DARWIN_WAKE_OWNER_PROBE_FD"
)

func TestDarwinWakeOwnerPrivateStopRequiresExplicitByte(t *testing.T) {
	t.Run("explicit rollback byte stops", func(t *testing.T) {
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stop, cleanup := watchAuthoritativeWakePrivateStop(readEnd)
		defer cleanup()
		if _, err := writeEnd.Write([]byte{1}); err != nil {
			_ = writeEnd.Close()
			t.Fatal(err)
		}
		_ = writeEnd.Close()
		select {
		case <-stop:
		case <-time.After(2 * time.Second):
			t.Fatal("explicit startup rollback byte did not stop the wake child")
		}
	})

	t.Run("successful exec EOF does not stop", func(t *testing.T) {
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stop, cleanup := watchAuthoritativeWakePrivateStop(readEnd)
		if err := writeEnd.Close(); err != nil {
			cleanup()
			t.Fatal(err)
		}
		cleanup()
		select {
		case <-stop:
			t.Fatal("writer EOF was misclassified as exact owner death")
		default:
		}
	})
}
