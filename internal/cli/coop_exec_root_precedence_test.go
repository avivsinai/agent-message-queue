//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCoopExecSessionsHonorUnpinnedAMRoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		session string
	}{
		{
			name:    "explicit session",
			args:    []string{"--session", "feature", "--no-wake", "--me", "alice", "sh"},
			session: "feature",
		},
		{
			name:    "default collab",
			args:    []string{"--no-wake", "--me", "alice", "sh"},
			session: defaultSessionName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCoopSessionPinForTest(t)
			projectBase := initCoopProjectForTest(t, "alice")
			ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
			t.Setenv(envRoot, ambientRoot)

			execEnv := captureCoopExecEnvironment(t, tc.args)
			wantRoot := filepath.Join(ambientBase, tc.session)
			if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
				t.Fatalf("coop exec AM_ROOT = %q, want ambient route %q", got, wantRoot)
			}
			if got := envValue(execEnv, envBaseRoot); !sameTreeIdentity(got, ambientBase) {
				t.Fatalf("coop exec AM_BASE_ROOT = %q, want %q", got, ambientBase)
			}
			if got := envValue(execEnv, envSession); got != tc.session {
				t.Fatalf("coop exec AM_SESSION = %q, want %q", got, tc.session)
			}
			if tc.session != defaultSessionName {
				if _, err := os.Lstat(filepath.Join(projectBase, tc.session)); !os.IsNotExist(err) {
					t.Fatalf("coop exec provisioned project fallback despite AM_ROOT override: %v", err)
				}
			}
			requireEnvSessionRoute(t, tc.session, wantRoot, ambientBase)
		})
	}
}

func TestCoopExecSessionsHonorCompleteLegacyPin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		session string
	}{
		{
			name:    "explicit session",
			args:    []string{"--session", "feature", "--no-wake", "--me", "alice", "sh"},
			session: "feature",
		},
		{
			name:    "default collab",
			args:    []string{"--no-wake", "--me", "alice", "sh"},
			session: defaultSessionName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCoopSessionPinForTest(t)
			initCoopProjectForTest(t, "alice")
			ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
			t.Setenv(envRoot, ambientRoot)
			t.Setenv(envBaseRoot, ambientBase)
			t.Setenv(envSession, "current")

			execEnv := captureCoopExecEnvironment(t, tc.args)
			wantRoot := filepath.Join(ambientBase, tc.session)
			if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
				t.Fatalf("coop exec AM_ROOT = %q, want legacy pinned route %q", got, wantRoot)
			}
			if got := envValue(execEnv, envBaseRoot); !sameTreeIdentity(got, ambientBase) {
				t.Fatalf("coop exec AM_BASE_ROOT = %q, want %q", got, ambientBase)
			}
			if got := envValue(execEnv, envSession); got != tc.session {
				t.Fatalf("coop exec AM_SESSION = %q, want %q", got, tc.session)
			}
		})
	}
}

func TestCoopExecSessionsHonorCompleteIdentityPin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		session string
	}{
		{
			name:    "explicit session",
			args:    []string{"--session", "feature", "--no-wake", "--me", "alice", "sh"},
			session: "feature",
		},
		{
			name:    "default collab",
			args:    []string{"--no-wake", "--me", "alice", "sh"},
			session: defaultSessionName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCoopSessionPinForTest(t)
			initCoopProjectForTest(t, "alice")
			ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
			rootID, baseRootID := treeIdentityTokens(ambientRoot, ambientBase)
			if rootID == "" || baseRootID == "" {
				t.Fatal("test fixture did not produce complete identity tokens")
			}
			t.Setenv(envRoot, ambientRoot)
			t.Setenv(envBaseRoot, ambientBase)
			t.Setenv(envSession, "current")
			t.Setenv(envRootID, rootID)
			t.Setenv(envBaseRootID, baseRootID)

			execEnv := captureCoopExecEnvironment(t, tc.args)
			wantRoot := filepath.Join(ambientBase, tc.session)
			if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
				t.Fatalf("coop exec AM_ROOT = %q, want pinned route %q", got, wantRoot)
			}
			if got := envValue(execEnv, envBaseRoot); !sameTreeIdentity(got, ambientBase) {
				t.Fatalf("coop exec AM_BASE_ROOT = %q, want %q", got, ambientBase)
			}
			if got := envValue(execEnv, envSession); got != tc.session {
				t.Fatalf("coop exec AM_SESSION = %q, want %q", got, tc.session)
			}
			requireEnvSessionRoute(t, tc.session, wantRoot, ambientBase)
		})
	}
}

func TestCoopExecSessionsRejectAMRootOutsideCompleteLegacyPinBeforeProvision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		session string
	}{
		{
			name:    "explicit session",
			args:    []string{"--session", "feature", "--no-wake", "--me", "alice", "sh"},
			session: "feature",
		},
		{
			name:    "default collab",
			args:    []string{"--no-wake", "--me", "alice", "sh"},
			session: defaultSessionName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCoopSessionPinForTest(t)
			initCoopProjectForTest(t, "alice")
			ambientBase, _ := makeCoopAmbientSessionForTest(t, "current")
			_, otherRoot := makeCoopAmbientSessionForTest(t, "other")
			t.Setenv(envRoot, otherRoot)
			t.Setenv(envBaseRoot, ambientBase)
			t.Setenv(envSession, "current")

			execCalled := false
			oldExec := coopExecProcess
			coopExecProcess = func(string, []string, []string) error {
				execCalled = true
				return errors.New("unexpected exec")
			}
			defer func() { coopExecProcess = oldExec }()

			err := runCoopExec(tc.args)
			if err == nil || !containsStr(err.Error(), "differs from pinned root") {
				t.Fatalf("coop exec error = %v, want legacy AM_ROOT/pin mismatch refusal", err)
			}
			if execCalled {
				t.Fatal("coop exec reached process replacement with a mismatched legacy pin")
			}
			if _, statErr := os.Lstat(filepath.Join(ambientBase, tc.session)); !os.IsNotExist(statErr) {
				t.Fatalf("coop exec provisioned a session before legacy pin refusal: %v", statErr)
			}
		})
	}
}

func TestCoopExecSessionRejectsStaleIdentityPinBeforeProvision(t *testing.T) {
	clearCoopSessionPinForTest(t)
	initCoopProjectForTest(t, "alice")
	ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
	otherBase, otherRoot := makeCoopAmbientSessionForTest(t, "other")
	otherRootID, _ := treeIdentityTokens(otherRoot, otherBase)
	_, baseRootID := treeIdentityTokens(ambientRoot, ambientBase)
	t.Setenv(envRoot, ambientRoot)
	t.Setenv(envBaseRoot, ambientBase)
	t.Setenv(envSession, "current")
	t.Setenv(envRootID, otherRootID)
	t.Setenv(envBaseRootID, baseRootID)

	execCalled := false
	oldExec := coopExecProcess
	coopExecProcess = func(string, []string, []string) error {
		execCalled = true
		return errors.New("unexpected exec")
	}
	defer func() { coopExecProcess = oldExec }()

	err := runCoopExec([]string{"--session", "feature", "--no-wake", "--me", "alice", "sh"})
	if err == nil || !containsStr(err.Error(), "target root identity is not current") {
		t.Fatalf("coop exec error = %v, want stale root identity refusal", err)
	}
	if execCalled {
		t.Fatal("coop exec reached process replacement with a stale identity pin")
	}
	if _, statErr := os.Lstat(filepath.Join(ambientBase, "feature")); !os.IsNotExist(statErr) {
		t.Fatalf("coop exec provisioned a session before identity refusal: %v", statErr)
	}
}

func TestCoopExecSessionRejectsAMRootOutsideCompleteIdentityPinBeforeProvision(t *testing.T) {
	clearCoopSessionPinForTest(t)
	initCoopProjectForTest(t, "alice")
	ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
	_, otherRoot := makeCoopAmbientSessionForTest(t, "other")
	rootID, baseRootID := treeIdentityTokens(ambientRoot, ambientBase)
	t.Setenv(envRoot, otherRoot)
	t.Setenv(envBaseRoot, ambientBase)
	t.Setenv(envSession, "current")
	t.Setenv(envRootID, rootID)
	t.Setenv(envBaseRootID, baseRootID)

	err := runCoopExec([]string{"--session", "feature", "--no-wake", "--me", "alice", "sh"})
	if err == nil || !containsStr(err.Error(), "root is not the pinned session directory") {
		t.Fatalf("coop exec error = %v, want AM_ROOT/pin mismatch refusal", err)
	}
	if _, statErr := os.Lstat(filepath.Join(ambientBase, "feature")); !os.IsNotExist(statErr) {
		t.Fatalf("coop exec provisioned a session before AM_ROOT/pin refusal: %v", statErr)
	}
}

func TestCoopExecSessionRejectsStaleBaseIdentityPinBeforeProvision(t *testing.T) {
	clearCoopSessionPinForTest(t)
	initCoopProjectForTest(t, "alice")
	ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
	otherBase, _ := makeCoopAmbientSessionForTest(t, "other")
	rootID, _ := treeIdentityTokens(ambientRoot, ambientBase)
	_, staleBaseRootID := treeIdentityTokens(filepath.Join(otherBase, "other"), otherBase)
	t.Setenv(envRoot, ambientRoot)
	t.Setenv(envBaseRoot, ambientBase)
	t.Setenv(envSession, "current")
	t.Setenv(envRootID, rootID)
	t.Setenv(envBaseRootID, staleBaseRootID)

	err := runCoopExec([]string{"--session", "feature", "--no-wake", "--me", "alice", "sh"})
	if err == nil || !containsStr(err.Error(), "pinned base root identity is not current") {
		t.Fatalf("coop exec error = %v, want stale base identity refusal", err)
	}
	if _, statErr := os.Lstat(filepath.Join(ambientBase, "feature")); !os.IsNotExist(statErr) {
		t.Fatalf("coop exec provisioned a session before stale base refusal: %v", statErr)
	}
}

func TestCoopExecAMRootOverridesMalformedProjectConfig(t *testing.T) {
	enterBrokenRootProject(t)
	clearCoopSessionPinForTest(t)
	ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
	t.Setenv(envRoot, ambientRoot)

	execEnv := captureCoopExecEnvironment(t, []string{
		"--session", "feature",
		"--no-wake",
		"--me", "alice",
		"sh",
	})
	wantRoot := filepath.Join(ambientBase, "feature")
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
		t.Fatalf("coop exec AM_ROOT = %q, want AM_ROOT override route %q", got, wantRoot)
	}
}

func TestCoopExecDefaultSessionAMRootOverridesMalformedProjectConfig(t *testing.T) {
	enterBrokenRootProject(t)
	clearCoopSessionPinForTest(t)
	ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
	t.Setenv(envRoot, ambientRoot)

	execEnv := captureCoopExecEnvironment(t, []string{
		"--no-wake",
		"--me", "alice",
		"sh",
	})
	wantRoot := filepath.Join(ambientBase, defaultSessionName)
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
		t.Fatalf("coop exec AM_ROOT = %q, want AM_ROOT override route %q", got, wantRoot)
	}
}

func TestCoopExecNamedSessionFallsBackToProjectConfig(t *testing.T) {
	clearCoopSessionPinForTest(t)
	setOptionalEnv(t, envRoot, "", false)
	projectBase := initCoopProjectForTest(t, "alice")

	execEnv := captureCoopExecEnvironment(t, []string{
		"--session", "feature",
		"--no-wake",
		"--me", "alice",
		"sh",
	})
	wantRoot := filepath.Join(projectBase, "feature")
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
		t.Fatalf("coop exec AM_ROOT = %q, want project fallback %q", got, wantRoot)
	}
}

func TestCoopExecDefaultSessionFallsBackToAMQGlobalRootWithoutCWDWrites(t *testing.T) {
	clearCoopSessionPinForTest(t)
	setOptionalEnv(t, envRoot, "", false)
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)

	globalBase := filepath.Join(t.TempDir(), "global-root")
	if err := fsq.EnsureRootDirs(globalBase); err != nil {
		t.Fatalf("initialize AMQ_GLOBAL_ROOT base: %v", err)
	}
	t.Setenv("AMQ_GLOBAL_ROOT", globalBase)

	execEnv := captureCoopExecEnvironment(t, []string{
		"--no-init",
		"--no-wake",
		"--me", "alice",
		"sh",
	})
	wantRoot := filepath.Join(globalBase, defaultSessionName)
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
		t.Fatalf("coop exec AM_ROOT = %q, want AMQ_GLOBAL_ROOT fallback %q", got, wantRoot)
	}
	if got := envValue(execEnv, envBaseRoot); !sameTreeIdentity(got, globalBase) {
		t.Fatalf("coop exec AM_BASE_ROOT = %q, want %q", got, globalBase)
	}
	if got := envValue(execEnv, envSession); got != defaultSessionName {
		t.Fatalf("coop exec AM_SESSION = %q, want %q", got, defaultSessionName)
	}
	if entries, err := os.ReadDir(cwd); err != nil {
		t.Fatalf("read cwd after coop exec: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("coop exec created cwd entries despite global fallback: %v", entries)
	}
}

func TestCoopExecDefaultSessionFallsBackToHomeAmqrcWithoutCWDWrites(t *testing.T) {
	clearCoopSessionPinForTest(t)
	setOptionalEnv(t, envRoot, "", false)
	setOptionalEnv(t, "AMQ_GLOBAL_ROOT", "", false)
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cwd := t.TempDir()
	t.Chdir(cwd)

	homeBase := filepath.Join(fakeHome, "configured-root")
	if err := fsq.EnsureRootDirs(homeBase); err != nil {
		t.Fatalf("initialize HOME .amqrc base: %v", err)
	}
	rcData, err := json.Marshal(map[string]string{"root": homeBase})
	if err != nil {
		t.Fatalf("marshal HOME .amqrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatalf("write HOME .amqrc: %v", err)
	}

	execEnv := captureCoopExecEnvironment(t, []string{
		"--no-init",
		"--no-wake",
		"--me", "alice",
		"sh",
	})
	wantRoot := filepath.Join(homeBase, defaultSessionName)
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, wantRoot) {
		t.Fatalf("coop exec AM_ROOT = %q, want HOME .amqrc fallback %q", got, wantRoot)
	}
	if got := envValue(execEnv, envBaseRoot); !sameTreeIdentity(got, homeBase) {
		t.Fatalf("coop exec AM_BASE_ROOT = %q, want %q", got, homeBase)
	}
	if got := envValue(execEnv, envSession); got != defaultSessionName {
		t.Fatalf("coop exec AM_SESSION = %q, want %q", got, defaultSessionName)
	}
	if entries, err := os.ReadDir(cwd); err != nil {
		t.Fatalf("read cwd after coop exec: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("coop exec created cwd entries despite HOME .amqrc fallback: %v", entries)
	}
}

func captureCoopExecEnvironment(t *testing.T, args []string) []string {
	t.Helper()
	sentinel := errors.New("exec sentinel")
	var execEnv []string
	oldExec := coopExecProcess
	coopExecProcess = func(_ string, _ []string, env []string) error {
		execEnv = append([]string(nil), env...)
		return sentinel
	}
	defer func() { coopExecProcess = oldExec }()

	if err := runCoopExec(args); !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want process sentinel", err)
	}
	return execEnv
}

func clearCoopSessionPinForTest(t *testing.T) {
	t.Helper()
	for _, key := range []string{envBaseRoot, envSession, envRootID, envBaseRootID} {
		setOptionalEnv(t, key, "", false)
	}
}

func makeCoopAmbientSessionForTest(t *testing.T, session string) (string, string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	if err := fsq.EnsureRootDirs(base); err != nil {
		t.Fatalf("create ambient base: %v", err)
	}
	root, err := provisionCoopSession(base, session, []string{"alice"}, "", "")
	if err != nil {
		t.Fatalf("create ambient session: %v", err)
	}
	return base, root
}

func requireEnvSessionRoute(t *testing.T, session, wantRoot, wantBase string) {
	t.Helper()
	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", session, "--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("env --session %s: %v", session, err)
	}
	var output envOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode env output: %v", err)
	}
	if !sameTreeIdentity(output.Root, wantRoot) || !sameTreeIdentity(output.BaseRoot, wantBase) {
		t.Fatalf(
			"env --session route = (%q, %q), want (%q, %q)",
			output.Root,
			output.BaseRoot,
			wantRoot,
			wantBase,
		)
	}
}
