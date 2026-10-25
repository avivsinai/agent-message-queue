package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// checkCodexExecutablePaths uses the same executable lookup as coop exec.
// It compares files rather than versions and never executes a candidate.
func checkCodexExecutablePaths() (doctorCheck, bool) {
	selected, err := exec.LookPath("codex")
	if err != nil {
		if errors.Is(err, exec.ErrDot) {
			return doctorCheck{Name: "Codex executable", Status: "warn", Message: fmt.Sprintf("this PATH resolves codex to %s in the current directory, which executable lookup refuses; put the intended Codex installation on an absolute PATH entry", selected)}, true
		}
		return doctorCheck{}, false
	}
	selected, err = filepath.Abs(selected)
	if err != nil {
		return doctorCheck{Name: "Codex executable", Status: "warn", Message: fmt.Sprintf("cannot resolve the Codex executable selected by this PATH: %v", err)}, true
	}
	first, err := os.Stat(selected)
	if err != nil {
		return doctorCheck{Name: "Codex executable", Status: "warn", Message: fmt.Sprintf("cannot inspect the Codex executable selected by this PATH (%s): %v", selected, err)}, true
	}
	alternatives := laterExecutablePaths("codex", first)
	if len(alternatives) == 0 {
		return doctorCheck{}, false
	}
	return doctorCheck{
		Name: "Codex executable", Status: "warn",
		Message: fmt.Sprintf("multiple Codex executables: with this PATH, amx and amq coop exec codex select %s; alternatives: %s. Compare their --version outputs and put the intended installation first in PATH", selected, strings.Join(alternatives, ", ")),
	}, true
}

// laterExecutablePaths keeps PATH order and compares physical files so
// repeated entries, symlinks, and hard links do not duplicate an installation.
func laterExecutablePaths(name string, first os.FileInfo) []string {
	files := []os.FileInfo{first}
	var alternatives []string
	foundSelected := false
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		candidate, err := filepath.Abs(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		candidate, err = exec.LookPath(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if !foundSelected {
			foundSelected = os.SameFile(first, info)
			continue
		}
		duplicate := false
		for _, file := range files {
			if os.SameFile(file, info) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			files = append(files, info)
			alternatives = append(alternatives, candidate)
		}
	}
	return alternatives
}
