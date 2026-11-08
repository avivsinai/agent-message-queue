//go:build darwin

package selfupgrade

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const storeHelperEnv = "AMQ_SELFUPGRADE_STORE_HELPER"

// A process that ExecImage started keeps a named image after its install
// directory is deleted. On 2026-11-07 brew cleanup deleted the Cellar folder
// that live amq processes ran from (the old stage was a hardlink beside the
// Cellar binary); tccd could not resolve them and every process under the
// same terminal got EPERM on an external volume for about 11 minutes. This
// lives at the exec boundary, so it needs a child process.
func TestExecImageKeepsANamedImageAfterTheInstallDirIsDeleted(t *testing.T) {
	home := realTempDir(t)
	install := filepath.Join(realTempDir(t), "Cellar", "amq", "1.0.0")
	binDir := filepath.Join(install, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidatePath := filepath.Join(binDir, "amq")
	signedDarwinCopy(t, candidatePath)

	child := exec.Command(candidatePath, "-test.run=^TestExecImageStoreHelperProcess$")
	child.Env = append(os.Environ(), "HOME="+home, storeHelperEnv+"=exec")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if line, ok := strings.CutPrefix(scanner.Text(), "running "); ok {
				ready <- line
				return
			}
		}
		close(ready)
	}()
	select {
	case _, ok := <-ready:
		if !ok {
			t.Fatal("helper exited before it reported its image")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not report its image")
	}

	if err := os.RemoveAll(install); err != nil {
		t.Fatal(err)
	}
	path, err := ProcessExecutablePath(child.Process.Pid)
	if err != nil {
		t.Fatalf("running image has no name after the install dir was deleted: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("running image path %s does not exist: %v", path, err)
	}
	store := filepath.Join(home, ".amq", "state", "versions") + string(filepath.Separator)
	if !strings.HasPrefix(path, store) {
		t.Fatalf("running image path = %s, want under %s", path, store)
	}
}

// Review of the version store: a prune ran after the entry directory was made,
// removed an abandoned temporary file there and then the emptied directory, so
// the publish into it failed with ENOENT and startup ran from the installed
// image.
func TestStoreImagePublishesIntoAnEntryWithAnAbandonedTempFile(t *testing.T) {
	root := realTempDir(t)
	candidate, err := CaptureImageEvidence(signedDarwinCandidate(t), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, strings.TrimPrefix(candidate.SHA256, "sha256:"))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(dir, "."+filepath.Base(candidate.ExecutionPath)+".tmp-1")
	if err := os.WriteFile(abandoned, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	saved := storeProcessPaths
	t.Cleanup(func() { storeProcessPaths = saved })
	storeProcessPaths = func(map[string]bool) (map[string]bool, error) { return map[string]bool{}, nil }

	stored, err := storeImageLocked(root, true, candidate)
	if err != nil {
		t.Fatalf("store image into an entry with an abandoned temp file: %v", err)
	}
	if _, err := os.Stat(stored.ExecutionPath); err != nil {
		t.Fatalf("stored image: %v", err)
	}
}

// TestExecImageStoreHelperProcess is the child of the test above. It execs
// its own image through ExecImage, then reports and waits for stdin to close.
func TestExecImageStoreHelperProcess(t *testing.T) {
	switch os.Getenv(storeHelperEnv) {
	case "exec":
		self, err := os.Executable()
		if err == nil {
			self, err = filepath.EvalSymlinks(self)
		}
		var candidate ImageEvidence
		if err == nil {
			candidate, err = CaptureImageEvidence(self, "1.0.0")
		}
		if err == nil {
			env := []string{storeHelperEnv + "=report"}
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, storeHelperEnv+"=") {
					env = append(env, entry)
				}
			}
			err = ExecImage(candidate, os.Args, env)
		}
		_, _ = fmt.Fprintln(os.Stderr, "helper exec failed:", err)
		os.Exit(1)
	case "report":
		self, _ := os.Executable()
		_, _ = fmt.Println("running " + self)
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	default:
		t.Skip("helper process for TestExecImageKeepsANamedImageAfterTheInstallDirIsDeleted")
	}
}

// realTempDir resolves /var to /private/var so path prefixes compare with
// what the kernel reports.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func signedDarwinCopy(t *testing.T, destination string) {
	t.Helper()
	source := signedDarwinCandidate(t)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0o755); err != nil {
		t.Fatal(err)
	}
}
