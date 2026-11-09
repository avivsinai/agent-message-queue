package launch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestExecutionTrustDigestBindsSessionAndPhysicalRoot(t *testing.T) {
	_, firstRoot := openTestRoot(t)
	_, secondRoot := openTestRoot(t)
	plan := validPlan()
	first, err := ExecutionTrustDigestWithAuthority(plan, "collab", firstRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := ExecutionTrustDigestWithAuthority(plan, "empty", firstRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	otherRoot, err := ExecutionTrustDigestWithAuthority(plan, "collab", secondRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if first == otherSession {
		t.Fatal("session change retained stale execution trust digest")
	}
	if first == otherRoot {
		t.Fatal("physical session-root change retained stale execution trust digest")
	}
}

func TestPrepareTrustDigestMatchesExistingTrustSubjectForPresentRoot(t *testing.T) {
	_, root := openTestRoot(t)
	plan := validPlan()
	planDigest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	rootIdentity, err := fsq.StableTreeIdentityInfo(root.FileInfo())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareTrustDigestWithAuthority(planDigest, "collab", root.Base(), rootIdentity, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := ExecutionTrustDigestWithAuthority(plan, "collab", root, "")
	if err != nil {
		t.Fatal(err)
	}
	if prepared != existing {
		t.Fatalf("Prepare trust digest %q differs from existing execution trust digest %q", prepared, existing)
	}
}

// Field regression (2026-11-09): a macOS update reboot renumbers st_dev of
// the project, base and session directories, and every trust digest that
// hashed those identities changed, so launch asked for trust again.
func TestPrepareTrustDigestSurvivesReboot(t *testing.T) {
	for _, session := range []string{"absent", "existing"} {
		t.Run(session, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			project, configuredRoot := filepath.Join(root, "project"), filepath.Join(root, "configured")
			sessionRoot := filepath.Join(configuredRoot, "profile-a", "collab")
			dirs := []string{project, configuredRoot}
			if session == "existing" {
				dirs = append(dirs, filepath.Dir(sessionRoot), sessionRoot)
			}
			for _, dir := range dirs {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(project, ".amqrc"), []byte(`{"root":"`+configuredRoot+`"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			request := PrepareRequest{
				Target:   PrepareTarget{ProjectRoot: project, BaseRoot: filepath.Dir(sessionRoot), SessionRoot: sessionRoot, Session: "collab"},
				Launcher: LauncherCommands, IntentDigest: testDigest('e'),
				Participants: []PrepareParticipant{{Handle: "operator", Runnable: false}},
			}
			dependencies := PrepareDependencies{Backends: map[string]Backend{LauncherCommands: &prepareTestBackend{}},
				AdapterFor:   func(provider, _ string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
				HostIdentity: "host:test"}
			before, err := Prepare(context.Background(), request, dependencies)
			if err != nil {
				t.Fatal(err)
			}
			rebootDevices(t)
			after, err := Prepare(context.Background(), request, dependencies)
			if err != nil {
				t.Fatal(err)
			}
			if after.TrustDigest != before.TrustDigest {
				t.Fatal("trust digest changed across a reboot")
			}
		})
	}
}
