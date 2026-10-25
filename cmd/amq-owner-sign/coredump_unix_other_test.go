//go:build unix && !linux

package main

import "testing"

// checkNoPipedDump has nothing to check outside Linux: the zero core limit
// already stops every dump there.
func checkNoPipedDump(*testing.T) {}
