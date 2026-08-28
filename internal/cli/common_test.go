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
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
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

func TestParseFlagsPositionalBodyHint(t *testing.T) {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.String("body", "", "message body")

	_, err := parseFlags(fs, []string{"message text"}, nil)
	if err == nil {
		t.Fatal("expected positional arguments to be rejected")
	}
	if !strings.Contains(err.Error(), "--body") {
		t.Fatalf("error should suggest registered --body flag: %v", err)
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

func TestValidateKnownHandlesAllowsReservedUserWithConfig(t *testing.T) {
	root := t.TempDir()
	writeKnownAgentsConfig(t, root, []string{"alice", "bob"})

	if err := validateKnownHandles(root, true, "user"); err != nil {
		t.Fatalf("reserved user handle should pass strict validation: %v", err)
	}
	if err := validateKnownHandles(root, true, "alice", "user"); err != nil {
		t.Fatalf("mixed configured and reserved handles should pass: %v", err)
	}
	if err := validateKnownHandles(root, true, "unknown"); err == nil {
		t.Fatal("unknown handle should still fail strict validation")
	}
}

func TestLoadKnownAgentsDistinguishesAbsentFromPresentEmptyConfig(t *testing.T) {
	root := t.TempDir()

	agents, err := loadKnownAgents(root, true)
	if err != nil {
		t.Fatalf("loadKnownAgents without config: %v", err)
	}
	if agents != nil {
		t.Fatalf("no config should return nil agents, got %#v", agents)
	}
	known, err := loadKnownAgentSet(root, true)
	if err != nil {
		t.Fatalf("loadKnownAgentSet without config: %v", err)
	}
	if known != nil {
		t.Fatalf("no config should return nil known set, got %#v", known)
	}

	writeKnownAgentsConfig(t, root, []string{})
	agents, err = loadKnownAgents(root, true)
	if err != nil {
		t.Fatalf("loadKnownAgents with empty config: %v", err)
	}
	if len(agents) != 1 || agents[0] != reservedHumanHandle {
		t.Fatalf("empty configured agents = %#v, want reserved user", agents)
	}
	known, err = loadKnownAgentSet(root, true)
	if err != nil {
		t.Fatalf("loadKnownAgentSet with empty config: %v", err)
	}
	if len(known) != 1 {
		t.Fatalf("empty configured known set = %#v, want reserved user only", known)
	}
	if _, ok := known[reservedHumanHandle]; !ok {
		t.Fatalf("empty configured known set = %#v, want reserved user", known)
	}
}

func TestHeaderValidatorAllowsReservedUserUnderStrict(t *testing.T) {
	root := t.TempDir()
	writeKnownAgentsConfig(t, root, []string{"claude", "codex"})

	validator, err := newHeaderValidator(root, true)
	if err != nil {
		t.Fatalf("newHeaderValidator: %v", err)
	}
	header := format.Header{
		Schema:  format.CurrentSchema,
		ID:      "operator-gate",
		From:    "claude",
		To:      []string{"user"},
		Thread:  "p2p/claude__user",
		Created: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := validator.validate(header); err != nil {
		t.Fatalf("strict validator should accept reserved user recipient: %v", err)
	}

	header.To = []string{"unknown"}
	if err := validator.validate(header); err == nil {
		t.Fatal("strict validator should still reject unknown recipients")
	}
}

func TestValidateKnownHandleNoConfig(t *testing.T) {
	root := t.TempDir()

	// No config file - should pass any handle
	if err := validateKnownHandles(root, true, "anyhandle"); err != nil {
		t.Errorf("no config should pass any handle: %v", err)
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

func TestValidateKnownHandleCorruptConfig(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write invalid JSON
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt config: %v", err)
	}

	// Corrupt config with strict=false should warn but not error
	if err := validateKnownHandles(root, false, "alice"); err != nil {
		t.Errorf("corrupt config with strict=false should warn, not error: %v", err)
	}

	// Corrupt config with strict=true should error
	if err := validateKnownHandles(root, true, "alice"); err == nil {
		t.Errorf("corrupt config with strict=true should error")
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

func TestDefaultRootEnvOverridesAmqrc(t *testing.T) {
	base := t.TempDir()

	// Write .amqrc with one root
	amqrcData, _ := json.Marshal(map[string]string{"root": "amqrc-root"})
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
	resetAmqrcCache()

	if err := os.Chdir(base); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Set AM_ROOT to a different value
	t.Setenv("AM_ROOT", "/env/root")

	got := defaultRoot()
	if got != "/env/root" {
		t.Fatalf("defaultRoot() = %q, want %q (env should override .amqrc)", got, "/env/root")
	}
}

func TestDefaultRootFallbackNoAmqrc(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envGlobalRoot, "")

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
	want := filepath.Join(base, defaultCoopRoot)
	expectSamePath(t, got, want)
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

func TestDefaultRootExplicitGlobalWinsOverRepoLocalAgentMail(t *testing.T) {
	fakeHome := t.TempDir()
	projectDir := filepath.Join(fakeHome, "workspace", "snagline")
	localRoot := filepath.Join(projectDir, defaultCoopRoot)
	globalEnvRoot := filepath.Join(fakeHome, "global-env-root")
	globalRCRoot := filepath.Join(fakeHome, "global-rc-root")
	for _, dir := range []string{localRoot, globalEnvRoot, globalRCRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	rcData, err := json.Marshal(map[string]string{"root": globalRCRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatalf("write global .amqrc: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	t.Setenv("HOME", fakeHome)
	t.Setenv(envRoot, "")
	t.Setenv(envGlobalRoot, globalEnvRoot)
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	expectSamePath(t, resolveRoot(defaultRoot()), globalEnvRoot)
}

func TestDefaultRootGlobalFallbacksOutsideLocalTree(t *testing.T) {
	tests := []struct {
		name      string
		globalEnv bool
	}{
		{name: "AMQ_GLOBAL_ROOT", globalEnv: true},
		{name: "home amqrc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeHome := t.TempDir()
			cwd := t.TempDir()
			globalEnvRoot := filepath.Join(fakeHome, "global-env-root")
			globalRCRoot := filepath.Join(fakeHome, "global-rc-root")
			for _, dir := range []string{globalEnvRoot, globalRCRoot} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			rcData, err := json.Marshal(map[string]string{"root": globalRCRoot})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
				t.Fatalf("write global .amqrc: %v", err)
			}

			oldDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.Chdir(oldDir)
				resetAmqrcCache()
			})
			t.Setenv("HOME", fakeHome)
			t.Setenv(envRoot, "")
			if tt.globalEnv {
				t.Setenv(envGlobalRoot, globalEnvRoot)
			} else {
				t.Setenv(envGlobalRoot, "")
			}
			resetAmqrcCache()
			if err := os.Chdir(cwd); err != nil {
				t.Fatal(err)
			}

			want := globalRCRoot
			if tt.globalEnv {
				want = globalEnvRoot
			}
			expectSamePath(t, resolveRoot(defaultRoot()), want)
		})
	}
}

func TestEnvAndImplicitCommandRootResolutionAgreeAcrossPrecedence(t *testing.T) {
	for _, source := range []string{
		"explicit root",
		"AM_ROOT",
		"project amqrc",
		"repo-local auto-detect",
		"AMQ_GLOBAL_ROOT",
		"home amqrc",
	} {
		t.Run(source, func(t *testing.T) {
			fakeHome := t.TempDir()
			cwd := t.TempDir()
			targetRoot := filepath.Join(t.TempDir(), "target-root")
			rootFlag := ""

			oldDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.Chdir(oldDir)
				resetAmqrcCache()
			})
			t.Setenv("HOME", fakeHome)
			for _, key := range []string{envRoot, envBaseRoot, envSession, envGlobalRoot} {
				t.Setenv(key, "")
			}

			switch source {
			case "explicit root":
				rootFlag = targetRoot
			case "AM_ROOT":
				t.Setenv(envRoot, targetRoot)
			case "project amqrc":
				rcData, marshalErr := json.Marshal(map[string]string{"root": targetRoot})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if writeErr := os.WriteFile(filepath.Join(cwd, ".amqrc"), rcData, 0o600); writeErr != nil {
					t.Fatalf("write project .amqrc: %v", writeErr)
				}
			case "repo-local auto-detect":
				targetRoot = filepath.Join(cwd, defaultCoopRoot)
			case "AMQ_GLOBAL_ROOT":
				t.Setenv(envGlobalRoot, targetRoot)
			case "home amqrc":
				rcData, marshalErr := json.Marshal(map[string]string{"root": targetRoot})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if writeErr := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); writeErr != nil {
					t.Fatalf("write home .amqrc: %v", writeErr)
				}
			default:
				t.Fatalf("unhandled root source %q", source)
			}
			if err := os.MkdirAll(targetRoot, 0o700); err != nil {
				t.Fatalf("create target root: %v", err)
			}
			resetAmqrcCache()
			if err := os.Chdir(cwd); err != nil {
				t.Fatal(err)
			}

			envRoot, _, err := resolveEnvConfig(rootFlag, "alice")
			if err != nil {
				t.Fatalf("resolve env root: %v", err)
			}

			fs := flag.NewFlagSet("participating-command", flag.ContinueOnError)
			common := addCommonFlags(fs)
			var args []string
			if rootFlag != "" {
				args = []string{"--root", rootFlag}
			}
			if handled, parseErr := parseFlags(fs, args, nil); handled || parseErr != nil {
				t.Fatalf("parse implicit command root: handled=%v err=%v", handled, parseErr)
			}

			expectSamePath(t, resolveRoot(common.Root), resolveRoot(envRoot))
			expectSamePath(t, resolveRoot(common.Root), targetRoot)
		})
	}
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

func TestOutsideGitHomeAgentMailOnlyCannotReenterParentDiscovery(t *testing.T) {
	clearSendMailboxTestEnv(t)
	fakeHome := t.TempDir()
	homeRoot := filepath.Join(fakeHome, defaultCoopRoot)
	configureSendTestRoot(t, homeRoot, "alice", "bob")
	cwd := filepath.Join(fakeHome, "workspace", "plain")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Chdir(cwd)
	resetAmqrcCache()

	before := snapshotTreeDigest(t, fakeHome)
	if _, _, _, err := resolveEnvConfigWithSource("", "alice"); err == nil {
		t.Fatal("env unexpectedly accepted an ancestor HOME/.agent-mail without ~/.amqrc")
	}
	stdout, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--me", "alice",
			"--to", "bob",
			"--body", "@missing-body-must-not-be-read",
		})
	})
	if err == nil {
		t.Fatal("bare send unexpectedly accepted an ancestor HOME/.agent-mail")
	}
	if stdout != "" {
		t.Fatalf("bare send emitted output before refusal: %q", stdout)
	}
	if after := snapshotTreeDigest(t, fakeHome); after != before {
		t.Fatalf("HOME tree mutated: before=%s after=%s", before, after)
	}
	if _, statErr := os.Lstat(filepath.Join(cwd, defaultCoopRoot)); !os.IsNotExist(statErr) {
		t.Fatalf("cwd-local fallback was created: %v", statErr)
	}
}

func TestRelativeHighPrecedenceRootsCanonicalizeBeforeEnvSendAndDoctor(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*testing.T, string)
		envArgs   []string
		sendArgs  []string
		doctorArg []string
	}{
		{
			name: "explicit root with existing ancestor pin",
			setup: func(t *testing.T, root string) {
				t.Setenv(envRoot, root)
				t.Setenv(envBaseRoot, root)
				t.Setenv(envSession, "")
			},
			envArgs:   []string{"--root", defaultCoopRoot},
			sendArgs:  []string{"--root", defaultCoopRoot},
			doctorArg: []string{"--root", defaultCoopRoot},
		},
		{
			name: "AM_ROOT",
			setup: func(t *testing.T, _ string) {
				t.Setenv(envRoot, defaultCoopRoot)
			},
		},
		{
			name: "AMQ_GLOBAL_ROOT",
			setup: func(t *testing.T, _ string) {
				t.Setenv(envGlobalRoot, defaultCoopRoot)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearSendMailboxTestEnv(t)
			fakeHome := t.TempDir()
			repo := filepath.Join(t.TempDir(), "repo")
			nested := filepath.Join(repo, "sub")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			targetRoot := filepath.Join(repo, defaultCoopRoot)
			configureSendTestRoot(t, targetRoot, "alice", "bob")
			t.Setenv("HOME", fakeHome)
			test.setup(t, canonicalTestPath(t, targetRoot))
			t.Chdir(nested)
			resetAmqrcCache()

			jsonArgs := append(append([]string{}, test.envArgs...), "--me", "alice", "--json")
			stdout, _, err := captureEnvOutput(t, func() error {
				return runEnv(jsonArgs)
			})
			if err != nil {
				t.Fatalf("env json: %v", err)
			}
			var output envOutput
			if err := json.Unmarshal([]byte(stdout), &output); err != nil {
				t.Fatalf("decode env json: %v\n%s", err, stdout)
			}
			if !filepath.IsAbs(output.Root) || !filepath.IsAbs(output.BaseRoot) {
				t.Fatalf("env returned non-absolute root=%q base=%q", output.Root, output.BaseRoot)
			}
			expectSamePath(t, output.Root, targetRoot)
			expectSamePath(t, output.BaseRoot, targetRoot)

			shellArgs := append(append([]string{}, test.envArgs...), "--me", "alice")
			stdout, _, err = captureEnvOutput(t, func() error {
				return runEnv(shellArgs)
			})
			if err != nil {
				t.Fatalf("env shell: %v", err)
			}
			if want := "export AM_ROOT=" + shellQuotePosix(canonicalTestPath(t, targetRoot)) + "\n"; !strings.Contains(stdout, want) {
				t.Fatalf("env shell omitted canonical root %q:\n%s", want, stdout)
			}

			sendArgs := append(append([]string{}, test.sendArgs...),
				"--me", "alice",
				"--to", "bob",
				"--body", test.name,
			)
			if _, _, err := captureEnvOutput(t, func() error {
				return runSend(sendArgs)
			}); err != nil {
				t.Fatalf("send: %v", err)
			}
			if got := inboxCount(t, targetRoot, "bob"); got != 1 {
				t.Fatalf("ancestor queue delivery count = %d, want 1", got)
			}

			doctorArgs := append(append([]string{}, test.doctorArg...), "--json")
			stdout, _, err = captureEnvOutput(t, func() error {
				return runDoctor(doctorArgs)
			})
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			var doctor doctorResult
			if err := json.Unmarshal([]byte(stdout), &doctor); err != nil {
				t.Fatalf("decode doctor json: %v\n%s", err, stdout)
			}
			foundRoot := false
			for _, check := range doctor.Checks {
				if check.Name == "Root directory" {
					foundRoot = true
					expectSamePath(t, check.Message, targetRoot)
				}
			}
			if !foundRoot {
				t.Fatal("doctor omitted Root directory check")
			}

			if _, statErr := os.Lstat(filepath.Join(nested, defaultCoopRoot)); !os.IsNotExist(statErr) {
				t.Fatalf("nested queue was created or mutated: %v", statErr)
			}
		})
	}
}

func TestBareGitRepositoryUsesSameFailClosedResolverForEnvAndSend(t *testing.T) {
	for _, local := range []bool{false, true} {
		name := map[bool]string{false: "home only refuses", true: "local queue is eligible"}[local]
		t.Run(name, func(t *testing.T) {
			clearSendMailboxTestEnv(t)
			fakeHome := t.TempDir()
			bare := filepath.Join(t.TempDir(), "repo.git")
			for _, dir := range []string{
				filepath.Join(bare, "objects"),
				filepath.Join(bare, "refs"),
			} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(bare, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			homeRoot := filepath.Join(fakeHome, "home-root")
			localRoot := filepath.Join(bare, defaultCoopRoot)
			for _, root := range []string{homeRoot, localRoot} {
				for _, agent := range []string{"alice", "bob"} {
					if err := fsq.EnsureAgentDirs(root, agent); err != nil {
						t.Fatal(err)
					}
				}
				configureSendTestRoot(t, root, "alice", "bob")
			}
			if !local {
				if err := os.RemoveAll(localRoot); err != nil {
					t.Fatal(err)
				}
			}
			rcData, err := json.Marshal(map[string]string{"root": homeRoot})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", fakeHome)
			t.Setenv("PATH", "")
			t.Chdir(bare)

			envResolved, _, envErr := resolveEnvConfig("", "alice")
			stdout, _, sendErr := captureEnvOutput(t, func() error {
				return runSend([]string{"--me", "alice", "--to", "bob", "--body", name})
			})
			if !local {
				if envErr == nil || GetExitCode(envErr) != ExitContextMismatch {
					t.Fatalf("env error = %v, want context mismatch", envErr)
				}
				if sendErr == nil || GetExitCode(sendErr) != ExitContextMismatch || stdout != "" {
					t.Fatalf("send stdout=%q err=%v, want empty output and context mismatch", stdout, sendErr)
				}
				if got := inboxCount(t, homeRoot, "bob"); got != 0 {
					t.Fatalf("home inbox count = %d, want 0", got)
				}
				return
			}
			if envErr != nil || sendErr != nil {
				t.Fatalf("env error=%v send error=%v", envErr, sendErr)
			}
			expectSamePath(t, resolveRoot(envResolved), localRoot)
			if got := inboxCount(t, localRoot, "bob"); got != 1 {
				t.Fatalf("local inbox count = %d, want 1", got)
			}
			if got := inboxCount(t, homeRoot, "bob"); got != 0 {
				t.Fatalf("home inbox count = %d, want 0", got)
			}
		})
	}
}

func TestPlainSendPrefersExplicitGlobalRootOverRepoLocalAgentMail(t *testing.T) {
	clearSendMailboxTestEnv(t)
	fakeHome := t.TempDir()
	projectDir := filepath.Join(fakeHome, "workspace", "snagline")
	localRoot := filepath.Join(projectDir, defaultCoopRoot)
	globalEnvRoot := filepath.Join(fakeHome, "global-env-root")
	globalRCRoot := filepath.Join(fakeHome, "global-rc-root")
	for _, root := range []string{localRoot, globalEnvRoot, globalRCRoot} {
		for _, agent := range []string{"alice", "bob"} {
			if err := fsq.EnsureAgentDirs(root, agent); err != nil {
				t.Fatalf("EnsureAgentDirs(%s, %s): %v", root, agent, err)
			}
		}
		configureSendTestRoot(t, root, "alice", "bob")
	}
	rcData, err := json.Marshal(map[string]string{"root": globalRCRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatalf("write global .amqrc: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	t.Setenv("HOME", fakeHome)
	t.Setenv(envGlobalRoot, globalEnvRoot)
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{"--me", "alice", "--to", "bob", "--body", "repo-local"})
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := inboxCount(t, globalEnvRoot, "bob"); got != 1 {
		t.Fatalf("AMQ_GLOBAL_ROOT inbox count = %d, want 1", got)
	}
	for name, root := range map[string]string{
		"repo-local": localRoot,
		"home amqrc": globalRCRoot,
	} {
		if got := inboxCount(t, root, "bob"); got != 0 {
			t.Fatalf("%s fallback inbox count = %d, want 0", name, got)
		}
	}
}

func TestPlainSendUsesGlobalFallbackOutsideLocalTree(t *testing.T) {
	clearSendMailboxTestEnv(t)
	fakeHome := t.TempDir()
	cwd := t.TempDir()
	globalRoot := filepath.Join(fakeHome, "global-root")
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(globalRoot, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	configureSendTestRoot(t, globalRoot, "alice", "bob")

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	t.Setenv("HOME", fakeHome)
	t.Setenv(envGlobalRoot, globalRoot)
	resetAmqrcCache()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{"--me", "alice", "--to", "bob", "--body", "global fallback"})
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := inboxCount(t, globalRoot, "bob"); got != 1 {
		t.Fatalf("global fallback inbox count = %d, want 1", got)
	}
}

func TestPlainSendRejectsBrokenProjectAmqrcBeforeGlobalFallback(t *testing.T) {
	tests := []struct {
		name      string
		globalEnv bool
	}{
		{name: "AMQ_GLOBAL_ROOT", globalEnv: true},
		{name: "home amqrc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearSendMailboxTestEnv(t)
			fakeHome := t.TempDir()
			projectDir := filepath.Join(fakeHome, "workspace", "broken-project")
			globalRoot := filepath.Join(fakeHome, "global-root")
			if err := os.MkdirAll(projectDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, agent := range []string{"alice", "bob"} {
				if err := fsq.EnsureAgentDirs(globalRoot, agent); err != nil {
					t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
				}
			}
			configureSendTestRoot(t, globalRoot, "alice", "bob")
			if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), []byte("{"), 0o600); err != nil {
				t.Fatalf("write project .amqrc: %v", err)
			}
			if tt.globalEnv {
				t.Setenv(envGlobalRoot, globalRoot)
			} else {
				rcData, err := json.Marshal(map[string]string{"root": globalRoot})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
					t.Fatalf("write global .amqrc: %v", err)
				}
			}

			oldDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.Chdir(oldDir)
				resetAmqrcCache()
			})
			t.Setenv("HOME", fakeHome)
			resetAmqrcCache()
			if err := os.Chdir(projectDir); err != nil {
				t.Fatal(err)
			}

			err = runSend([]string{
				"--me", "alice",
				"--to", "bob",
				"--body", "@missing-body-must-not-be-read",
			})
			if err == nil || !strings.Contains(err.Error(), "invalid .amqrc") {
				t.Fatalf("send error = %v, want broken project .amqrc", err)
			}
			if got := inboxCount(t, globalRoot, "bob"); got != 0 {
				t.Fatalf("global fallback received %d messages, want 0", got)
			}
		})
	}
}

func TestPlainSendExplicitRootsOverrideBrokenProjectAmqrc(t *testing.T) {
	tests := []struct {
		name string
		args func(root string) []string
		env  func(t *testing.T, root string)
	}{
		{
			name: "explicit root",
			args: func(root string) []string {
				return []string{"--root", root, "--me", "alice", "--to", "bob", "--body", "explicit"}
			},
			env: func(*testing.T, string) {},
		},
		{
			name: "AM_ROOT",
			args: func(string) []string {
				return []string{"--me", "alice", "--to", "bob", "--body", "environment"}
			},
			env: func(t *testing.T, root string) {
				t.Setenv(envRoot, root)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearSendMailboxTestEnv(t)
			fakeHome := t.TempDir()
			projectDir := filepath.Join(fakeHome, "workspace", "broken-project")
			targetRoot := filepath.Join(fakeHome, "target-root")
			if err := os.MkdirAll(projectDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, agent := range []string{"alice", "bob"} {
				if err := fsq.EnsureAgentDirs(targetRoot, agent); err != nil {
					t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
				}
			}
			configureSendTestRoot(t, targetRoot, "alice", "bob")
			if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), []byte("{"), 0o600); err != nil {
				t.Fatalf("write project .amqrc: %v", err)
			}

			oldDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.Chdir(oldDir)
				resetAmqrcCache()
			})
			t.Setenv("HOME", fakeHome)
			tt.env(t, targetRoot)
			resetAmqrcCache()
			if err := os.Chdir(projectDir); err != nil {
				t.Fatal(err)
			}

			if err := runSend(tt.args(targetRoot)); err != nil {
				t.Fatalf("send with higher-precedence root: %v", err)
			}
			if got := inboxCount(t, targetRoot, "bob"); got != 1 {
				t.Fatalf("target inbox count = %d, want 1", got)
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

func TestResolveRootCurrentDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".agent-mail")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})
	if err := os.Chdir(base); err != nil {
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
		t.Fatalf("resolveRoot cwd = %q, want %q", got, want)
	}
}

func TestClassifyRootIgnoresStaleEnvBaseRootAndUsesAmqrc(t *testing.T) {
	t.Setenv("AM_BASE_ROOT", filepath.Join(t.TempDir(), "stale-base"))

	projectDir := t.TempDir()
	baseRoot := filepath.Join(projectDir, ".agent-mail")
	sessionRoot := filepath.Join(baseRoot, "collab")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatalf("mkdir session root: %v", err)
	}

	rc := map[string]any{"root": ".agent-mail"}
	rcData, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if got := classifyRoot(sessionRoot); got != baseRoot {
		t.Fatalf("classifyRoot(%q) = %q, want %q", sessionRoot, got, baseRoot)
	}
}

func TestClassifyRootDefaultLayoutConvention(t *testing.T) {
	t.Setenv("AM_BASE_ROOT", "")

	baseRoot := filepath.Join(t.TempDir(), defaultCoopRoot)
	sessionRoot := filepath.Join(baseRoot, "collab")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatalf("mkdir session root: %v", err)
	}

	if got := classifyRoot(sessionRoot); got != baseRoot {
		t.Fatalf("classifyRoot(%q) = %q, want %q", sessionRoot, got, baseRoot)
	}
}

func TestBaseRootOfExactEnvBaseWinsOverUnrelatedSibling(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, defaultCoopRoot)
	sessionRoot := filepath.Join(baseRoot, "collab")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatalf("mkdir session root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(parent, ".claude", "agents"), 0o700); err != nil {
		t.Fatalf("mkdir unrelated agents dir: %v", err)
	}
	t.Setenv("AM_BASE_ROOT", baseRoot)

	if got := classifyRoot(baseRoot); got != "" {
		t.Fatalf("classifyRoot(%q) = %q, want empty for a base root", baseRoot, got)
	}
	if got := baseRootOf(baseRoot); got != baseRoot {
		t.Fatalf("baseRootOf(%q) = %q, want exact env base %q", baseRoot, got, baseRoot)
	}
	if !sameBaseTree(baseRoot, sessionRoot) {
		t.Fatalf("sameBaseTree(%q, %q) = false, want true", baseRoot, sessionRoot)
	}
}

func TestClassifyRootCustomRootDoesNotMatchDefaultLayoutConvention(t *testing.T) {
	t.Setenv("AM_BASE_ROOT", "")

	baseRoot := filepath.Join(t.TempDir(), "my-queue")
	sessionRoot := filepath.Join(baseRoot, "collab")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatalf("mkdir session root: %v", err)
	}

	if got := classifyRoot(sessionRoot); got != "" {
		t.Fatalf("classifyRoot(%q) = %q, want empty", sessionRoot, got)
	}
}

func TestSessionName(t *testing.T) {
	tests := []struct {
		root string
		want string
	}{
		{"/path/to/.agent-mail/team", "team"},
		{"/path/to/.agent-mail/auth", "auth"},
		{".agent-mail/team", "team"},
		{"/single", "single"},
	}
	for _, tt := range tests {
		got := sessionName(tt.root)
		if got != tt.want {
			t.Errorf("sessionName(%q) = %q, want %q", tt.root, got, tt.want)
		}
	}
}
