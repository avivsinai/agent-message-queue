package claude

import (
	"os/exec"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/loginservice"
	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

// HookBinary is the path a hook should run for the program name installed
// beside the running program: StableBinary of SiblingBinary(name).
func HookBinary(name string) (string, error) {
	path, err := SiblingBinary(name)
	if err != nil {
		return "", err
	}
	path, _, err = loginservice.StableBinary(path, name)
	return path, err
}

// executable is the program's installed path, never its version-store copy,
// which has no program beside it; tests replace it.
var executable = selfupgrade.InstalledImage

// SiblingBinary is the program name beside the running program's resolved
// path, else on PATH. The one beside the program as it was started (a
// Homebrew bin symlink, which keeps its path across upgrades) stands in for
// it when that one resolves into the same directory.
func SiblingBinary(name string) (string, error) {
	if self, err := executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			resolved := filepath.Join(filepath.Dir(real), name)
			if installedFile(resolved) {
				started := filepath.Join(filepath.Dir(self), name)
				if target, err := filepath.EvalSymlinks(started); err == nil && filepath.Dir(target) == filepath.Dir(real) {
					return started, nil
				}
				return resolved, nil
			}
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}
