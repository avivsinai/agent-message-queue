//go:build unix

package main

import (
	"io"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/loginservice"
)

// logSink returns the writer up and serve log through. The bounded, stamped
// writer is the neutral package's LogSink for the state directory's up.log.
func logSink(stateDir string, w io.Writer) io.Writer {
	return loginservice.LogSink(filepath.Join(stateDir, "up.log"), w)
}
