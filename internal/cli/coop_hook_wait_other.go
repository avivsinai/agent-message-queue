//go:build !(darwin || linux)

package cli

import "context"

// runCoopWait does nothing where amq wake does not run: the waiter reads
// the wake's settings and lock.
func runCoopWait(context.Context) error { return nil }

// coopWaitRunsInBackground reports no session that keeps the waiter in the
// background where the waiter does not run.
func coopWaitRunsInBackground() bool { return false }
