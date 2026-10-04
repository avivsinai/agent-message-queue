//go:build !unix

package main

import (
	"context"
	"errors"
)

// listACPProcessesOS is unix-only; no process listing on Windows.
func listACPProcessesOS(ctx context.Context) ([]acpProcess, error) {
	return nil, nil
}

// installedIdentityOS is unix-only; no stat identity on Windows.
func installedIdentityOS() (execIdentity, error) {
	return execIdentity{}, errors.New("unsupported on this platform")
}

// processIdentityOS is unix-only; no identity on Windows.
func processIdentityOS(ctx context.Context, pid int) (execIdentity, error) {
	return execIdentity{}, errors.New("unsupported on this platform")
}
