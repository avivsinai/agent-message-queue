package selfupgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// EnvInstalledImage carries the path a process was started as when it moved
// onto its copy in the version store. Children inherit it, so a reader
// accepts it only for its own program name.
const EnvInstalledImage = "AMQ_INSTALLED_IMAGE"

// InstalledImage is the path this program was installed and started as. A
// process that runs from the version store has only its own program in that
// directory, so a caller that needs a program installed beside it, or that
// writes the program's path into durable configuration, uses this path.
func InstalledImage() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	return installedImage(exe, InStore(filepath.Clean(resolved)), os.Getenv(EnvInstalledImage))
}

func installedImage(exe string, inStore bool, recorded string) (string, error) {
	if !inStore {
		return exe, nil
	}
	if recorded != "" && filepath.IsAbs(recorded) && filepath.Base(recorded) == filepath.Base(exe) {
		return recorded, nil
	}
	return "", fmt.Errorf("%s runs from the version store and records no installed image", exe)
}

// withInstalledImage is env with EnvInstalledImage set to path.
func withInstalledImage(env []string, path string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, EnvInstalledImage+"=") {
			out = append(out, entry)
		}
	}
	return append(out, EnvInstalledImage+"="+path)
}

// StoredCopyOf reports whether running is a version-store entry that holds
// the same content as the program at installed. A store entry is a copy, so
// its file identity never matches the installed program's; its entry key,
// the content digest, does.
func StoredCopyOf(running, installed string) (bool, error) {
	if !InStore(filepath.Clean(running)) {
		return false, nil
	}
	file, err := os.Open(installed)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return false, err
	}
	key := filepath.Base(filepath.Dir(filepath.Clean(running)))
	return key == hex.EncodeToString(hasher.Sum(nil)), nil
}
