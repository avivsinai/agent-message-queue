//go:build windows

package main

// forbidCoreDumps has nothing to set on Windows: there is no core size limit
// and the Go runtime writes no core file. A crash dump there comes only from
// Windows Error Reporting, which the runtime enables only for GOTRACEBACK=wer.
func forbidCoreDumps() error { return nil }
