//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

var errWakeImageChangedWhileHashing = selfupgrade.ErrImageChangedWhileHashing

var captureCurrentWakeImageEvidence = captureCurrentWakeImageEvidenceDefault

func captureCurrentWakeImageEvidenceDefault() (wakeImageEvidenceV1, error) {
	path, err := os.Executable()
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("resolve current wake executable: %w", err)
	}
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return wakeImageEvidenceV1{}, fmt.Errorf("current wake executable path is not absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("resolve current wake executable symlinks: %w", err)
	}
	// Resolving the process-reported pathname does not bind this open to the
	// image actually executing. Keep it diagnostic on every platform.
	return captureWakeImageEvidence(filepath.Clean(resolved), strings.TrimSpace(cliVersion))
}

func captureWakeImageEvidence(path, embeddedVersion string) (wakeImageEvidenceV1, error) {
	return selfupgrade.CaptureImageEvidenceWithMutator(path, embeddedVersion, nil)
}

func captureWakeImageEvidenceFromOpenFile(
	file *os.File,
	executionPath, embeddedVersion, method string,
) (wakeImageEvidenceV1, error) {
	return selfupgrade.CaptureImageEvidenceFromOpenFileWithMutator(
		file,
		executionPath,
		embeddedVersion,
		method,
		nil,
	)
}
