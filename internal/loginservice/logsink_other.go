//go:build !unix

package loginservice

import "io"

// LogSink leaves output unchanged where no service writes a watched log.
func LogSink(_ string, w io.Writer) io.Writer { return w }
