package cli

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestCleanupLaunchJournalReportsExactTargetAndRequiresExplicitRoot(t *testing.T) {
	rootPath := t.TempDir()
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	nonce := "019c8a2f-2b13-7000-8000-000000000020"
	plan := launch.Plan{Version: launch.PlanVersion, Agents: []launch.AgentPlan{{
		Handle: "claude", Argv: []string{"/usr/bin/true", nonce}, Cwd: t.TempDir(),
		AdapterMode: launch.AdapterModeMint, ResumePolicy: launch.ResumeEnabled,
		LaunchNonce: nonce, ConversationID: nonce,
		DynamicArgv: []launch.DynamicArg{{Index: 1, Kind: launch.DynamicArgLaunchNonce}},
	}}}
	digest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	config := launch.ProjectConfig{
		Schema: launch.ProjectConfigSchema, DefaultSession: "collab", Layout: launch.LayoutIntent{Type: launch.LayoutColumns},
		Agents: []launch.ProjectAgentConfig{{Handle: "claude", Adapter: "claude", Command: []string{"claude"}, ResumePolicy: launch.ResumeEnabled}},
	}
	profile := launch.Profile{Backend: "test", Platform: "test", VersionRange: "*", Version: 1, Capabilities: []launch.Capability{launch.CapCreate}}
	record, err := launch.NewLaunchJournal(
		launch.ReconcileRequest{ProjectRoot: project, Session: "collab", Root: root, Config: config},
		"test", launch.DetectResult{Available: true, Profile: profile, HostIdentity: "host:test", InstanceIdentity: "instance:test"},
		plan, digest, nonce,
		[]launch.AgentReconcileResult{{Handle: "claude", ConversationDisposition: launch.DispositionFresh}},
		[]launch.ConversationRecord{{Version: launch.ConversationVersion, Handle: "claude", State: launch.CapturePending, ProviderVersion: "test", LaunchNonce: nonce}},
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := launch.AcquireLease(root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := launch.WriteJournal(root, lease, record); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	if err := runCleanup([]string{"--launch-journal", "--dry-run"}); err == nil {
		t.Fatalf("cleanup without explicit root error = %v", err)
	}
	stdout, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--launch-journal", "--root", rootPath, "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var dry struct {
		Removed bool `json:"removed"`
		Journal struct {
			Path        string `json:"path"`
			Backend     string `json:"backend"`
			LaunchNonce string `json:"launch_nonce"`
		} `json:"launch_journal"`
	}
	if err := json.Unmarshal([]byte(stdout), &dry); err != nil {
		t.Fatal(err)
	}
	if dry.Removed || dry.Journal.Path != launch.JournalPath(rootPath) || dry.Journal.Backend != "test" || dry.Journal.LaunchNonce != nonce {
		t.Fatalf("dry-run report=%#v", dry)
	}
	if _, err := launch.LoadJournal(root); err != nil {
		t.Fatalf("dry-run removed journal: %v", err)
	}

	stdout, _, err = captureEnvOutput(t, func() error {
		return runCleanup([]string{"--launch-journal", "--root", rootPath, "--yes", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(stdout), &dry); err != nil || !dry.Removed {
		t.Fatalf("cleanup report=%s err=%v", stdout, err)
	}
	if _, err := launch.LoadJournal(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal after cleanup: %v", err)
	}
}
