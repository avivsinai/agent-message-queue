package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	migrate := func() (migrateHomeTrustPart, error) {
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
	if err != nil || part.Status != migrateHomeMoved || part.Projects != 1 {
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
	if GetExitCode(err) != ExitActionRequired || part.Refusal != launch.TrustMoveBothExist {
		t.Fatalf("both trees: part=%+v err=%v", part, err)
	}
	for _, path := range []string{legacyStore.Path(), store.Path()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("trust record after the refusal: %v", err)
		}
	}
}
