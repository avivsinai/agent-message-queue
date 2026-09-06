package cli

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestParseFlagsRejectsPositionals(t *testing.T) {
	fs := flag.NewFlagSet("route explain", flag.ContinueOnError)
	laterFlag := fs.String("later", "", "test flag")

	handled, err := parseFlags(fs, []string{"stray", "--later", "value"}, nil)
	if handled {
		t.Fatal("positional rejection was reported as handled")
	}
	if err == nil {
		t.Fatal("expected positional arguments to be rejected")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), `route explain does not accept positional arguments (got "stray --later value")`) {
		t.Fatalf("error does not use flag set name and remaining arguments: %v", err)
	}
	if strings.Contains(err.Error(), "--body") {
		t.Fatalf("error suggests unavailable --body flag: %v", err)
	}
	if *laterFlag != "" {
		t.Fatalf("flag after positional was parsed as %q, want empty", *laterFlag)
	}
}

func TestParseFlagsAllowPositionalsRetainsArguments(t *testing.T) {
	fs := flag.NewFlagSet("coop exec", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "queue root")

	handled, err := parseFlagsAllowPositionals(fs, []string{"--root", "/tmp/queue", "codex", "--agent-flag"}, nil)
	if err != nil {
		t.Fatalf("parseFlagsAllowPositionals: %v", err)
	}
	if handled {
		t.Fatal("ordinary parse was reported as handled")
	}
	if *rootFlag != "/tmp/queue" {
		t.Fatalf("root = %q, want /tmp/queue", *rootFlag)
	}
	got := fs.Args()
	want := []string{"codex", "--agent-flag"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("remaining args = %#v, want %#v", got, want)
	}
}

func TestNormalizeHandle(t *testing.T) {
	if got, err := normalizeHandle("codex"); err != nil || got != "codex" {
		t.Fatalf("normalizeHandle valid: %v, %v", got, err)
	}
	if _, err := normalizeHandle("Codex"); err == nil {
		t.Fatalf("expected error for uppercase handle")
	}
	if _, err := normalizeHandle("co/dex"); err == nil {
		t.Fatalf("expected error for invalid characters")
	}
	if _, err := normalizeHandle("-codex"); err == nil {
		t.Fatalf("expected error for flag-shaped handle")
	}
	if got, err := normalizeHandle("codex_1"); err != nil || got != "codex_1" {
		t.Fatalf("normalizeHandle underscore: %v, %v", got, err)
	}
}

func TestValidateSessionName(t *testing.T) {
	valid := []string{"feature-x", "auth", "my_session", "abc123", "a-b-c"}
	for _, name := range valid {
		if err := validateSessionName(name); err != nil {
			t.Errorf("validateSessionName(%q) unexpected error: %v", name, err)
		}
	}
	invalid := []string{"", "Feature-X", "my/session", "has space", "a.b", "foo@bar"}
	for _, name := range invalid {
		if err := validateSessionName(name); err == nil {
			t.Errorf("validateSessionName(%q) expected error, got nil", name)
		}
	}
}

func TestValidateKnownHandle(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create config with known agents
	cfg := map[string]any{
		"version": 1,
		"agents":  []string{"alice", "bob"},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Known handle should pass
	if err := validateKnownHandles(root, false, "alice"); err != nil {
		t.Errorf("known handle should pass: %v", err)
	}

	// Unknown handle with strict=false should warn but not error
	if err := validateKnownHandles(root, false, "unknown"); err != nil {
		t.Errorf("unknown handle with strict=false should warn, not error: %v", err)
	}

	// Unknown handle with strict=true should error
	if err := validateKnownHandles(root, true, "unknown"); err == nil {
		t.Errorf("unknown handle with strict=true should error")
	}
}

func writeKnownAgentsConfig(t *testing.T, root string, agents []string) {
	t.Helper()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	cfg := map[string]any{
		"version": 1,
		"agents":  agents,
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestDefaultRootFromAmqrc(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "custom-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write .amqrc (base root only, coop exec defaults to session "collab")
	amqrcData, _ := json.Marshal(map[string]string{"root": "custom-root"})
	if err := os.WriteFile(filepath.Join(base, ".amqrc"), amqrcData, 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
		resetAmqrcCache()
		_ = os.Unsetenv("AM_ROOT")
	})
	_ = os.Unsetenv("AM_ROOT")
	resetAmqrcCache()

	if err := os.Chdir(base); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := defaultRoot()
	// Resolves to the literal .amqrc root
	want := filepath.Join(base, "custom-root")
	gotEval, _ := filepath.EvalSymlinks(got)
	wantEval, _ := filepath.EvalSymlinks(want)
	if gotEval != wantEval {
		t.Fatalf("defaultRoot() = %q, want %q", got, want)
	}
}

func snapshotTreeDigest(t *testing.T, root string) string {
	t.Helper()
	hasher := sha256.New()
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		_, _ = hasher.Write([]byte("<absent>"))
		return fmt.Sprintf("%x", hasher.Sum(nil))
	} else if err != nil {
		t.Fatalf("lstat snapshot root %s: %v", root, err)
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hasher, "%s\x00%#o\x00", rel, uint32(info.Mode()))
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, _ = hasher.Write(data)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = hasher.Write([]byte(target))
		}
		_, _ = hasher.Write([]byte{0})
		return nil
	}); err != nil {
		t.Fatalf("snapshot tree %s: %v", root, err)
	}
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

func TestEnvAndBareSendAgreeWithCompetingSourcesInsideAndOutsideGit(t *testing.T) {
	tests := []struct {
		name      string
		insideGit bool
		local     bool
		globalEnv bool
		homeRC    bool
		want      string
		wantError bool
	}{
		{name: "outside/home beats auto", local: true, homeRC: true, want: "home"},
		{name: "outside/global beats auto and home", local: true, globalEnv: true, homeRC: true, want: "global"},
		{name: "inside/global beats auto", insideGit: true, local: true, globalEnv: true, homeRC: true, want: "global"},
		{name: "inside/auto beats ineligible home", insideGit: true, local: true, homeRC: true, want: "local"},
		{name: "inside/home only refuses", insideGit: true, homeRC: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearSendMailboxTestEnv(t)
			fakeHome := t.TempDir()
			cwd := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.insideGit {
				if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			roots := map[string]string{
				"local":  filepath.Join(cwd, defaultCoopRoot),
				"global": filepath.Join(t.TempDir(), "global-root"),
				"home":   filepath.Join(t.TempDir(), "home-root"),
			}
			for _, root := range roots {
				for _, agent := range []string{"alice", "bob"} {
					if err := fsq.EnsureAgentDirs(root, agent); err != nil {
						t.Fatal(err)
					}
				}
				configureSendTestRoot(t, root, "alice", "bob")
			}
			if !test.local {
				if err := os.RemoveAll(roots["local"]); err != nil {
					t.Fatal(err)
				}
			}
			if test.globalEnv {
				t.Setenv(envGlobalRoot, roots["global"])
			}
			if test.homeRC {
				rcData, err := json.Marshal(map[string]string{"root": roots["home"]})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", fakeHome)
			t.Chdir(cwd)
			resetAmqrcCache()

			before := make(map[string]string, len(roots))
			for name, root := range roots {
				before[name] = snapshotTreeDigest(t, root)
			}
			resolvedEnv, _, envErr := resolveEnvConfig("", "alice")
			body := test.name
			if test.wantError {
				body = "@missing-body-must-not-be-read"
			}
			stdout, _, sendErr := captureEnvOutput(t, func() error {
				return runSend([]string{"--me", "alice", "--to", "bob", "--body", body})
			})
			if test.wantError {
				if envErr == nil || GetExitCode(envErr) != ExitContextMismatch {
					t.Fatalf("env error = %v, want context mismatch", envErr)
				}
				if sendErr == nil || GetExitCode(sendErr) != ExitContextMismatch || stdout != "" {
					t.Fatalf("bare send stdout=%q err=%v, want empty output and context mismatch", stdout, sendErr)
				}
				for name, root := range roots {
					if got := inboxCount(t, root, "bob"); got != 0 {
						t.Fatalf("%s inbox count = %d, want 0", name, got)
					}
					if after := snapshotTreeDigest(t, root); after != before[name] {
						t.Fatalf("%s tree mutated before routing refusal: before=%s after=%s", name, before[name], after)
					}
				}
				return
			}
			if envErr != nil || sendErr != nil {
				t.Fatalf("env error=%v send error=%v", envErr, sendErr)
			}
			wantRoot := roots[test.want]
			expectSamePath(t, resolveRoot(resolvedEnv), wantRoot)
			for name, root := range roots {
				wantCount := 0
				if name == test.want {
					wantCount = 1
				}
				if got := inboxCount(t, root, "bob"); got != wantCount {
					t.Fatalf("%s inbox count = %d, want %d", name, got, wantCount)
				}
			}
		})
	}
}

func TestResolveRootFindsParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".agent-mail")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	sub := filepath.Join(base, "nested", "dir")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})
	if err := os.Chdir(sub); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := resolveRoot(".agent-mail")
	want, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	gotEval, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("eval got: %v", err)
	}
	wantEval, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("eval want: %v", err)
	}
	if gotEval != wantEval {
		t.Fatalf("resolveRoot parent = %q, want %q", got, want)
	}
}
