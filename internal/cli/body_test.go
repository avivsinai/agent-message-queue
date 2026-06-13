package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// withStdin swaps os.Stdin for a regular file containing content (empty string
// for an empty stdin) and returns a restore func. A regular file is never a
// char device, so readStdinBody takes the ReadAll path deterministically.
func withStdin(t *testing.T, content string) func() {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write stdin file: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open stdin file: %v", err)
	}
	orig := os.Stdin
	os.Stdin = f
	return func() {
		os.Stdin = orig
		_ = f.Close()
	}
}

func TestReadBody(t *testing.T) {
	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "body.md")
	if err := os.WriteFile(filePath, []byte("from file"), 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}

	tests := []struct {
		name       string
		flag       string
		stdin      string
		useStdin   bool
		allowEmpty bool
		want       string
		wantErr    bool
	}{
		{name: "literal", flag: "hello", want: "hello"},
		{name: "file", flag: "@" + filePath, want: "from file"},
		{name: "dash reads stdin", flag: "-", stdin: "piped body", useStdin: true, want: "piped body"},
		{name: "at-dash reads stdin", flag: "@-", stdin: "piped body", useStdin: true, want: "piped body"},
		{name: "empty flag reads stdin", flag: "", stdin: "piped body", useStdin: true, want: "piped body"},
		{name: "dash with empty stdin errors", flag: "-", stdin: "", useStdin: true, wantErr: true},
		{name: "empty flag with empty stdin errors", flag: "", stdin: "", useStdin: true, wantErr: true},
		{name: "whitespace-only literal errors", flag: "   ", wantErr: true},
		{name: "dash with empty stdin allowed", flag: "-", stdin: "", useStdin: true, allowEmpty: true, want: ""},
		{name: "whitespace-only literal allowed", flag: "   ", allowEmpty: true, want: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.useStdin {
				restore := withStdin(t, tt.stdin)
				defer restore()
			}
			got, err := readBody(tt.flag, tt.allowEmpty)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got body %q", got)
				}
				if !strings.Contains(err.Error(), "empty body") {
					t.Errorf("error should mention empty body, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSend_DashBodyDoesNotDeliverLiteralHyphen guards the reported footgun: a
// send with --body - and no piped input must fail visibly instead of silently
// delivering a "-" (or blank) body to the recipient.
func TestSend_DashBodyDoesNotDeliverLiteralHyphen(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	restore := withStdin(t, "")
	defer restore()

	err := runSend([]string{"--root", root, "--me", "alice", "--to", "bob", "--subject", "evidence", "--body", "-"})
	if err == nil {
		t.Fatal("expected error for empty --body -, got nil")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "empty body") {
		t.Errorf("error should mention empty body, got: %v", err)
	}

	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "bob"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected bob inbox to remain empty, got %d message(s)", len(entries))
	}
}

// TestSend_AllowEmptyDeliversBlankBody confirms the explicit escape hatch still
// lets a blank body through when the sender opts in.
func TestSend_AllowEmptyDeliversBlankBody(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}

	restore := withStdin(t, "")
	defer restore()

	err := runSend([]string{"--root", root, "--me", "alice", "--to", "bob", "--subject", "fyi", "--body", "-", "--allow-empty"})
	if err != nil {
		t.Fatalf("unexpected error with --allow-empty: %v", err)
	}

	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "bob"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in bob inbox, got %d", len(entries))
	}
}
