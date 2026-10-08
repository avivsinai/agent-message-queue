package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// Review of #993 r3 (Pro, P1): a resume that names no thread id let Codex
// pick a thread, possibly one recorded for another session or handle, and
// amq there took that other identity. AMQ refuses it before exec; --last and
// a name are resolved to an id first (resolveCodexResumeLaunch).
func TestUnresolvedCodexResumeIsRefused(t *testing.T) {
	const id = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91ac"
	recorded := 0
	record := func(string) error { recorded++; return nil }
	for _, args := range [][]string{
		{"resume", "--all"},
		{"resume", "-i", "shot.png", id},
		// Review of #993 r4 (Pro, P1): options before the subcommand.
		{"-c", "model=x", "resume"},
	} {
		err := recordResumedCodexThread("codex", args, nil)
		var mismatch *ExitCodeError
		if !errors.As(err, &mismatch) || mismatch.Code != ExitContextMismatch {
			t.Errorf("%q: err=%v, want the launch refused", args, err)
		}
		if err := recordResumedCodexThread("codex", args, record); err == nil {
			t.Errorf("%q with a recorder: launch allowed", args)
		}
	}
	if recorded != 0 {
		t.Fatalf("recorded %d unresolved selections", recorded)
	}
}

// Bead agent-message-queue-611.64 acceptance rows 1 and 2: a resume --last
// carries the thread AMQ selected (T1) in argv, so a thread that becomes
// latest later is not the one Codex opens, and a leading id U is the prompt,
// never the thread. AMQ binds T1 and Codex gets no --last.
func TestCodexResumeLastCarriesTheSelectedThread(t *testing.T) {
	const t1, u = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91b1", "01a1166e-dd8e-77c0-bc3b-a4b7e24e91b9"
	for _, tc := range []struct{ args, want []string }{
		{[]string{"resume", "--last"}, []string{"resume", t1}},
		{[]string{"resume", u, "--last"}, []string{"resume", t1, u}},
		{[]string{"--no-alt-screen", "resume", "--last", "-m", "x", "--", "-fix it"}, []string{"--no-alt-screen", "resume", "-m", "x", t1, "--", "-fix it"}},
	} {
		r, ok := parseCodexResume(tc.args)
		selector, prompt, last := r.selection()
		if !ok || r.unparsed || selector != nil || !last {
			t.Fatalf("%q: parsed %+v, selector %v, last %v; want --last with no selector", tc.args, r, selector, last)
		}
		args := rewriteCodexResume(tc.args, r, t1, prompt)
		var recorded []string
		err := recordResumedCodexThread("codex", args, func(thread string) error { recorded = append(recorded, thread); return nil })
		if err != nil || !slices.Equal(args, tc.want) || !slices.Equal(recorded, []string{t1}) {
			t.Fatalf("%q: argv %q, recorded %q, err %v; want %q bound to %s", tc.args, args, recorded, err, tc.want, t1)
		}
	}
}

// Review of #993 r3 (Pro): session s2 resuming by id a thread recorded for
// s1 must act as s2, not take s1's identity from the old record.
func TestResumeByIDTakesTheLaunchingSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	base := secureTempDirForTest(t)
	for _, s := range []string{"s1", "s2"} {
		if err := os.Mkdir(filepath.Join(base, s), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const thread = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91ad"
	for _, s := range []string{"s1", "s2"} {
		env := buildCoopExecEnvironment(nil, filepath.Join(base, s), "codex", s)
		if err := recordResumedCodexThread("codex", []string{"resume", thread}, codexThreadIdentityRecorder(env)); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range identityEnvKeys {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv("CODEX_THREAD_ID", thread)
	if err := adoptCodexThreadIdentity(); err != nil || os.Getenv(envSession) != "s2" {
		t.Fatalf("err=%v session=%q, want s2", err, os.Getenv(envSession))
	}
}
