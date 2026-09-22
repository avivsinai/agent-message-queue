//go:build windows

package claude

import "testing"

// testRegistryFIFO: the FIFO shape is a unix-only probe; the directory
// shape in TestRegistryLeafMustBeRegularFile covers the non-regular-file
// rule on Windows.
func testRegistryFIFO(_ *testing.T, _, _ string) {}
