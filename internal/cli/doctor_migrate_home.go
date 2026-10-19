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
	DryRun      bool                 `json:"dry_run"`
	LaunchTrust migrateHomeTrustPart `json:"launch_trust"`
	NotChecked  []string             `json:"not_checked"`
}

type migrateHomeTrustPart struct {
	Status   string                 `json:"status"`
	From     string                 `json:"from"`
	To       string                 `json:"to"`
	Projects int                    `json:"projects"`
	Gate     []migrateHomeGateEntry `json:"gate"`
	Refusal  string                 `json:"refusal,omitempty"`
	Detail   string                 `json:"detail,omitempty"`
	Remedy   string                 `json:"remedy,omitempty"`
}

type migrateHomeGateEntry struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
}

func runDoctorMigrateHome(jsonOutput, dryRun, yes bool) error {
	report := migrateHomeReport{
		DryRun: dryRun,
		NotChecked: []string{
			"programs that embed the launchapi package: an older one asks for trust again and recreates the legacy trust directory; the next --migrate-home then refuses",
			"amq binaries outside PATH=" + os.Getenv("PATH"),
		},
	}
	part := &report.LaunchTrust
	move, found, err := launch.PlanLegacyTrustMove()
	part.From, part.To, part.Projects = move.From, move.To, move.Projects
	if err != nil {
		return migrateHomeRefuse(&report, jsonOutput, err)
	}
	if !found {
		part.Status = migrateHomeNothingToDo
		return writeMigrateHomeReport(report, jsonOutput)
	}
	part.Gate = migrateHomeGate("amq")
	for i := range part.Gate {
		if !part.Gate[i].OK {
			code := migrateHomeBinaryTooOld
			if part.Gate[i].Version == "" {
				code = migrateHomeVersionUnreadable
			}
			return migrateHomeRefuse(&report, jsonOutput, &launch.TrustMoveRefusal{
				Code:    code,
				Message: fmt.Sprintf("%s: %s", part.Gate[i].Path, part.Gate[i].Reason),
			})
		}
	}
	if dryRun {
		part.Status = migrateHomeWouldMove
		return writeMigrateHomeReport(report, jsonOutput)
	}
	if !yes {
		part.Status = migrateHomeWouldMove
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
		return migrateHomeRefuse(&report, jsonOutput, err)
	}
	part.Status = migrateHomeMoved
	return writeMigrateHomeReport(report, jsonOutput)
}

// migrateHomeRefuse reports a refusal with exit 6. Any other error is not a
// refusal and is returned as it is.
func migrateHomeRefuse(report *migrateHomeReport, jsonOutput bool, err error) error {
	var refusal *launch.TrustMoveRefusal
	if !errors.As(err, &refusal) {
		return err
	}
	part := &report.LaunchTrust
	part.Status = migrateHomeRefused
	part.Refusal = refusal.Code
	part.Detail = refusal.Message
	part.Remedy = migrateHomeRemedy(refusal.Code, part)
	if err := writeMigrateHomeReport(*report, jsonOutput); err != nil {
		return err
	}
	return ActionRequiredError("launch trust not moved (%s): %s", refusal.Code, refusal.Message)
}

func migrateHomeRemedy(code string, part *migrateHomeTrustPart) string {
	switch code {
	case launch.TrustMoveBothExist:
		return fmt.Sprintf("New binaries use %s, so %s is inert. An older amq or launchapi program, or a different XDG_STATE_HOME, wrote it: find that first. "+
			"Then remove %s (the only effect is a new trust prompt for the projects that only it holds) and run again. "+
			"Never remove %s: that makes the stale grants in %s active.", part.From, part.To, part.To, part.From, part.To)
	case launch.TrustMoveCrossDevice:
		return "AMQ does not copy trust. Keep the legacy location; it continues to work. " +
			"Changing XDG_STATE_HOME does not move the grants: it changes which directory amq reads, and every launch asks for trust again."
	case migrateHomeBinaryTooOld, migrateHomeVersionUnreadable:
		return fmt.Sprintf("Replace it with amq %s or later, or remove it from PATH, then run again.", migrateHomeVersionFloor)
	}
	return "Fix the path, then run again."
}

// migrateHomeGate reads the version of every name binary on PATH from its
// build information, without running it. A binary passes only when its
// version is readable and not older than migrateHomeVersionFloor.
func migrateHomeGate(name string) []migrateHomeGateEntry {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	entries := []migrateHomeGateEntry{}
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
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
		entry := migrateHomeGateEntry{Path: candidate}
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
	fmt.Fprintf(&b, "  from    %s", part.From)
	if part.Projects > 0 {
		fmt.Fprintf(&b, "  (projects: %d)", part.Projects)
	}
	fmt.Fprintf(&b, "\n  to      %s\n", part.To)
	for _, entry := range part.Gate {
		verdict := "ok"
		if !entry.OK {
			verdict = entry.Reason
		}
		version := entry.Version
		if version == "" {
			version = "?"
		}
		fmt.Fprintf(&b, "  gate    amq %s  %s  %s\n", version, entry.Path, verdict)
	}
	switch part.Status {
	case migrateHomeNothingToDo:
		fmt.Fprintf(&b, "  status  nothing to do: %s does not exist\n", part.From)
	case migrateHomeWouldMove:
		b.WriteString("  status  would rename it; launches keep their trust\n")
	case migrateHomeMoved:
		b.WriteString("  status  moved; launches keep their trust\n")
	case migrateHomeRefused:
		fmt.Fprintf(&b, "  status  refused (%s): %s; nothing changed\n", part.Refusal, part.Detail)
		fmt.Fprintf(&b, "  remedy  %s\n", part.Remedy)
	}
	if part.Status != migrateHomeNothingToDo {
		b.WriteString("Not checked\n")
		for _, line := range report.NotChecked {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	if report.DryRun {
		b.WriteString("Dry run: nothing changed.\n")
	}
	return writeStdout("%s", b.String())
}
