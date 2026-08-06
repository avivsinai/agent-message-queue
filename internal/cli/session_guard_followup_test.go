package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSessionlessCoopPinRejectsRootSwitch(t *testing.T) {
	commands := []struct {
		name string
		run  func(root string) error
	}{
		{
			name: "drain",
			run: func(_ string) error {
				return runDrain([]string{"--me", "alice"})
			},
		},
		{
			name: "send",
			run: func(_ string) error {
				return runSend([]string{"--me", "alice", "--to", "bob", "--body", "wrong root"})
			},
		},
		{
			name: "env",
			run: func(_ string) error {
				_, _, err := captureEnvOutput(t, func() error { return runEnv(nil) })
				return err
			},
		},
	}

	for _, tt := range commands {
		t.Run(tt.name, func(t *testing.T) {
			rootA := filepath.Join(t.TempDir(), "queue-a")
			rootB := filepath.Join(t.TempDir(), "queue-b")
			for _, root := range []string{rootA, rootB} {
				for _, agent := range []string{"alice", "bob"} {
					if err := fsq.EnsureAgentDirs(root, agent); err != nil {
						t.Fatalf("EnsureAgentDirs(%s, %s): %v", root, agent, err)
					}
				}
			}
			if tt.name == "drain" {
				deliverGuardMessage(t, rootB, "alice", "sessionless-theft")
			}

			pinEnv := buildCoopExecEnvironment(nil, rootA, "alice", "")
			applyEnvSlice(t, pinEnv, envRoot, envBaseRoot, envSession, envMe)
			t.Setenv(envRoot, rootB)

			err := tt.run(rootB)
			assertConsumeRefused(t, err, tt.name)
		})
	}
}

func TestListOwnBaseDoesNotSuppressContradictoryLegacyPinWarning(t *testing.T) {
	baseRoot := initializedSendMailboxRoot(t, "alice")
	if err := fsq.EnsureAgentDirs(filepath.Join(baseRoot, "current"), "alice"); err != nil {
		t.Fatal(err)
	}
	deliverGuardMessage(t, baseRoot, "alice", "base-inspection")
	t.Setenv(envRoot, baseRoot)
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "current")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--root", baseRoot, "--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("contradictory legacy pin should retain read-only list inspection: %v", err)
	}
	if !strings.Contains(stdout, `"id": "base-inspection"`) {
		t.Fatalf("list did not inspect the requested base mailbox: %q", stdout)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "differs from pinned root") {
		t.Fatalf("list suppressed contradictory legacy pin warning: %q", stderr)
	}
	if got := inboxCount(t, baseRoot, "alice"); got != 1 {
		t.Fatalf("read-only list mutated base inbox: count = %d, want 1", got)
	}
}

func TestSessionlessEnvOutputPinsExactRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue-a")
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--root", root, "--me", "alice"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}
	if !strings.Contains(stdout, "export AM_BASE_ROOT="+shellQuotePosix(root)+"\n") {
		t.Fatalf("sessionless output must pin the exact root in AM_BASE_ROOT: %q", stdout)
	}
	if !strings.Contains(stdout, "export AM_SESSION=\n") {
		t.Fatalf("sessionless output must emit an empty AM_SESSION: %q", stdout)
	}
}

func TestLoadSessionPinNamesIncompletePinEvidence(t *testing.T) {
	tests := []struct {
		name        string
		session     *string
		rootID      *string
		baseRootID  *string
		wantNames   []string
		secretValue string
	}{
		{
			name:      "named session only",
			session:   pointerTo("collab"),
			wantNames: []string{envSession, envBaseRoot},
		},
		{
			name:      "empty session only",
			session:   pointerTo(""),
			wantNames: []string{envSession, envBaseRoot},
		},
		{
			name:        "root identity only",
			rootID:      pointerTo("v1:secret-root-token"),
			wantNames:   []string{envRootID, envBaseRoot},
			secretValue: "v1:secret-root-token",
		},
		{
			name:        "base identity only",
			baseRootID:  pointerTo("v1:secret-base-token"),
			wantNames:   []string{envBaseRootID, envBaseRoot},
			secretValue: "v1:secret-base-token",
		},
		{
			name:        "identity pair",
			rootID:      pointerTo("v1:secret-root-token"),
			baseRootID:  pointerTo("v1:secret-base-token"),
			wantNames:   []string{envRootID, envBaseRootID, envBaseRoot},
			secretValue: "secret-",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setOptionalEnv(t, envSession, optionalString(tc.session), tc.session != nil)
			setOptionalEnv(t, envRootID, optionalString(tc.rootID), tc.rootID != nil)
			setOptionalEnv(t, envBaseRootID, optionalString(tc.baseRootID), tc.baseRootID != nil)
			setOptionalEnv(t, envBaseRoot, "", false)

			_, err := loadSessionPin()
			if err == nil || GetExitCode(err) != ExitContextMismatch {
				t.Fatalf("incomplete pin should be context mismatch, got %v", err)
			}
			for _, name := range tc.wantNames {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("error must name evidence %s: %v", name, err)
				}
			}
			presentEvidence := map[string]bool{
				envSession:    tc.session != nil,
				envRootID:     tc.rootID != nil,
				envBaseRootID: tc.baseRootID != nil,
			}
			for name, shouldAppear := range presentEvidence {
				if strings.Contains(err.Error(), name) != shouldAppear {
					t.Fatalf("error evidence name %s presence = %t, want %t: %v", name, strings.Contains(err.Error(), name), shouldAppear, err)
				}
			}
			if !strings.Contains(err.Error(), "--root <path>") ||
				!strings.Contains(err.Error(), "clear stale pin variables before using --session <name>") {
				t.Fatalf("error must distinguish direct repin from clearing stale evidence first: %v", err)
			}
			if tc.secretValue != "" && strings.Contains(err.Error(), tc.secretValue) {
				t.Fatalf("error leaked opaque identity token: %v", err)
			}
		})
	}
}

func pointerTo(value string) *string {
	return &value
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func TestEnvSessionFlagUsesPinnedCustomBaseFromForeignCWD(t *testing.T) {
	customBase := filepath.Join(t.TempDir(), "custom-queue")
	sourceRoot := filepath.Join(customBase, "session1")
	targetRoot := filepath.Join(customBase, "session2")
	for _, root := range []string{sourceRoot, targetRoot} {
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", root, err)
		}
	}

	foreignProject := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreignProject, ".amqrc"), []byte(`{"root":"foreign-mail"}`), 0o600); err != nil {
		t.Fatalf("write foreign .amqrc: %v", err)
	}
	t.Chdir(foreignProject)

	t.Setenv(envRoot, sourceRoot)
	t.Setenv(envBaseRoot, customBase)
	t.Setenv(envSession, "session1")
	t.Setenv(envMe, "alice")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "session2", "--me", "alice"})
	})
	if err != nil {
		t.Fatalf("runEnv --session: %v", err)
	}
	for _, want := range []string{
		"export AM_ROOT=" + shellQuotePosix(targetRoot) + "\n",
		"export AM_BASE_ROOT=" + shellQuotePosix(customBase) + "\n",
		"export AM_SESSION=session2\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("pinned-base session route missing %q: %q", want, stdout)
		}
	}
}

func TestBlankRootAndSessionFlagsAreUsageErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	t.Setenv(envRoot, root)
	setOptionalEnv(t, envSession, "", false)
	setOptionalEnv(t, envBaseRoot, "", false)

	tests := []struct {
		name string
		args []string
	}{
		{name: "root", args: []string{"--root=", "--ignore-session-pin", "--me", "alice"}},
		{name: "session", args: []string{"--session=", "--me", "alice"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runDrain(tt.args)
			if err == nil || GetExitCode(err) != ExitUsage {
				t.Fatalf("blank --%s should be usage error, got %v", tt.name, err)
			}
		})
	}
}

func TestInvalidInferredSessionFallsBackToExactRootPin(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".agent-mail", "Foo.Bar")
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--root", root, "--me", "alice"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}
	if !strings.Contains(stdout, "export AM_BASE_ROOT="+shellQuotePosix(root)+"\n") ||
		!strings.Contains(stdout, "export AM_SESSION=\n") {
		t.Fatalf("invalid inferred session must become an exact-root pin: %q", stdout)
	}
	if strings.Contains(stdout, "AM_SESSION=Foo.Bar") {
		t.Fatalf("env emitted an invalid session identity: %q", stdout)
	}
	if got := coopSessionIdentity(root, "", root); got != "" {
		t.Fatalf("coop inferred invalid session %q, want sessionless identity", got)
	}
}

func applyEnvSlice(t *testing.T, env []string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		value, present := lookupEnvSlice(env, key)
		setOptionalEnv(t, key, value, present)
	}
}

func lookupEnvSlice(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func setOptionalEnv(t *testing.T, key, value string, present bool) {
	t.Helper()
	old, hadOld := os.LookupEnv(key)
	t.Cleanup(func() {
		if hadOld {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
	if present {
		if err := os.Setenv(key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	} else if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}
