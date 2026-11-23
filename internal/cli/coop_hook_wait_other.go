//go:build !(darwin || linux)

package cli

import (
	"context"
	"io"
)

// runCoopWait does nothing where amq wake does not run: the waiter reads
// the wake's settings and lock.
func runCoopWait(context.Context) error { return nil }

// runCoopWaitCodex does nothing where amq wake does not run.
func runCoopWaitCodex(context.Context, io.Reader) error { return nil }

// runCoopPromptCodex does nothing where amq wake does not run.
func runCoopPromptCodex(io.Reader, io.Writer) error { return nil }

// coopWaitRuns reports no waiter where the waiter does not run.
func coopWaitRuns(root, me string) bool { return false }

// coopWaitRunsInBackground reports no session that keeps the waiter in the
// background where the waiter does not run.
func coopWaitRunsInBackground() bool { return false }

// coopWaitSettings is the settings read of a seat where amq wake does not
// run. No settings file is written on these platforms, so the defaults are
// the only layer: an attended claim there is never true, which is exactly
// what the hooks that read it must assume.
func coopWaitSettings(root, me string, last wakeSettingsLayers) (wakeSettings, wakeSettingsLayers) {
	return mergeWakeSettingsLayers(wakeSettingsLayerRead{}, wakeSettingsLayerRead{}, last)
}
