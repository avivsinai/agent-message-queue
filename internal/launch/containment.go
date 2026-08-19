package launch

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

const (
	providerProjectContainedCode = ProviderProjectContainedCode
	wrapperProjectContainedCode  = WrapperProjectContainedCode
	amqProjectContainedCode      = AMQProjectContainedCode
)

// rejectProjectContained refuses both the path presented by the caller and
// its physical target. Checking only the latter lets a project-tracked
// symlink point at an external executable while retaining a project-writable
// argv[0].
func rejectProjectContained(projectRoot, raw, resolved, code string) error {
	project, err := resolvedPath(projectRoot)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	rawPath, err := rawExecutablePath(raw)
	if err != nil {
		return err
	}
	if pathWithin(rawPath, project) || (resolved != "" && pathWithin(resolved, project)) {
		return &LaunchPathError{Code: code, Path: raw}
	}
	return nil
}

func absoluteExecutablePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		lookedUp, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		path = lookedUp
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// rawExecutablePath resolves symlinks in the parent directories but leaves
// the executable's final directory entry untouched.
func rawExecutablePath(path string) (string, error) {
	abs, err := absoluteExecutablePath(path)
	if err != nil {
		return "", err
	}
	parent, err := resolvedPath(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}
