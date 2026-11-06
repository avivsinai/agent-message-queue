//go:build !unix

package main

import "io"

// logSink leaves output unchanged where no endpoint writes up.log.
func logSink(_ string, w io.Writer) io.Writer { return w }
