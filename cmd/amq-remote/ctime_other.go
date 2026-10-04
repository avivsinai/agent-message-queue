//go:build !darwin && !linux

package main

import (
	"os"
	"time"
)

// ctimeOf has no portable unix-independent form here; other unix builds
// report no install time, so the stale check stays silent rather than
// guessing.
func ctimeOf(info os.FileInfo) time.Time {
	return time.Time{}
}
