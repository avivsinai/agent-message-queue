package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
)

// Bead agent-message-queue-611.61 (live, codex-cli 0.160.1): tool commands of
// a Codex thread on the shared daemon see no AM_* variables, only
// CODEX_THREAD_ID. An amq command there takes the identity coop exec
// recorded for the thread. Design review of 611.61 (Pro): a later coop exec
// for the same handle revokes the old thread's identity, and any explicit
// identity variable, even empty, wins.
func TestCodexThreadTakesItsRecordedIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	base := secureTempDirForTest(t)
	root := filepath.Join(base, "s1")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	env := buildCoopExecEnvironment(nil, root, "codex", "s1")
	record := codexThreadIdentityRecorder(env)
	if record == nil {
		t.Fatal("no recorder for a complete coop exec identity")
	}
	const thread, later = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a6", "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a7"
	if err := record(thread); err != nil {
		t.Fatal(err)
	}
	clear := func() {
		for _, key := range identityEnvKeys {
			t.Setenv(key, "")
			_ = os.Unsetenv(key)
		}
	}

	clear()
	t.Setenv("CODEX_THREAD_ID", thread)
	if err := adoptCodexThreadIdentity(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(envRoot) != root || os.Getenv(envMe) != "codex" || os.Getenv(envSession) != "s1" {
		t.Fatalf("adopted root=%q me=%q session=%q", os.Getenv(envRoot), os.Getenv(envMe), os.Getenv(envSession))
	}

	clear()
	t.Setenv(envMe, "")
	if err := adoptCodexThreadIdentity(); err != nil || os.Getenv(envRoot) != "" {
		t.Fatalf("explicit empty AM_ME: err=%v root=%q, want no adoption", err, os.Getenv(envRoot))
	}

	if err := record(later); err != nil {
		t.Fatal(err)
	}
	clear()
	err := adoptCodexThreadIdentity()
	var mismatch *ExitCodeError
	if !errors.As(err, &mismatch) || mismatch.Code != ExitContextMismatch || os.Getenv(envRoot) != "" {
		t.Fatalf("revoked thread: err=%v root=%q, want a context mismatch and nothing adopted", err, os.Getenv(envRoot))
	}

	clear()
	t.Setenv("CODEX_THREAD_ID", "01a1166e-0000-7000-8000-000000000000")
	if err := adoptCodexThreadIdentity(); err != nil || os.Getenv(envRoot) != "" {
		t.Fatalf("unrecorded thread: err=%v, want today's behavior", err)
	}
	if _, err := codexidentity.Lookup(later, absPath(os.Getenv("CODEX_HOME"))); err != nil {
		t.Fatalf("current thread lookup: %v", err)
	}
}

// Bead agent-message-queue-611.61: a thread the user resumes by id through
// coop exec (codex resume <id>) carries the session's identity too.
func TestResumedCodexThreadTakesTheSessionIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	base := secureTempDirForTest(t)
	const thread = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a9"
	// AMQ's own session resume shape: options first, the id last.
	if err := recordResumedCodexThread("codex", []string{"resume", "--yolo", "-c", "notify=[\"x\"]", thread}, codexThreadIdentityRecorder(buildCoopExecEnvironment(nil, base, "codex", ""))); err != nil {
		t.Fatal(err)
	}
	for _, key := range identityEnvKeys {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv("CODEX_THREAD_ID", thread)
	if err := adoptCodexThreadIdentity(); err != nil || os.Getenv(envRoot) != base || os.Getenv(envMe) != "codex" {
		t.Fatalf("resumed thread: err=%v root=%q me=%q", err, os.Getenv(envRoot), os.Getenv(envMe))
	}
}

// Review of #993 (Pro, P1): a record that cannot be written stopped nothing;
// the launch resumed the thread without an identity.
func TestUnrecordableThreadStopsTheLaunch(t *testing.T) {
	err := recordResumedCodexThread("codex", []string{"resume", "01a1166e-dd8e-77c0-bc3b-a4b7e24e91aa"}, func(string) error { return errors.New("disk full") })
	var mismatch *ExitCodeError
	if !errors.As(err, &mismatch) || mismatch.Code != ExitContextMismatch {
		t.Fatalf("record failure: err=%v, want the launch stopped with a context mismatch", err)
	}
}
