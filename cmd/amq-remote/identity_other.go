//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package main

import (
	"context"
	"errors"
)

// staleACPSupportedOS is false on unsupported platforms: the whole stale
// harness inspection is skipped, and doctor stays quiet about it on
// purpose.
const staleACPSupportedOS = false

// listACPProcessesOS lists nothing: process listing is Linux and BSD only.
func listACPProcessesOS(ctx context.Context) ([]acpProcess, error) {
	return nil, nil
}

// installedIdentityOS has no stat identity outside Linux and the BSDs.
func installedIdentityOS() (execIdentity, error) {
	return execIdentity{}, errors.New("unsupported on this platform")
}

// processIdentityOS has no process identity outside Linux and the BSDs.
func processIdentityOS(ctx context.Context, pid int) (execIdentity, error) {
	return execIdentity{}, errors.New("unsupported on this platform")
}
