package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// receipts list/wait honor --strict for an unrostered handle, and thread
// honors it for a corrupt roster instead of falling back to a directory scan
// (audit 2026-11-10).
func TestReceiptsAndThreadHonorStrict(t *testing.T) {
	root := initializedSendMailboxRoot(t, "claude")
	if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := initializedSendMailboxRoot(t, "claude")

	cases := []struct {
		name string
		run  func() error
	}{
		{"receipts list unknown handle", func() error {
			return runReceiptsList([]string{"--root", good, "--me", "ghost", "--strict"})
		}},
		{"receipts wait unknown handle", func() error {
			return runReceiptsWait([]string{"--root", good, "--me", "ghost", "--msg-id", "m1", "--strict", "--timeout", "10ms"})
		}},
		{"thread corrupt roster", func() error {
			return runThread([]string{"--root", root, "--id", "t1", "--strict"})
		}},
	}
	for _, tc := range cases {
		if err := tc.run(); err == nil || strings.Contains(err.Error(), "timed out") {
			t.Errorf("%s: --strict gave %v, want the roster error", tc.name, err)
		}
	}
	if err := runThread([]string{"--root", root, "--id", "t1"}); err != nil {
		t.Errorf("thread without --strict on a corrupt roster: %v, want fallback", err)
	}
}
