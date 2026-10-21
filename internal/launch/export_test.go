package launch

import (
	"fmt"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// decodePlan strictly decodes and validates an emitted launch plan.
func decodePlan(data []byte) (Plan, error) {
	var plan Plan
	if err := decodeStrict(data, &plan); err != nil {
		return Plan{}, fmt.Errorf("decode launch plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// inspectLease reads the lease state under the lease interlock.
func inspectLease(root *fsq.DeliveryRoot) (LeaseInspection, error) {
	var inspection LeaseInspection
	err := withLeaseInterlock(root, func() error {
		var inspectErr error
		inspection, inspectErr = inspectLeaseLocked(root)
		return inspectErr
	})
	return inspection, err
}

func leasePath(sessionRoot string) string {
	return filepath.Join(sessionRoot, bindingDirectory, leaseFilename)
}
