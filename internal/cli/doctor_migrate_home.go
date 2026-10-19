package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

// migrateHomeVersionFloor is the first release that keeps launch trust in
// ~/.amq/state on a fresh machine (#1049) and the keepalive home in
// ~/.amq/keepalive (#1052). An older binary uses only the legacy locations,
// so it would write <legacy>/launch again after the move.
const migrateHomeVersionFloor = "0.97.0"

const (
	migrateHomeNothingToDo = "nothing_to_do"
	migrateHomeWouldMove   = "would_move"
	migrateHomeMoved       = "moved"
	migrateHomeRefused     = "refused"

	migrateHomeBinaryTooOld      = "binary_too_old"
	migrateHomeVersionUnreadable = "version_unreadable"
)

type migrateHomeReport struct {
	DryRun      bool            `json:"dry_run"`
	LaunchTrust migrateHomePart `json:"launch_trust"`
}

// migrateHomePart is one home's migration. Every list is present, empty when
// it has no entries.
type migrateHomePart struct {
	Status       string                 `json:"status"`
	From         string                 `json:"from"`
	To           string                 `json:"to"`
	Gate         []migrateHomeGateEntry `json:"gate"`
	Blockers     []migrateHomeBlocker   `json:"blockers"`
	Steps        []migrateHomeStep      `json:"steps"`
	OperatorNext []string               `json:"operator_next"`
	NotChecked   []string               `json:"not_checked"`
	fromProjects int
}

type migrateHomeGateEntry struct {
	Source  string `json:"source"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
}

type migrateHomeBlocker struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
	Remedy  string `json:"remedy"`
}

type migrateHomeStep struct {
	Step string `json:"step"`
	Done bool   `json:"done"`
}

func runDoctorMigrateHome(jsonOutput, dryRun, yes bool) error {
	report := migrateHomeReport{DryRun: dryRun}
	part := &report.LaunchTrust
	*part = migrateHomePart{
		Gate:         []migrateHomeGateEntry{},
		Blockers:     []migrateHomeBlocker{},
		Steps:        []migrateHomeStep{},
		OperatorNext: []string{},
		NotChecked: []string{
			"programs that embed the launchapi package: an older one asks for trust again and recreates the legacy trust directory; the next --migrate-home then refuses",
			"amq binaries outside PATH=" + os.Getenv("PATH"),
		},
	}
	move, found, err := launch.PlanLegacyTrustMove()
	part.From, part.To, part.fromProjects = move.From, move.To, move.FromProjects
	if err != nil && !part.addRefusal(err, move) {
		return err
	}
	if !found {
		part.Status = migrateHomeNothingToDo
		return writeMigrateHomeReport(report, jsonOutput)
	}
	part.Gate = migrateHomeGate("amq")
	for _, entry := range part.Gate {
		if entry.OK {
			continue
		}
		code := migrateHomeBinaryTooOld
		if entry.Version == "" {
			code = migrateHomeVersionUnreadable
		}
		part.Blockers = append(part.Blockers, migrateHomeBlocker{
			Code:    code,
			Subject: entry.Path,
			Detail:  entry.Reason,
			Remedy:  fmt.Sprintf("Replace it with amq %s or later, or remove it from PATH, then run again.", migrateHomeVersionFloor),
		})
	}
	part.Steps = append(part.Steps, migrateHomeStep{Step: fmt.Sprintf("rename %s to %s", move.From, move.To)})
	if len(part.Blockers) > 0 {
		return migrateHomeRefuse(report, jsonOutput)
	}
	part.Status = migrateHomeWouldMove
	if dryRun {
		return writeMigrateHomeReport(report, jsonOutput)
	}
	if !yes {
		if err := writeMigrateHomeReport(report, false); err != nil {
			return err
		}
		confirmed, err := confirmPrompt("Move launch trust now?")
		if err != nil {
			return err
		}
		if !confirmed {
			return writeStdoutLine("Aborted.")
		}
	}
	if err := launch.ExecuteLegacyTrustMove(move); err != nil {
		if !part.addRefusal(err, move) {
			return err
		}
		return migrateHomeRefuse(report, jsonOutput)
	}
	part.Steps[0].Done = true
	part.Status = migrateHomeMoved
	return writeMigrateHomeReport(report, jsonOutput)
}

// addRefusal records a trust move refusal as a blocker. It reports false for
// any other error, which the caller returns as it is.
func (part *migrateHomePart) addRefusal(err error, move launch.LegacyTrustMove) bool {
	var refusal *launch.TrustMoveRefusal
	if !errors.As(err, &refusal) {
		return false
	}
	part.Blockers = append(part.Blockers, migrateHomeBlocker{
		Code:    refusal.Code,
		Subject: refusal.Subject,
		Detail:  refusal.Message,
		Remedy:  migrateHomeTrustRemedy(refusal.Code, move),
	})
	return true
}

func migrateHomeRefuse(report migrateHomeReport, jsonOutput bool) error {
	report.LaunchTrust.Status = migrateHomeRefused
	if err := writeMigrateHomeReport(report, jsonOutput); err != nil {
		return err
	}
	codes := make([]string, 0, len(report.LaunchTrust.Blockers))
	for _, blocker := range report.LaunchTrust.Blockers {
		codes = append(codes, blocker.Code)
	}
	return ActionRequiredError("launch trust not moved: %s", strings.Join(codes, ", "))
}

func migrateHomeTrustRemedy(code string, move launch.LegacyTrustMove) string {
	switch code {
	case launch.TrustMoveBothExist:
		return fmt.Sprintf("New binaries use %[1]s, so %[2]s is inert. %[1]s was written after a newer amq wrote %[2]s: "+
			"by an older amq or launchapi program, by an amq launch that ran during a migration, or by an amq with another XDG_STATE_HOME. "+
			"Find that first. Then remove %[2]s (the only effect is a new trust prompt for the projects it holds) "+
			"and run amq doctor --migrate-home again while no amq launch runs. Never remove %[1]s: that makes the stale grants in %[2]s active.",
			move.From, move.To)
	case launch.TrustMoveCrossDevice:
		return "AMQ does not copy trust. Keep the legacy location; it continues to work. " +
			"Changing XDG_STATE_HOME does not move the grants: it changes which directory amq reads, and every launch asks for trust again."
	}
	return "AMQ does not follow or replace it. Make it a plain directory, then run again."
}

// migrateHomeGate reads the version of every name binary on PATH from its
// build information, without running it. A binary passes only when its
// version is readable and not older than migrateHomeVersionFloor. An empty
// PATH entry is the current directory, as it is for a shell.
func migrateHomeGate(name string) []migrateHomeGateEntry {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	entries := []migrateHomeGateEntry{}
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		candidate, err := filepath.Abs(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
			continue
		}
		path, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			path = candidate
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		entry := migrateHomeGateEntry{Source: "PATH", Path: candidate}
		if path != candidate {
			entry.Path = candidate + " -> " + path
		}
		version, err := selfupgrade.ReadEmbeddedVersion(path)
		switch {
		case err != nil:
			entry.Reason = fmt.Sprintf("version cannot be read: %v", err)
		case selfupgrade.VersionStrictlyNewer(version, migrateHomeVersionFloor):
			entry.Version = version
			entry.Reason = "older than " + migrateHomeVersionFloor
		default:
			entry.Version = version
			entry.OK = true
		}
		entries = append(entries, entry)
	}
	return entries
}

func writeMigrateHomeReport(report migrateHomeReport, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(os.Stdout, map[string]any{"migrate_home": report})
	}
	part := report.LaunchTrust
	var b strings.Builder
	b.WriteString("Launch trust\n")
	fmt.Fprintf(&b, "  from     %s", part.From)
	if part.fromProjects > 0 {
		fmt.Fprintf(&b, "  (projects: %d)", part.fromProjects)
	}
	fmt.Fprintf(&b, "\n  to       %s\n", part.To)
	for _, entry := range part.Gate {
		verdict := "ok"
		if !entry.OK {
			verdict = entry.Reason
		}
		version := entry.Version
		if version == "" {
			version = "?"
		}
		fmt.Fprintf(&b, "  gate     %s: amq %s  %s  %s\n", entry.Source, version, entry.Path, verdict)
	}
	switch part.Status {
	case migrateHomeNothingToDo:
		fmt.Fprintf(&b, "  status   nothing to do: %s does not exist\n", part.From)
	case migrateHomeWouldMove:
		b.WriteString("  status   would rename it; launches keep their trust\n")
	case migrateHomeMoved:
		b.WriteString("  status   moved; launches keep their trust\n")
	case migrateHomeRefused:
		fmt.Fprintf(&b, "  status   refused, %d blockers; nothing changed\n", len(part.Blockers))
	}
	for _, blocker := range part.Blockers {
		fmt.Fprintf(&b, "  blocker  %s: %s\n", blocker.Code, blocker.Subject)
		fmt.Fprintf(&b, "           %s\n", blocker.Detail)
		fmt.Fprintf(&b, "           %s\n", blocker.Remedy)
	}
	if part.Status != migrateHomeNothingToDo {
		b.WriteString("Not checked\n")
		for _, line := range part.NotChecked {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	if report.DryRun {
		b.WriteString("Dry run: nothing changed.\n")
	}
	return writeStdout("%s", b.String())
}
