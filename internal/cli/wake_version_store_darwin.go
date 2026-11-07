//go:build darwin

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

var runFromVersionStore = selfupgrade.RunFromStore

// runWakeFromVersionStore moves a wake onto its copy in the version store
// before it reads any lock or state, so a package manager that deletes the
// installed image does not leave the wake without a named image. A wake that
// a restart resumed keeps its private stage copy: the resume binds that path.
// Any failure leaves the wake on its current image.
func runWakeFromVersionStore() {
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err == nil && darwinWakeRestartStagePath(filepath.Clean(executable)) {
		return
	}
	if err := runFromVersionStore(cliVersion); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "amq wake: keeps running from the installed image: %v\n", err)
	}
}
