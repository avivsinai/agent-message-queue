package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type doctorMailboxJSON struct {
	Handle         string   `json:"handle"`
	Provenance     string   `json:"provenance"`
	Status         string   `json:"status"`
	Issues         []string `json:"issues"`
	RepairEligible bool     `json:"repair_eligible"`
}

func TestDoctorIssue289ConfiguredOnlyMailboxIsReported(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"missing"},
	}, true); err != nil {
		t.Fatal(err)
	}

	result := runDoctorMailboxJSON(t, root)
	missing := findDoctorMailboxTestEntry(t, result.Mailboxes, "missing")
	if missing.Provenance != "configured" || missing.Status != "error" || !missing.RepairEligible {
		t.Fatalf("configured-only mailbox = %#v", missing)
	}
	if !doctorMailboxContains(missing.Issues, "missing:.") {
		t.Fatalf("issues = %#v, want missing mailbox root", missing.Issues)
	}
}

func TestDoctorIssue289DiscoveredOnlyMailboxIsNotRepairEligible(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"healthy"},
	}, true); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "healthy"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "agents", "orphan"), 0o700); err != nil {
		t.Fatal(err)
	}

	result := runDoctorMailboxJSON(t, root)
	orphan := findDoctorMailboxTestEntry(t, result.Mailboxes, "orphan")
	if orphan.Provenance != "discovered" || orphan.Status != "warn" || orphan.RepairEligible {
		t.Fatalf("discovered-only mailbox = %#v", orphan)
	}
}

func TestDoctorIssue289RepairCreatesOnlyMissingDirsAndIsIdempotent(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"legacy"},
	}, true); err != nil {
		t.Fatal(err)
	}
	messagePath := filepath.Join(fsq.AgentInboxNew(root, "legacy"), "sentinel.md")
	if err := os.WriteFile(messagePath, []byte("do not touch"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(messagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fsq.AgentInboxCur(root, "legacy")); err != nil {
		t.Fatal(err)
	}

	first := runDoctorMailboxJSONArgs(t, root, "--fix-mailboxes", "--json")
	if first.MailboxRepair == nil || first.MailboxRepair.Status != "repaired" {
		var failure *fsq.MailboxRepairFailure
		if first.MailboxRepair != nil {
			failure = first.MailboxRepair.Failure
		}
		t.Fatalf("repair = %#v failure=%#v", first.MailboxRepair, failure)
	}
	if len(first.MailboxRepair.CreatedPaths) != 1 ||
		first.MailboxRepair.CreatedPaths[0] != "agents/legacy/inbox/cur" {
		t.Fatalf("created_paths = %#v", first.MailboxRepair.CreatedPaths)
	}
	after, err := os.Stat(messagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("existing message identity or mode changed: before=%v after=%v", before, after)
	}
	data, err := os.ReadFile(messagePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "do not touch" {
		t.Fatalf("existing message changed: %q", data)
	}

	second := runDoctorMailboxJSONArgs(t, root, "--fix-mailboxes", "--json")
	if second.MailboxRepair == nil || len(second.MailboxRepair.CreatedPaths) != 0 {
		t.Fatalf("idempotent repair = %#v", second.MailboxRepair)
	}
}

func TestDoctorIssue289RepairRefusesSymlinkWithoutMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform privileges")
	}
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"legacy"},
	}, true); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fsq.AgentInboxCur(root, "legacy")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, fsq.AgentInboxCur(root, "legacy")); err != nil {
		t.Fatal(err)
	}

	result := runDoctorMailboxJSONArgs(t, root, "--fix-mailboxes", "--json")
	if result.MailboxRepair == nil || result.MailboxRepair.Failure == nil ||
		result.MailboxRepair.Failure.Code != "preflight_failed" {
		t.Fatalf("repair = %#v", result.MailboxRepair)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside sentinel changed: data=%q err=%v", data, err)
	}
	info, err := os.Lstat(fsq.AgentInboxCur(root, "legacy"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was changed: info=%v err=%v", info, err)
	}
}

type doctorMailboxResultJSON struct {
	Checks        []doctorCheck            `json:"checks"`
	Mailboxes     []doctorMailboxJSON      `json:"mailboxes"`
	MailboxRepair *fsq.MailboxRepairResult `json:"mailbox_repair"`
	Ops           *doctorOpsResult         `json:"ops"`
}

func runDoctorMailboxJSON(t *testing.T, root string) doctorMailboxResultJSON {
	t.Helper()
	return runDoctorMailboxJSONArgs(t, root, "--json")
}

func runDoctorMailboxJSONArgs(t *testing.T, root string, args ...string) doctorMailboxResultJSON {
	t.Helper()
	t.Setenv(envRoot, root)
	output, err := captureEnvStdout(t, func() error {
		return runDoctor(args)
	})
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	var result doctorMailboxResultJSON
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v\n%s", err, output)
	}
	return result
}

func TestDoctorIssue289ConfiguredDiscoveredLegacyMailbox(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{"healthy", "legacy"} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"healthy", "legacy"},
	}, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, "legacy"), "unread.md"), []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fsq.AgentInboxCur(root, "legacy")); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envRoot, root)
	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--ops", "--json"})
	})
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}

	var result struct {
		Checks    []doctorCheck       `json:"checks"`
		Mailboxes []doctorMailboxJSON `json:"mailboxes"`
		Ops       *doctorOpsResult    `json:"ops"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v\n%s", err, output)
	}

	legacy := findDoctorMailboxTestEntry(t, result.Mailboxes, "legacy")
	if legacy.Provenance != "configured_and_discovered" {
		t.Fatalf("legacy provenance = %q", legacy.Provenance)
	}
	if legacy.Status != "error" {
		t.Fatalf("legacy status = %q, want error", legacy.Status)
	}
	if !legacy.RepairEligible {
		t.Fatal("legacy should be repair eligible")
	}
	if !doctorMailboxContains(legacy.Issues, "missing:inbox/cur") {
		t.Fatalf("legacy issues = %#v", legacy.Issues)
	}
	if got := doctorCheckStatus(result.Checks, "Mailboxes"); got != "error" {
		t.Fatalf("Mailboxes status = %q, want error", got)
	}
	if result.Ops == nil {
		t.Fatal("ops result missing")
	}
	for _, agent := range result.Ops.Agents {
		if agent.Handle == "legacy" {
			if agent.UnreadCount != 1 {
				t.Fatalf("legacy unread_count = %d, want 1", agent.UnreadCount)
			}
			return
		}
	}
	t.Fatal("legacy missing from ops")
}

func findDoctorMailboxTestEntry(t *testing.T, entries []doctorMailboxJSON, handle string) doctorMailboxJSON {
	t.Helper()
	for _, entry := range entries {
		if entry.Handle == handle {
			return entry
		}
	}
	t.Fatalf("mailbox %q missing: %#v", handle, entries)
	return doctorMailboxJSON{}
}

func doctorMailboxContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func doctorCheckStatus(checks []doctorCheck, name string) string {
	for _, check := range checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}
