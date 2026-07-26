package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMailboxLayoutRepairReportsConcurrentAppearanceWithoutMutation(t *testing.T) {
	rootPath := mailboxLayoutTestRoot(t, "legacy")
	root := openMailboxLayoutTestRoot(t, rootPath)

	result := repairMailboxLayout(root, mailboxRepairHooks{
		afterPreflight: func() {
			if err := os.Mkdir(filepath.Join(rootPath, "agents", "legacy"), 0o700); err != nil {
				t.Fatalf("create concurrent mailbox: %v", err)
			}
		},
	})

	if result.Status != "partial" || result.Failure == nil {
		t.Fatalf("result = %#v", result)
	}
	if result.Failure.Code != "concurrent_race" ||
		result.Failure.Path != "agents/legacy" ||
		len(result.CreatedPaths) != 0 {
		t.Fatalf("result = %#v failure=%#v", result, result.Failure)
	}
	entries, err := os.ReadDir(filepath.Join(rootPath, "agents", "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("concurrent mailbox mutated: %#v", entries)
	}
}

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

func TestMailboxLayoutRepairRefusesNonDirectoryPreflight(t *testing.T) {
	rootPath := mailboxLayoutTestRoot(t, "legacy")
	if err := EnsureAgentDirs(rootPath, "legacy"); err != nil {
		t.Fatal(err)
	}
	cur := AgentInboxCur(rootPath, "legacy")
	if err := os.Remove(cur); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cur, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openMailboxLayoutTestRoot(t, rootPath)

	result := RepairMailboxLayout(root)

	if result.Status != "failed" || result.Failure == nil ||
		result.Failure.Code != "preflight_failed" {
		t.Fatalf("result = %#v failure=%#v", result, result.Failure)
	}
	data, err := os.ReadFile(cur)
	if err != nil || string(data) != "not a directory" {
		t.Fatalf("non-directory changed: data=%q err=%v", data, err)
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
