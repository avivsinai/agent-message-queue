package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMailboxLayoutRepairReportsExactPartialProgress(t *testing.T) {
	rootPath := mailboxLayoutTestRoot(t, "legacy")
	if err := EnsureAgentDirs(rootPath, "legacy"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		AgentInboxCur(rootPath, "legacy"),
		AgentDLQCur(rootPath, "legacy"),
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	root := openMailboxLayoutTestRoot(t, rootPath)
	injected := errors.New("injected mkdir failure")

	result := repairMailboxLayout(root, mailboxRepairHooks{
		fail: func(stage, path string) error {
			if stage == "mkdir" && filepath.ToSlash(path) == "agents/legacy/dlq/cur" {
				return injected
			}
			return nil
		},
	})

	if result.Status != "partial" || result.Failure == nil {
		t.Fatalf("result = %#v", result)
	}
	if result.Failure.Code != "create_failed" ||
		result.Failure.Path != "agents/legacy/dlq/cur" ||
		result.Failure.Message != injected.Error() {
		t.Fatalf("failure = %#v", result.Failure)
	}
	if len(result.CreatedPaths) != 1 ||
		result.CreatedPaths[0] != "agents/legacy/inbox/cur" {
		t.Fatalf("created_paths = %#v", result.CreatedPaths)
	}
	if info, err := os.Stat(AgentInboxCur(rootPath, "legacy")); err != nil || !info.IsDir() {
		t.Fatalf("created directory missing: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(AgentDLQCur(rootPath, "legacy")); !os.IsNotExist(err) {
		t.Fatalf("failed directory unexpectedly exists: %v", err)
	}
}

func TestMailboxLayoutRepairRefusesConfigReplacementBeforeFirstMkdir(t *testing.T) {
	rootPath := mailboxLayoutTestRoot(t, "legacy")
	if err := EnsureAgentDirs(rootPath, "legacy"); err != nil {
		t.Fatal(err)
	}
	missing := AgentDLQCur(rootPath, "legacy")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	root := openMailboxLayoutTestRoot(t, rootPath)
	configPath := filepath.Join(rootPath, "meta", "config.json")
	originalPath := configPath + ".original"

	result := repairMailboxLayout(root, mailboxRepairHooks{
		afterPreflight: func() {
			if err := os.Rename(configPath, originalPath); err != nil {
				t.Fatalf("rename config: %v", err)
			}
			if err := os.WriteFile(configPath, []byte(`{"version":1,"agents":["attacker"]}`), 0o600); err != nil {
				t.Fatalf("replace config: %v", err)
			}
		},
	})
	if result.Status == "repaired" {
		t.Fatalf("repair accepted replaced authorization config: %#v", result)
	}
	if len(result.CreatedPaths) != 0 {
		t.Fatalf("repair mutated before rejecting config replacement: %#v", result.CreatedPaths)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("repair created path after config replacement: %v", err)
	}
}

func mailboxLayoutTestRoot(t *testing.T, agents ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	configJSON := `{"version":1,"agents":[]}`
	if len(agents) == 1 {
		configJSON = `{"version":1,"agents":["` + agents[0] + `"]}`
	}
	if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func openMailboxLayoutTestRoot(t *testing.T, rootPath string) *DeliveryRoot {
	t.Helper()
	identity, err := SnapshotDeliveryRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close delivery root: %v", err)
		}
	})
	return root
}

func TestRepairMailboxLayoutAuthorizationVerifyAndRepairHappyPath(t *testing.T) {
	rootPath := mailboxLayoutTestRoot(t, "legacy")
	if err := EnsureAgentDirs(rootPath, "legacy"); err != nil {
		t.Fatal(err)
	}
	missing := AgentDLQCur(rootPath, "legacy")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	root := openMailboxLayoutTestRoot(t, rootPath)
	authorization, _, err := OpenMailboxConfigAuthorization(root)
	if err != nil {
		t.Fatalf("OpenMailboxConfigAuthorization: %v", err)
	}
	defer func() { _ = authorization.Close() }()
	if err := authorization.Verify(); err != nil {
		t.Fatalf("authorization.Verify on retained config: %v", err)
	}

	result := RepairMailboxLayoutForAgentsWithAuthorization(root, authorization, []string{"legacy"})
	if result.Status != "repaired" {
		t.Fatalf("repair result = %#v, want repaired", result)
	}
	if info, err := os.Stat(missing); err != nil || !info.IsDir() {
		t.Fatalf("repaired dlq/cur missing: info=%v err=%v", info, err)
	}

	// Swapping the config after authorization must fail closed on Verify.
	configPath := filepath.Join(rootPath, "meta", "config.json")
	if err := os.WriteFile(configPath, []byte(`{"version":1,"agents":["attacker"]}`), 0o600); err != nil {
		t.Fatalf("replace config: %v", err)
	}
	if err := authorization.Verify(); err == nil {
		t.Fatal("authorization.Verify accepted a replaced config")
	}
}

func TestRepairMailboxLayoutForAgentsRepairsRequestedHandle(t *testing.T) {
	rootPath := mailboxLayoutTestRoot(t, "legacy")
	if err := EnsureAgentDirs(rootPath, "legacy"); err != nil {
		t.Fatal(err)
	}
	missing := AgentDLQCur(rootPath, "cursor")
	root := openMailboxLayoutTestRoot(t, rootPath)

	result := RepairMailboxLayoutForAgents(root, []string{"cursor"})
	if result.Status != "repaired" {
		t.Fatalf("repair result = %#v, want repaired for handle outside config roster", result)
	}
	if info, err := os.Stat(missing); err != nil || !info.IsDir() {
		t.Fatalf("requested mailbox not repaired: info=%v err=%v", info, err)
	}
}

func TestRepairMailboxLayoutRepairsFullConfiguredRoster(t *testing.T) {
	rootPath := mailboxLayoutTestRoot(t, "legacy")
	if err := EnsureAgentDirs(rootPath, "legacy"); err != nil {
		t.Fatal(err)
	}
	missing := AgentDLQCur(rootPath, "legacy")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	root := openMailboxLayoutTestRoot(t, rootPath)

	result := RepairMailboxLayout(root)
	if result.Status != "repaired" {
		t.Fatalf("repair result = %#v, want repaired", result)
	}
	if info, err := os.Stat(missing); err != nil || !info.IsDir() {
		t.Fatalf("configured mailbox not repaired: info=%v err=%v", info, err)
	}
}
