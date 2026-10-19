package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review finding from PR #252's discovery regression: known bases must not be
// reclassified by display hints, including before their first session exists.
func TestDisplayClassificationPreservesKnownBase(t *testing.T) {
	clearSendMailboxTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	base := filepath.Join(home, defaultCoopRoot)
	for _, dir := range []string{base, filepath.Join(home, ".claude", "agents")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := classifyRootForDisplay(base); got != base {
		t.Errorf("display base = %q, want known base %q", got, base)
	}
	if got := resolveSessionNameForDisplay(base); got != "" {
		t.Errorf("base has display session %q, want none", got)
	}
	text, err := captureEnvStdout(t, func() error {
		return runWho([]string{"--root", base})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "Base root: "+base+"\n") {
		t.Errorf("empty known base must remain the discovery base, got:\n%s", text)
	}
}
