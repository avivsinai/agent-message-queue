package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

// migrateHomeVersionFloor is the first release that keeps launch trust in
// ~/.amq/state on a fresh machine and the keepalive home in
// ~/.amq/keepalive. An older binary uses only the legacy locations,
// so it would write <legacy>/launch or ~/.amq-keepalive again after the move.
const migrateHomeVersionFloor = "0.97.0"

const (
	migrateHomeNothingToDo = "nothing_to_do"
	migrateHomeWouldMove   = "would_move"
	migrateHomeMoved       = "moved"
	migrateHomeFinished    = "finished"
	migrateHomeRefused     = "refused"

	migrateHomeBinaryTooOld      = "binary_too_old"
	migrateHomeVersionUnreadable = "version_unreadable"
)

// errMigrateHomeRefused reports that a part's run recorded a blocker.
var errMigrateHomeRefused = errors.New("home migration refused")

// migrateHomeRun runs a probe (ps, launchctl print, systemctl is-active) or a
// launchctl step. It returns the combined output and the exit code; err is set
// only when the command did not run.
var migrateHomeRun = func(name string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode(), nil
	}
	if err != nil {
		return string(out), -1, err
	}
	return string(out), 0, nil
}

type migrateHomeReport struct {
	DryRun      bool            `json:"dry_run"`
	LaunchTrust migrateHomePart `json:"launch_trust"`
	Keepalive   migrateHomePart `json:"keepalive"`
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
	title        string
	summary      string
}

type migrateHomeGateEntry struct {
	Source  string `json:"source"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	remedy  string
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

func newMigrateHomePart(title string, notChecked ...string) migrateHomePart {
	return migrateHomePart{
		Gate:         []migrateHomeGateEntry{},
		Blockers:     []migrateHomeBlocker{},
		Steps:        []migrateHomeStep{},
		OperatorNext: []string{},
		NotChecked:   notChecked,
		title:        title,
	}
}

func (part *migrateHomePart) block(code, subject, detail, remedy string) {
	part.Blockers = append(part.Blockers, migrateHomeBlocker{Code: code, Subject: subject, Detail: detail, Remedy: remedy})
}

// step adds a planned step and returns its index.
func (part *migrateHomePart) step(format string, args ...any) int {
	part.Steps = append(part.Steps, migrateHomeStep{Step: fmt.Sprintf(format, args...)})
	return len(part.Steps) - 1
}

func runDoctorMigrateHome(jsonOutput, dryRun, yes bool) error {
	report := migrateHomeReport{
		DryRun: dryRun,
		LaunchTrust: newMigrateHomePart("Launch trust",
			"programs that embed the launchapi package: an older one asks for trust again and recreates the legacy trust directory; the next --migrate-home then refuses",
			"amq binaries outside PATH="+os.Getenv("PATH"),
		),
		Keepalive: newMigrateHomePart("Keepalive home",
			"amq, amq-keepalive and amq-remote binaries outside PATH="+os.Getenv("PATH")+" that no plist, hook or wake names",
			"a supervisor you start yourself with an explicit --registry",
		),
	}
	parts := []*migrateHomePart{&report.LaunchTrust, &report.Keepalive}
	runs := make([]func() error, len(parts))
	var err error
	if runs[0], err = planMigrateHomeTrust(parts[0]); err != nil {
		return err
	}
	if runs[1], err = planMigrateHomeKeepalive(parts[1]); err != nil {
		return err
	}
	pending := 0
	for i, part := range parts {
		switch {
		case len(part.Blockers) > 0:
			part.Status = migrateHomeRefused
			runs[i] = nil
		case runs[i] == nil:
			part.Status = migrateHomeNothingToDo
		default:
			part.Status = migrateHomeWouldMove
			pending++
		}
	}
	if !dryRun && pending > 0 {
		if !yes {
			if err := writeMigrateHomeReport(report, false); err != nil {
				return err
			}
			confirmed, err := confirmPrompt("Migrate now?")
			if err != nil {
				return err
			}
			if !confirmed {
				return writeStdoutLine("Aborted.")
			}
		}
		for i, part := range parts {
			if runs[i] == nil {
				continue
			}
			if err := runs[i](); errors.Is(err, errMigrateHomeRefused) {
				part.Status = migrateHomeRefused
			} else if err != nil {
				return err
			}
		}
	}
	if err := writeMigrateHomeReport(report, jsonOutput); err != nil {
		return err
	}
	var refused []string
	for _, part := range parts {
		if part.Status != migrateHomeRefused {
			continue
		}
		codes := make([]string, 0, len(part.Blockers))
		for _, blocker := range part.Blockers {
			codes = append(codes, blocker.Code)
		}
		refused = append(refused, fmt.Sprintf("%s (%s)", strings.ToLower(part.title), strings.Join(codes, ", ")))
	}
	if len(refused) > 0 {
		return ActionRequiredError("not migrated: %s", strings.Join(refused, "; "))
	}
	return nil
}

// planMigrateHomeTrust plans the launch trust move. It returns nil when there
// is nothing to run.
func planMigrateHomeTrust(part *migrateHomePart) (func() error, error) {
	move, found, err := launch.PlanLegacyTrustMove()
	part.From, part.To = move.From, move.To
	if move.FromProjects > 0 {
		part.summary = fmt.Sprintf("projects: %d", move.FromProjects)
	}
	if err != nil && !part.addTrustRefusal(err, move) {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	gate := newMigrateHomeGate()
	gate.addDirs("PATH", migrateHomePATH(), "amq")
	gate.report(part, func(string) string {
		return fmt.Sprintf("Replace it with amq %s or later, or remove it from PATH, then run again.", migrateHomeVersionFloor)
	})
	rename := part.step("rename %s to %s", move.From, move.To)
	return func() error {
		if err := launch.ExecuteLegacyTrustMove(move); err != nil {
			if part.addTrustRefusal(err, move) {
				return errMigrateHomeRefused
			}
			return err
		}
		part.Steps[rename].Done = true
		part.Status = migrateHomeMoved
		return nil
	}, nil
}

// addTrustRefusal records a trust move refusal as a blocker. It reports false
// for any other error, which the caller returns as it is.
func (part *migrateHomePart) addTrustRefusal(err error, move launch.LegacyTrustMove) bool {
	var refusal *launch.TrustMoveRefusal
	if !errors.As(err, &refusal) {
		return false
	}
	part.block(refusal.Code, refusal.Subject, refusal.Message, migrateHomeTrustRemedy(refusal.Code, move))
	return true
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

// migrateHomePATH is this process's PATH. An empty entry is the current
// directory, as it is for a shell.
func migrateHomePATH() []string {
	return migrateHomeSplitPATH(os.Getenv("PATH"))
}

func migrateHomeSplitPATH(value string) []string {
	dirs := filepath.SplitList(value)
	for i, dir := range dirs {
		if dir == "" {
			dirs[i] = "."
		}
	}
	return dirs
}

// migrateHomeGate collects the binaries that can write a legacy home and
// reads each version from build information, without running it. A binary
// passes only when its version is readable and not older than
// migrateHomeVersionFloor. A path that names no executable file is skipped:
// nothing can run it.
type migrateHomeGate struct {
	entries []migrateHomeGateEntry
	seen    map[string]bool
}

func newMigrateHomeGate() *migrateHomeGate {
	return &migrateHomeGate{entries: []migrateHomeGateEntry{}, seen: map[string]bool{}}
}

func (gate *migrateHomeGate) addDirs(source string, dirs []string, names ...string) {
	for _, dir := range dirs {
		for _, name := range names {
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			gate.addPath(source, filepath.Join(dir, name))
		}
	}
}

func (gate *migrateHomeGate) addPath(source, candidate string) {
	gate.addPathRemedy(source, candidate, "")
}

// addPathRemedy adds a binary whose remedy, when it fails, is remedy rather
// than the one for its source.
func (gate *migrateHomeGate) addPathRemedy(source, candidate, remedy string) {
	if strings.TrimSpace(candidate) == "" {
		return
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return
	}
	path, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		path = candidate
	}
	if gate.seen[path] {
		return
	}
	gate.seen[path] = true
	entry := migrateHomeGateEntry{Source: source, Path: candidate, remedy: remedy}
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
	gate.entries = append(gate.entries, entry)
}

// report puts the gate into part, with a blocker for each binary that fails.
func (gate *migrateHomeGate) report(part *migrateHomePart, remedy func(source string) string) {
	part.Gate = gate.entries
	for _, entry := range gate.entries {
		if entry.OK {
			continue
		}
		code := migrateHomeBinaryTooOld
		if entry.Version == "" {
			code = migrateHomeVersionUnreadable
		}
		fix := entry.remedy
		if fix == "" {
			fix = remedy(entry.Source)
		}
		part.block(code, entry.Path, entry.Source+": "+entry.Reason, fix)
	}
}

func writeMigrateHomeReport(report migrateHomeReport, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(os.Stdout, map[string]any{"migrate_home": report})
	}
	var b strings.Builder
	for _, part := range []migrateHomePart{report.LaunchTrust, report.Keepalive} {
		b.WriteString(part.title + "\n")
		fmt.Fprintf(&b, "  from     %s", part.From)
		if part.summary != "" {
			fmt.Fprintf(&b, "  (%s)", part.summary)
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
			fmt.Fprintf(&b, "  gate     %s: %s  %s  %s\n", entry.Source, version, entry.Path, verdict)
		}
		switch part.Status {
		case migrateHomeNothingToDo:
			b.WriteString("  status   nothing to do\n")
		case migrateHomeWouldMove:
			b.WriteString("  status   would migrate\n")
		case migrateHomeMoved:
			b.WriteString("  status   moved\n")
		case migrateHomeFinished:
			b.WriteString("  status   finished an earlier move\n")
		case migrateHomeRefused:
			fmt.Fprintf(&b, "  status   refused, %d blockers\n", len(part.Blockers))
		}
		for _, step := range part.Steps {
			mark := "todo"
			if step.Done {
				mark = "done"
			}
			fmt.Fprintf(&b, "  step     %s: %s\n", mark, step.Step)
		}
		for _, blocker := range part.Blockers {
			fmt.Fprintf(&b, "  blocker  %s: %s\n", blocker.Code, blocker.Subject)
			fmt.Fprintf(&b, "           %s\n", blocker.Detail)
			fmt.Fprintf(&b, "           %s\n", blocker.Remedy)
		}
		for _, next := range part.OperatorNext {
			fmt.Fprintf(&b, "  next     %s\n", next)
		}
		if part.Status != migrateHomeNothingToDo {
			for _, line := range part.NotChecked {
				fmt.Fprintf(&b, "  unchecked %s\n", line)
			}
		}
	}
	if report.DryRun {
		b.WriteString("Dry run: nothing changed.\n")
	}
	return writeStdout("%s", b.String())
}
