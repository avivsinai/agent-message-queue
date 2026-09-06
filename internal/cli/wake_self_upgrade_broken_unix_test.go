//go:build darwin || linux

package cli

import (
	"errors"
	"testing"
)

func readWakeSelfUpgradeRestartRecord(t *testing.T, fixture wakeRestartFixture) wakeRestartRecord {
	t.Helper()
	var record wakeRestartRecord
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		var exists bool
		var err error
		record, exists, err = readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("wake restart record is missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return record
}
