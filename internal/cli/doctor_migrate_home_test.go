package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/hookinstall"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/launchd"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestDoctorMigrateHomeMovesLaunchTrust(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdgState := filepath.Join(base, "xdg-state")
	project := filepath.Join(base, "project")
	emptyPath := filepath.Join(base, "bin")
	for _, dir := range []string{home, project, emptyPath} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", xdgState)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "xdg-config"))
	t.Setenv("PATH", emptyPath)
	grant := launch.TrustRecord{SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
	legacyStore, err := launch.OpenTrustStore(filepath.Join(xdgState, "amq"), project)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyStore.Replace(grant); err != nil {
		t.Fatal(err)
	}
	migrate := func() (migrateHomePart, error) {
		t.Helper()
		stdout, _, err := captureEnvOutput(t, func() error {
			return runDoctor([]string{"--migrate-home", "--yes", "--json"})
		})
		var out struct {
			MigrateHome migrateHomeReport `json:"migrate_home"`
		}
		if jsonErr := json.Unmarshal([]byte(stdout), &out); jsonErr != nil {
			t.Fatalf("doctor --migrate-home output %q: %v (run error %v)", stdout, jsonErr, err)
		}
		return out.MigrateHome.LaunchTrust, err
	}

	part, err := migrate()
	if err != nil || part.Status != migrateHomeMoved || len(part.Steps) != 1 || !part.Steps[0].Done {
		t.Fatalf("first run: part=%+v err=%v", part, err)
	}
	stateDir, err := launch.DefaultLaunchStateDir()
	if err != nil || stateDir != filepath.Join(home, ".amq", "state") {
		t.Fatalf("trust root after the move = %q, %v", stateDir, err)
	}
	store, err := launch.OpenTrustStore(stateDir, project)
	if err != nil {
		t.Fatal(err)
	}
	if _, trusted, err := store.LoadForDigest(grant.SemanticDigest); err != nil || !trusted {
		t.Fatalf("moved grant: trusted %t, err %v", trusted, err)
	}

	// Review of #1049, on #1047: with records in both places, a move must
	// not leave the ~/.amq/state/launch records to become active when
	// <legacy>/launch goes. The command refuses and changes nothing.
	if err := legacyStore.Replace(grant); err != nil {
		t.Fatal(err)
	}
	part, err = migrate()
	if GetExitCode(err) != ExitActionRequired || len(part.Blockers) != 1 || part.Blockers[0].Code != launch.TrustMoveBothExist {
		t.Fatalf("both trees: part=%+v err=%v", part, err)
	}
	for _, path := range []string{legacyStore.Path(), store.Path()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("trust record after the refusal: %v", err)
		}
	}
}

func TestDoctorMigrateHomeMovesKeepaliveHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the keepalive home move needs flock")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "root")
	emptyPath := filepath.Join(base, "bin")
	for _, dir := range []string{home, root, emptyPath} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "xdg-state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "xdg-config"))
	t.Setenv("PATH", emptyPath)
	old := filepath.Join(home, ".amq-keepalive")
	cur := filepath.Join(home, ".amq", "keepalive")
	keepaliveBin := filepath.Join(emptyPath, "amq-keepalive")

	entry, err := registry.New(filepath.Join(old, "registry.json")).Upsert(registry.Entry{
		Root: root, Agent: "alice", Adapter: "cmux", Target: "surface:1", State: registry.StateActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	claudeConfig := filepath.Join(home, ".claude", "settings.json")
	if _, err := hookinstall.Install(hookinstall.Options{
		Agent:        hookinstall.AgentClaude,
		ScriptPath:   filepath.Join(old, "hooks", "amq-keepalive-session-start.sh"),
		BinaryPath:   keepaliveBin,
		ClaudeConfig: claudeConfig,
		Timeout:      30 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	plistPath, err := launchd.DefaultPlistPath(launchd.DefaultLabel)
	if err != nil {
		t.Fatal(err)
	}
	if err := launchd.Install(context.Background(), launchd.Options{
		BinaryPath: keepaliveBin, RegistryPath: filepath.Join(old, "registry.json"),
		AMQPath: filepath.Join(emptyPath, "amq"), Interval: 2 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	var launchctl []string
	previous := migrateHomeRun
	t.Cleanup(func() { migrateHomeRun = previous })
	migrateHomeRun = func(name string, args ...string) (string, int, error) {
		switch {
		case name == "launchctl" && args[0] == "print":
			return "Could not find service", launchctlNotFound, nil
		case name == "launchctl":
			launchctl = append(launchctl, args[0])
		case name == "systemctl":
			return "inactive", 3, nil
		}
		return "", 0, nil
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDoctor([]string{"--migrate-home", "--yes", "--json"})
	})
	var out struct {
		MigrateHome migrateHomeReport `json:"migrate_home"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &out); jsonErr != nil || err != nil || out.MigrateHome.Keepalive.Status != migrateHomeMoved {
		t.Fatalf("doctor --migrate-home: err=%v json=%v output=%s", err, jsonErr, stdout)
	}

	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("legacy home after the move: %v", err)
	}
	regPath, err := registry.DefaultPath()
	if err != nil || regPath != filepath.Join(cur, "registry.json") {
		t.Fatalf("registry path = %q, %v", regPath, err)
	}
	file, err := registry.New(regPath).Load()
	if err != nil || len(file.Entries) != 1 || file.Entries[0].ID != entry.ID {
		t.Fatalf("moved registry = %+v, %v", file.Entries, err)
	}
	installed, err := launchd.ReadInstalled(plistPath, launchd.DefaultLabel)
	if err != nil || installed.Options.RegistryPath != regPath || installed.Options.BinaryPath != keepaliveBin || installed.Options.Interval != 2*time.Minute {
		t.Fatalf("plist after the move = %+v, %v", installed.Options, err)
	}
	hooks, err := hookinstall.OwnedSessionStartHooks(claudeConfig)
	want := hookinstall.HookCommand{BinaryPath: keepaliveBin, ScriptPath: filepath.Join(cur, "hooks", "amq-keepalive-session-start.sh"), Timeout: 30 * time.Second}
	if err != nil || len(hooks) != 1 || hooks[0] != want {
		t.Fatalf("claude hooks after the move = %+v, %v", hooks, err)
	}
	if wantCalls := []string{"bootout", "bootstrap", "kickstart"}; runtime.GOOS == "darwin" && !slices.Equal(launchctl, wantCalls) {
		t.Fatalf("launchctl calls = %v, want %v", launchctl, wantCalls)
	}
}
