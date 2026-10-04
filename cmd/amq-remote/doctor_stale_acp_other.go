//go:build !unix

package main

import "time"

// listACPProcessesOS is unix-only; no process listing on Windows.
func listACPProcessesOS() ([]acpProcess, error) {
	return nil, nil
}

// installedTimeOS is unix-only; no ctime on Windows.
func installedTimeOS() (time.Time, error) {
	return time.Time{}, nil
}
