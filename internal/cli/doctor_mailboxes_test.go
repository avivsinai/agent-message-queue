package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	Remedy         string   `json:"remedy"`
}

func TestDoctorReportsFlagShapedLegacyMailboxWithoutRepairingIt(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "alice")
	legacy := "-legacy"
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		if err := os.MkdirAll(fsq.AgentMailboxPath(root, legacy, leaf), 0o700); err != nil {
			t.Fatalf("create legacy mailbox leaf %s: %v", leaf, err)
		}
	}

	result := runDoctorMailboxJSON(t, root)
	got := findDoctorMailboxTestEntry(t, result.Mailboxes, legacy)
	if got.Provenance != "discovered" || got.Status != "warn" ||
		got.RepairEligible || !doctorMailboxContains(got.Issues, "invalid_handle") {
		t.Fatalf("flag-shaped legacy mailbox = %#v", got)
	}
	for _, want := range []string{"preserve", "rename or remove"} {
		if !strings.Contains(got.Remedy, want) {
			t.Fatalf("legacy remedy missing %q: %q", want, got.Remedy)
		}
	}
}

func TestDoctorKeepsConfiguredFlagShapedLegacyMailboxVisibleAndUnrepairable(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	legacy := "-legacy"
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		if err := os.MkdirAll(fsq.AgentMailboxPath(root, legacy, leaf), 0o700); err != nil {
			t.Fatalf("create legacy mailbox leaf %s: %v", leaf, err)
		}
	}
	configPath := filepath.Join(root, "meta", "config.json")
	if err := os.WriteFile(configPath, []byte(`{"version":1,"agents":["-legacy"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, repair := range []bool{false, true} {
		name := "inspect"
		args := []string{"--json"}
		if repair {
			name = "repair_refusal"
			args = []string{"--fix-mailboxes", "--json"}
		}
		t.Run(name, func(t *testing.T) {
			result := runDoctorMailboxJSONArgs(t, root, args...)
			got := findDoctorMailboxTestEntry(t, result.Mailboxes, legacy)
			if got.Provenance != "configured_and_discovered" ||
				got.Status != "error" || got.RepairEligible ||
				!doctorMailboxContains(got.Issues, "invalid_handle") {
				t.Fatalf("configured flag-shaped legacy mailbox = %#v", got)
			}
			if repair && (result.MailboxRepair == nil ||
				result.MailboxRepair.Failure == nil ||
				result.MailboxRepair.Failure.Stage != "authorization") {
				t.Fatalf("repair result = %#v", result.MailboxRepair)
			}
		})
	}
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

func TestDoctorRepairIncludesReservedHumanMailboxOutsideRawRoster(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "alice")
	if err := fsq.EnsureAgentDirs(root, reservedHumanHandle); err != nil {
		t.Fatal(err)
	}
	missing := fsq.AgentDLQCur(root, reservedHumanHandle)
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	clearDoctorSessionPin(t)

	mailboxes, repair, check := inspectDoctorMailboxes(root, "", true, false)

	if check.Status != "ok" {
		t.Fatalf("mailbox check = %#v", check)
	}
	if repair == nil || repair.Status != "repaired" {
		t.Fatalf("repair = %#v", repair)
	}
	if _, err := os.Stat(missing); err != nil {
		t.Fatalf("doctor did not repair reserved human mailbox: %v; mailboxes=%#v", err, mailboxes)
	}
}

func TestDoctorMailboxRepairRemedyNeverAdvertisesPinOverride(t *testing.T) {
	target := healthyDoctorMailboxRoot(t, "bob")
	if err := os.Remove(fsq.AgentInboxCur(target, "bob")); err != nil {
		t.Fatal(err)
	}

	setDoctorIdentityPin(t, target)
	_, _, matchingCheck := inspectDoctorMailboxes(target, "", false, false)
	if strings.Contains(matchingCheck.Message, "--ignore-session-pin") {
		t.Fatalf("matching-pin remedy advertised escape hatch: %q", matchingCheck.Message)
	}
	wantMatching := doctorRootCommandForOS(target, "", runtime.GOOS, "--fix-mailboxes")
	if !strings.Contains(matchingCheck.Message, wantMatching) {
		t.Fatalf("matching-pin remedy missing exact command %q: %q", wantMatching, matchingCheck.Message)
	}

	foreignPin := healthyDoctorMailboxRoot(t, "alice")
	setDoctorIdentityPin(t, foreignPin)
	_, _, crossPinCheck := inspectDoctorMailboxes(target, "", false, false)
	wantCrossPin := doctorRootCommandForOS(target, "", runtime.GOOS, "--fix-mailboxes")
	if !strings.Contains(crossPinCheck.Message, wantCrossPin) {
		t.Fatalf("cross-pin remedy missing exact non-bypass command %q: %q", wantCrossPin, crossPinCheck.Message)
	}
	if strings.Contains(crossPinCheck.Message, "--ignore-session-pin") {
		t.Fatalf("cross-pin remedy advertised escape hatch: %q", crossPinCheck.Message)
	}
}

func TestDoctorSessionLocalConfigRemedyExecutesWithoutConfiglessBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exact printed command execution uses the POSIX test shell")
	}
	base := filepath.Join(secureTempDirForTest(t), ".agent-mail")
	root := filepath.Join(base, "collab")
	if err := fsq.EnsureRootDirs(base); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatal(err)
	}
	configureSendTestRoot(t, root, "bob")
	missing := fsq.AgentDLQCur(root, "bob")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	clearDoctorSessionPin(t)

	_, _, check := inspectDoctorMailboxes(root, "", false, false)
	command := advertisedDoctorMailboxRepairCommand(t, check)
	if strings.Contains(command, "--base-root") {
		t.Fatalf("session-local remedy named config-less parent: %q", command)
	}
	if strings.Contains(command, "--ignore-session-pin") {
		t.Fatalf("session-local remedy advertised pin escape: %q", command)
	}
	executeAdvertisedAMQCommand(t, command)
	if err := fsq.ValidateExistingMailboxLayout(openDeliveryRootForCLITest(t, root), "bob"); err != nil {
		t.Fatalf("advertised doctor remedy left mailbox incomplete: %v", err)
	}
}

func TestDoctorImplicitUserUsesEffectiveConfiguredRoster(t *testing.T) {
	for _, authority := range []string{"session_local", "base"} {
		for _, state := range []string{"absent", "incomplete"} {
			t.Run(authority+"/"+state, func(t *testing.T) {
				base := filepath.Join(secureTempDirForTest(t), ".agent-mail")
				root := filepath.Join(base, "collab")
				if err := fsq.EnsureRootDirs(base); err != nil {
					t.Fatal(err)
				}
				if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
					t.Fatal(err)
				}
				configRoot := root
				explicitBase := ""
				if authority == "base" {
					configRoot = base
					explicitBase = base
				}
				configureSendTestRoot(t, configRoot, "alice")
				if state == "incomplete" {
					if err := fsq.EnsureAgentDirs(root, reservedHumanHandle); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(fsq.AgentDLQCur(root, reservedHumanHandle)); err != nil {
						t.Fatal(err)
					}
				}
				clearDoctorSessionPin(t)

				mailboxes, repair, check := inspectDoctorMailboxes(root, explicitBase, false, false)
				if repair != nil {
					t.Fatalf("inspection returned repair: %#v", repair)
				}
				user := findDoctorMailboxInspection(t, mailboxes, reservedHumanHandle)
				wantProvenance := fsq.MailboxConfigured
				if state == "incomplete" {
					wantProvenance = fsq.MailboxConfiguredAndDiscovered
				}
				if user.Provenance != wantProvenance || !user.RepairEligible || user.Status != "error" {
					t.Fatalf("implicit user inspection = %#v, want provenance=%s repairable error", user, wantProvenance)
				}
				if strings.Contains(user.Remedy, "add") || strings.Contains(check.Message, `add "user"`) {
					t.Fatalf("implicit user was treated as unconfigured: entry=%#v check=%q", user, check.Message)
				}

				_, repaired, repairedCheck := inspectDoctorMailboxes(root, explicitBase, true, false)
				if repaired == nil || repaired.Status != "repaired" || repairedCheck.Status != "ok" {
					t.Fatalf("repair=%#v check=%#v", repaired, repairedCheck)
				}
				if err := fsq.ValidateExistingMailboxLayout(openDeliveryRootForCLITest(t, root), reservedHumanHandle); err != nil {
					t.Fatalf("doctor did not repair implicit user: %v", err)
				}
			})
		}
	}
}

func TestDoctorImplicitUserUsesPresentEmptyEffectiveRoster(t *testing.T) {
	for _, authority := range []string{"session_local", "base"} {
		for _, state := range []string{"absent", "incomplete"} {
			t.Run(authority+"/"+state, func(t *testing.T) {
				base := filepath.Join(secureTempDirForTest(t), ".agent-mail")
				root := filepath.Join(base, "collab")
				configRoot := root
				explicitBase := ""
				if authority == "base" {
					configRoot = base
					explicitBase = base
				}
				configureSendTestRoot(t, configRoot)
				if err := fsq.EnsureRootDirs(root); err != nil {
					t.Fatal(err)
				}
				if state == "incomplete" {
					if err := fsq.EnsureAgentDirs(root, reservedHumanHandle); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(fsq.AgentDLQCur(root, reservedHumanHandle)); err != nil {
						t.Fatal(err)
					}
				}
				clearDoctorSessionPin(t)

				mailboxes, repair, check := inspectDoctorMailboxes(root, explicitBase, false, false)
				if repair != nil {
					t.Fatalf("inspection returned repair: %#v", repair)
				}
				user := findDoctorMailboxInspection(t, mailboxes, reservedHumanHandle)
				wantProvenance := fsq.MailboxConfigured
				if state == "incomplete" {
					wantProvenance = fsq.MailboxConfiguredAndDiscovered
				}
				if user.Provenance != wantProvenance || !user.RepairEligible || user.Status != "error" {
					t.Fatalf("empty-roster user inspection = %#v, want provenance=%s repairable error; check=%#v", user, wantProvenance, check)
				}

				_, repaired, repairedCheck := inspectDoctorMailboxes(root, explicitBase, true, false)
				if repaired == nil || repaired.Status != "repaired" || repairedCheck.Status != "ok" {
					t.Fatalf("repair=%#v check=%#v", repaired, repairedCheck)
				}
				if err := fsq.ValidateExistingMailboxLayout(openDeliveryRootForCLITest(t, root), reservedHumanHandle); err != nil {
					t.Fatalf("doctor did not repair empty-roster user: %v", err)
				}
			})
		}
	}
}

func advertisedDoctorMailboxRepairCommand(t *testing.T, check doctorCheck) string {
	t.Helper()
	const marker = "repair: "
	parts := strings.SplitN(check.Message, marker, 2)
	if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
		t.Fatalf("doctor check has no repair command: %#v", check)
	}
	return strings.TrimSpace(strings.SplitN(parts[1], ";", 2)[0])
}

func findDoctorMailboxInspection(t *testing.T, mailboxes []fsq.MailboxInspection, handle string) fsq.MailboxInspection {
	t.Helper()
	for _, mailbox := range mailboxes {
		if mailbox.Handle == handle {
			return mailbox
		}
	}
	t.Fatalf("mailbox %q not found in %#v", handle, mailboxes)
	return fsq.MailboxInspection{}
}

func TestDoctorIssue289InspectionIgnoresMismatchedSessionPin(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "healthy")
	pinnedRoot := healthyDoctorMailboxRoot(t, "pinned")
	setDoctorIdentityPin(t, pinnedRoot)

	mailboxes, repair, check := inspectDoctorMailboxes(root, "", false, false)

	if check.Status != "ok" {
		t.Fatalf("mailbox check = %#v", check)
	}
	if repair != nil {
		t.Fatalf("inspection returned repair result: %#v", repair)
	}
	if len(mailboxes) != 2 ||
		findDoctorMailboxInspection(t, mailboxes, "healthy").Status != "ok" ||
		findDoctorMailboxInspection(t, mailboxes, reservedHumanHandle).Status != "ok" {
		t.Fatalf("mailboxes = %#v", mailboxes)
	}
}

func TestDoctorIssue289InspectionWithoutSessionPinFailsOpen(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "healthy")
	clearDoctorSessionPin(t)

	mailboxes, repair, check := inspectDoctorMailboxes(root, "", false, false)

	if check.Status != "ok" {
		t.Fatalf("mailbox check = %#v", check)
	}
	if repair != nil {
		t.Fatalf("inspection returned repair result: %#v", repair)
	}
	if len(mailboxes) != 2 ||
		findDoctorMailboxInspection(t, mailboxes, "healthy").Status != "ok" ||
		findDoctorMailboxInspection(t, mailboxes, reservedHumanHandle).Status != "ok" {
		t.Fatalf("mailboxes = %#v", mailboxes)
	}
}

func TestDoctorIssue289RunDoctorInspectsOutsidePopulatedSessionPin(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "healthy")
	pinnedRoot := healthyDoctorMailboxRoot(t, "pinned")
	setDoctorIdentityPin(t, pinnedRoot)

	result := runDoctorMailboxJSON(t, root)

	if got := doctorCheckStatus(result.Checks, "Mailboxes"); got != "ok" {
		t.Fatalf("Mailboxes status = %q, checks=%#v", got, result.Checks)
	}
	if got := doctorCheckStatus(result.Checks, "Session identity pin"); got != "warn" {
		t.Fatalf("Session identity pin status = %q, want warn", got)
	}
	if len(result.Mailboxes) != 2 ||
		findDoctorMailboxTestEntry(t, result.Mailboxes, "healthy").Status != "ok" ||
		findDoctorMailboxTestEntry(t, result.Mailboxes, reservedHumanHandle).Status != "ok" {
		t.Fatalf("mailboxes = %#v", result.Mailboxes)
	}
}

func TestDoctorWarnsOnContradictoryLegacyPinAndStillInspects(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "healthy")
	pinnedBase := secureTempDirForTest(t)
	t.Setenv(envBaseRoot, pinnedBase)
	t.Setenv(envSession, "current")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	result := runDoctorMailboxJSON(t, root)

	pinCheck := findDoctorCheck(t, result.Checks, "Session identity pin")
	if pinCheck.Status != "warn" || !strings.Contains(pinCheck.Message, "differs from pinned root") {
		t.Fatalf("legacy pin check = %#v, want mismatch warning", pinCheck)
	}
	if got := doctorCheckStatus(result.Checks, "Mailboxes"); got != "ok" {
		t.Fatalf("Mailboxes status = %q, checks=%#v", got, result.Checks)
	}
	if len(result.Mailboxes) != 2 ||
		findDoctorMailboxTestEntry(t, result.Mailboxes, "healthy").Status != "ok" ||
		findDoctorMailboxTestEntry(t, result.Mailboxes, reservedHumanHandle).Status != "ok" {
		t.Fatalf("doctor did not complete read-only inspection: %#v", result.Mailboxes)
	}
}

func TestDoctorOwnBaseMailboxRepairRefusesContradictoryLegacyPin(t *testing.T) {
	baseRoot := healthyDoctorMailboxRoot(t, "healthy")
	if err := fsq.EnsureAgentDirs(filepath.Join(baseRoot, "current"), "healthy"); err != nil {
		t.Fatal(err)
	}
	missing := fsq.AgentInboxCur(baseRoot, "healthy")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "current")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	result := runDoctorMailboxJSONArgs(t, baseRoot,
		"--root", baseRoot,
		"--fix-mailboxes",
		"--json",
	)

	check := findDoctorCheck(t, result.Checks, "Mailboxes")
	if result.MailboxRepair != nil || check.Status != "error" ||
		!strings.Contains(check.Message, "pinned session context") {
		t.Fatalf("contradictory legacy pin did not refuse own-base repair: repair=%#v check=%#v", result.MailboxRepair, check)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("refused own-base repair mutated mailbox: %v", err)
	}
}

func TestDoctorIssue289RepairRefusesMismatchedSessionPinWithRemedy(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "healthy")
	pinnedRoot := healthyDoctorMailboxRoot(t, "pinned")
	setDoctorIdentityPin(t, pinnedRoot)
	if err := os.Remove(fsq.AgentInboxCur(root, "healthy")); err != nil {
		t.Fatal(err)
	}

	_, repair, check := inspectDoctorMailboxes(root, "", true, false)

	if repair != nil {
		t.Fatalf("refused repair returned result: %#v", repair)
	}
	for _, want := range []string{"refusing to repair", "pinned session context", "re-run from the intended session"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("repair error missing %q: %#v", want, check)
		}
	}
	if _, err := os.Stat(fsq.AgentInboxCur(root, "healthy")); !os.IsNotExist(err) {
		t.Fatalf("refused repair mutated missing directory: %v", err)
	}
}

func TestDoctorExplicitRootRepairRequiresPinMatchOrOverride(t *testing.T) {
	baseRoot := filepath.Join(secureTempDirForTest(t), "custom base")
	sessionRoot := filepath.Join(baseRoot, "collab")
	if err := fsq.EnsureAgentDirs(sessionRoot, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureRootDirs(baseRoot); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"bob"},
	}, true); err != nil {
		t.Fatal(err)
	}
	missing := fsq.AgentDLQCur(sessionRoot, "bob")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	unrelated := healthyDoctorMailboxRoot(t, "source")
	setDoctorIdentityPin(t, unrelated)
	t.Setenv(envRoot, unrelated)
	t.Setenv(envSession, "source-session")

	inspectionOutput, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{
			"--root", sessionRoot,
			"--base-root", baseRoot,
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("explicit doctor inspection: %v", err)
	}
	var inspection doctorMailboxResultJSON
	if err := json.Unmarshal([]byte(inspectionOutput), &inspection); err != nil {
		t.Fatal(err)
	}
	if got := doctorCheckStatus(inspection.Checks, "Session identity pin"); got != "warn" {
		t.Fatalf("explicit inspection pin status = %q, want warn", got)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("explicit inspection mutated mailbox: %v", err)
	}

	refusedOutput, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{
			"--root", sessionRoot,
			"--base-root", baseRoot,
			"--fix-mailboxes",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("mismatched repair command: %v", err)
	}
	var refused doctorMailboxResultJSON
	if err := json.Unmarshal([]byte(refusedOutput), &refused); err != nil {
		t.Fatal(err)
	}
	refusedCheck := findDoctorCheck(t, refused.Checks, "Mailboxes")
	if refused.MailboxRepair != nil || refusedCheck.Status != "error" ||
		!strings.Contains(refusedCheck.Message, "pinned session context") {
		t.Fatalf("mismatched repair was not refused: repair=%#v check=%#v", refused.MailboxRepair, refusedCheck)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("mismatched explicit repair mutated mailbox: %v", err)
	}

	overrideOutput, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{
			"--root", sessionRoot,
			"--base-root", baseRoot,
			"--ignore-session-pin",
			"--fix-mailboxes",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("explicit override repair: %v", err)
	}
	var repaired doctorMailboxResultJSON
	if err := json.Unmarshal([]byte(overrideOutput), &repaired); err != nil {
		t.Fatal(err)
	}
	if repaired.MailboxRepair == nil || repaired.MailboxRepair.Status != "repaired" {
		t.Fatalf("override repair = %#v checks=%#v", repaired.MailboxRepair, repaired.Checks)
	}
	if err := fsq.ValidateExistingMailboxLayout(openDeliveryRootForCLITest(t, sessionRoot), "bob"); err != nil {
		t.Fatalf("override doctor repair left mailbox incomplete: %v", err)
	}
}

func TestDoctorExplicitBaseRequiresRootAndDirectRelationship(t *testing.T) {
	baseRoot := healthyDoctorMailboxRoot(t, "bob")
	if err := runDoctor([]string{"--base-root", baseRoot, "--fix-mailboxes"}); err == nil ||
		!strings.Contains(err.Error(), "--base-root requires an explicit --root") {
		t.Fatalf("base without root error = %v", err)
	}
	if err := runDoctor([]string{"--ignore-session-pin", "--fix-mailboxes"}); err == nil ||
		!strings.Contains(err.Error(), "--ignore-session-pin requires an explicit --root") {
		t.Fatalf("pin override without root error = %v", err)
	}
	if err := runDoctor([]string{"--root=", "--ignore-session-pin", "--fix-mailboxes"}); err == nil ||
		!strings.Contains(err.Error(), "--root cannot be empty") {
		t.Fatalf("pin override with blank root error = %v", err)
	}

	outsideRoot := healthyDoctorMailboxRoot(t, "bob")
	missing := fsq.AgentInboxCur(outsideRoot, "bob")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	err := runDoctor([]string{
		"--root", outsideRoot,
		"--base-root", baseRoot,
		"--fix-mailboxes",
		"--json",
	})
	if err == nil || !strings.Contains(err.Error(), "one direct child") {
		t.Fatalf("outside base error = %v", err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("outside-base refusal mutated mailbox: %v", err)
	}
}

func TestDoctorExplicitPinnedBaseRepairIsAuthorized(t *testing.T) {
	baseRoot := filepath.Join(secureTempDirForTest(t), ".agent-mail")
	sessionRoot := filepath.Join(baseRoot, "collab")
	configureSendTestRoot(t, baseRoot, "alice", "bob")
	if err := fsq.EnsureAgentDirs(sessionRoot, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(baseRoot, "agents", "bob")); err != nil {
		t.Fatal(err)
	}
	pinSendSessionForTest(t, baseRoot, sessionRoot, "collab")

	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{
			"--root", baseRoot,
			"--fix-mailboxes",
			"--json",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result doctorMailboxResultJSON
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.MailboxRepair == nil || result.MailboxRepair.Status != "repaired" {
		t.Fatalf("pinned base repair = %#v checks=%#v", result.MailboxRepair, result.Checks)
	}
	if err := fsq.ValidateExistingMailboxLayout(openDeliveryRootForCLITest(t, baseRoot), "bob"); err != nil {
		t.Fatalf("pinned base repair left mailbox incomplete: %v", err)
	}
}

func TestDoctorOpsUsesExplicitBaseConfigAuthority(t *testing.T) {
	baseRoot := filepath.Join(secureTempDirForTest(t), "custom base")
	sessionRoot := filepath.Join(baseRoot, "collab")
	configureSendTestRoot(t, baseRoot, "bob")
	if err := fsq.EnsureAgentDirs(sessionRoot, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(sessionRoot, reservedHumanHandle); err != nil {
		t.Fatal(err)
	}
	clearDoctorSessionPin(t)

	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{
			"--root", sessionRoot,
			"--base-root", baseRoot,
			"--ops",
			"--json",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result doctorMailboxResultJSON
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Ops == nil || len(result.Ops.Agents) != 1 || result.Ops.Agents[0].Handle != "bob" {
		t.Fatalf("explicit-base ops = %#v", result.Ops)
	}
	mailbox := findDoctorMailboxTestEntry(t, result.Mailboxes, "bob")
	if mailbox.Provenance != "configured_and_discovered" || mailbox.Status != "ok" ||
		len(mailbox.Issues) != 0 || mailbox.Remedy != "" {
		t.Fatalf("explicit-base mailbox classification = %#v", mailbox)
	}
	if check := findDoctorCheck(t, result.Checks, "Mailboxes"); check.Status != "ok" {
		t.Fatalf("explicit-base mailbox check = %#v", check)
	}
	for _, hint := range result.Ops.Hints {
		if hint.Code == "config_error" {
			t.Fatalf("explicit-base ops emitted config error: %#v", hint)
		}
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
	for _, want := range []string{"meta/config.json", "amq doctor --root", "--fix-mailboxes", "agents/orphan", "preserve"} {
		if !strings.Contains(orphan.Remedy, want) {
			t.Fatalf("orphan remedy missing %q: %q", want, orphan.Remedy)
		}
	}
	if strings.Contains(orphan.Remedy, "--ignore-session-pin") {
		t.Fatalf("unpinned orphan remedy advertised escape hatch: %q", orphan.Remedy)
	}
}

func TestDoctorDiscoveredOnlyMailboxKeepsRemedyAfterConfiguredRepair(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "claude")
	if err := os.Remove(fsq.AgentInboxCur(root, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "agents", "orphan"), 0o700); err != nil {
		t.Fatal(err)
	}

	result := runDoctorMailboxJSONArgs(t, root, "--fix-mailboxes", "--json")

	if result.MailboxRepair == nil ||
		!doctorMailboxContains(result.MailboxRepair.CreatedPaths, "agents/claude/inbox/cur") {
		t.Fatalf("repair = %#v", result.MailboxRepair)
	}
	if doctorMailboxContains(result.MailboxRepair.CreatedPaths, "agents/orphan/inbox") {
		t.Fatalf("repair completed discovered-only orphan: %#v", result.MailboxRepair.CreatedPaths)
	}
	orphan := findDoctorMailboxTestEntry(t, result.Mailboxes, "orphan")
	if orphan.Status != "warn" || orphan.RepairEligible || orphan.Remedy == "" {
		t.Fatalf("orphan = %#v", orphan)
	}
	check := findDoctorCheck(t, result.Checks, "Mailboxes")
	for _, want := range []string{"orphan:", "next:", "meta/config.json"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("mailbox check missing %q: %q", want, check.Message)
		}
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
	if !doctorMailboxContains(first.MailboxRepair.CreatedPaths, "agents/legacy/inbox/cur") ||
		!doctorMailboxContains(first.MailboxRepair.CreatedPaths, "agents/user/receipts") {
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

func healthyDoctorMailboxRoot(t *testing.T, handle string) string {
	t.Helper()
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, handle); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, reservedHumanHandle); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{handle},
	}, true); err != nil {
		t.Fatal(err)
	}
	return root
}

func setDoctorIdentityPin(t *testing.T, root string) {
	t.Helper()
	token, err := resolveTreeIdentityToken(root)
	if err != nil {
		t.Fatalf("resolveTreeIdentityToken(%s): %v", root, err)
	}
	t.Setenv(envBaseRoot, root)
	t.Setenv(envSession, "")
	t.Setenv(envRootID, token)
	t.Setenv(envBaseRootID, token)
}

func clearDoctorSessionPin(t *testing.T) {
	t.Helper()
	for _, key := range []string{envRootID, envBaseRoot, envBaseRootID, envSession} {
		setOptionalEnv(t, key, "", false)
	}
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

func findDoctorCheck(t *testing.T, checks []doctorCheck, name string) doctorCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor check %q missing: %#v", name, checks)
	return doctorCheck{}
}
