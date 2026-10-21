package cli

import (
	"os"
	"time"
)

// Test entry points that wrap production functions with the defaults and
// directory handling tests use. Production calls the wrapped functions
// directly.

// injectNotification delivers one system-fixed payload as both input and output.
func injectNotification(cfg *wakeConfig, text string, deferForInput bool) error {
	payload := wakePayload{
		text:       text,
		provenance: wakePayloadSystemFixed,
	}
	return deliverWakeNotification(
		cfg,
		wakeNotification{input: payload, output: payload},
		deferForInput,
	)
}

func runOpsChecks(root string, rootSource string, fixWakeLocks bool, explicitBaseRoot ...string) *doctorOpsResult {
	return runOpsChecksWithSchema(
		root,
		rootSource,
		fixWakeLocks,
		wakeCheckSchemaV1,
		explicitBaseRoot...,
	)
}

func (state *wakeDoorbellState) plan(
	now time.Time,
	current map[string]os.FileInfo,
) wakeDoorbellPlan {
	return state.planHeld(now, current, nil)
}
