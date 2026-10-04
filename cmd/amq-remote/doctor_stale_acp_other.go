//go:build !unix

package main

// listACPProcessesOS is unix-only; no process listing on Windows.
func listACPProcessesOS() ([]acpProcess, error) {
	return nil, nil
}
