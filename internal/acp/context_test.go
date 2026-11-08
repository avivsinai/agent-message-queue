package acp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// clearContextEnv removes every inherited AMQ context variable so each case
// declares its own complete environment.
func clearContextEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvRoot, EnvMe, EnvTo, EnvBaseRoot, EnvSession, EnvRootID, EnvBaseRootID} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() { _ = os.Unsetenv(name) })
	}
}

// sessionRoot builds a base root with one session child, mirroring the layout
// amq creates for a named session.
func sessionRoot(t *testing.T, session string) (base string, root string) {
	t.Helper()
	base = t.TempDir()
	root = filepath.Join(base, session)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create session root: %v", err)
	}
	return base, root
}

// requireContextError asserts the refusal is typed so the caller can report the
// repository's context-mismatch exit code.
func requireContextError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("LoadConfig succeeded, want a fail-closed refusal")
	}
	var contextErr *ContextError
	if !errors.As(err, &contextErr) {
		t.Fatalf("error %v is not a ContextError, so the caller cannot report exit code 5", err)
	}
	if contextErr.Error() == "" {
		t.Error("refusal carried no explanation")
	}
}

func TestLoadConfigRefusesMissingRoot(t *testing.T) {
	clearContextEnv(t)
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	requireContextError(t, mustLoadConfigError(t))
}

func TestLoadConfigRefusesRelativeRoot(t *testing.T) {
	clearContextEnv(t)
	t.Setenv(EnvRoot, "relative/queue")
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	requireContextError(t, mustLoadConfigError(t))
}

func TestLoadConfigRefusesUnusableHandles(t *testing.T) {
	cases := map[string]struct{ me, to string }{
		"missing sender":      {me: "", to: testTo},
		"missing recipient":   {me: testMe, to: ""},
		"traversal sender":    {me: "../escape", to: testTo},
		"traversal dest":      {me: testMe, to: "../escape"},
		"uppercase dest":      {me: testMe, to: "Codex"},
		"leading dash dest":   {me: testMe, to: "-codex"},
		"path separator dest": {me: testMe, to: "codex/inner"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clearContextEnv(t)
			t.Setenv(EnvRoot, t.TempDir())
			t.Setenv(EnvMe, tc.me)
			t.Setenv(EnvTo, tc.to)

			requireContextError(t, mustLoadConfigError(t))
		})
	}
}

func TestLoadConfigAcceptsUnpinnedRoot(t *testing.T) {
	clearContextEnv(t)
	root := t.TempDir()
	t.Setenv(EnvRoot, root)
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Root != root || cfg.Me != testMe || cfg.To != testTo {
		t.Fatalf("cfg = %+v, want root %s, me %s, to %s", cfg, root, testMe, testTo)
	}
}

// TestLoadConfigRefusesRootThatContradictsSessionPin is the load-bearing
// negative case. An implementation that simply trusted AM_ROOT would happily
// deliver into a foreign tree while the shell was pinned somewhere else.
func TestLoadConfigRefusesRootThatContradictsSessionPin(t *testing.T) {
	clearContextEnv(t)
	base, _ := sessionRoot(t, "session1")
	foreign := t.TempDir()

	t.Setenv(EnvRoot, foreign)
	t.Setenv(EnvBaseRoot, base)
	t.Setenv(EnvSession, "session1")
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	requireContextError(t, mustLoadConfigError(t))
}

func TestLoadConfigAcceptsMatchingSessionPin(t *testing.T) {
	clearContextEnv(t)
	base, root := sessionRoot(t, "session1")

	t.Setenv(EnvRoot, root)
	t.Setenv(EnvBaseRoot, base)
	t.Setenv(EnvSession, "session1")
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Root != root {
		t.Fatalf("cfg.Root = %s, want the pinned session root %s", cfg.Root, root)
	}
}

// TestLoadConfigRefusesPinEvidenceWithoutBaseRoot mirrors amq's incomplete-pin
// refusal: partial evidence must not be resolved by guessing the base.
func TestLoadConfigRefusesPinEvidenceWithoutBaseRoot(t *testing.T) {
	evidence := map[string]string{EnvSession: "session1", EnvRootID: "v1:x:1:1", EnvBaseRootID: "v1:x:1:1"}
	for name, value := range evidence {
		t.Run(name, func(t *testing.T) {
			clearContextEnv(t)
			t.Setenv(EnvRoot, t.TempDir())
			t.Setenv(EnvMe, testMe)
			t.Setenv(EnvTo, testTo)
			t.Setenv(name, value)

			requireContextError(t, mustLoadConfigError(t))
		})
	}
}

func TestLoadConfigRefusesInvalidSessionName(t *testing.T) {
	clearContextEnv(t)
	base := t.TempDir()
	t.Setenv(EnvRoot, filepath.Join(base, "bad"))
	t.Setenv(EnvBaseRoot, base)
	t.Setenv(EnvSession, "../escape")
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	requireContextError(t, mustLoadConfigError(t))
}

func TestLoadConfigAcceptsAuthenticatedIdentityPin(t *testing.T) {
	clearContextEnv(t)
	base, root := sessionRoot(t, "session1")

	t.Setenv(EnvRoot, root)
	t.Setenv(EnvBaseRoot, base)
	t.Setenv(EnvSession, "session1")
	t.Setenv(EnvRootID, treeIdentity(t, root))
	t.Setenv(EnvBaseRootID, treeIdentity(t, base))
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	if _, err := LoadConfig(); err != nil {
		t.Fatalf("LoadConfig with a matching identity pin: %v", err)
	}
}

// TestLoadConfigRefusesReplacedTree proves an identity pin is authenticated
// rather than merely present: recreating the directory at the same path must not
// pass. A path-only check would accept this.
func TestLoadConfigRefusesReplacedTree(t *testing.T) {
	clearContextEnv(t)
	base, root := sessionRoot(t, "session1")
	staleRootID := treeIdentity(t, root)
	baseID := treeIdentity(t, base)

	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove session root: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("recreate session root: %v", err)
	}
	if treeIdentity(t, root) == staleRootID {
		t.Skip("filesystem reused the inode for the recreated directory")
	}

	t.Setenv(EnvRoot, root)
	t.Setenv(EnvBaseRoot, base)
	t.Setenv(EnvSession, "session1")
	t.Setenv(EnvRootID, staleRootID)
	t.Setenv(EnvBaseRootID, baseID)
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	requireContextError(t, mustLoadConfigError(t))
}

func TestLoadConfigRefusesHalfIdentityPin(t *testing.T) {
	clearContextEnv(t)
	base, root := sessionRoot(t, "session1")

	t.Setenv(EnvRoot, root)
	t.Setenv(EnvBaseRoot, base)
	t.Setenv(EnvSession, "session1")
	t.Setenv(EnvRootID, treeIdentity(t, root))
	t.Setenv(EnvBaseRootID, "")
	t.Setenv(EnvMe, testMe)
	t.Setenv(EnvTo, testTo)

	requireContextError(t, mustLoadConfigError(t))
}

func treeIdentity(t *testing.T, path string) string {
	t.Helper()
	token, err := fsq.StableTreeIdentity(path)
	if err != nil {
		t.Fatalf("resolve tree identity for %s: %v", path, err)
	}
	return token
}

func mustLoadConfigError(t *testing.T) error {
	t.Helper()
	cfg, err := LoadConfig()
	if err == nil {
		t.Fatalf("LoadConfig returned %+v, want a refusal", cfg)
	}
	return err
}
