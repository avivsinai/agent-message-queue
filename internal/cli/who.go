package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runWho(args []string) error {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	common := addCommonFlags(fs)

	usage := usageWithFlags(fs, "amq who [options]",
		"List sessions and agents in the current project.",
		"Shows active/stale status and whether activity comes from a verified notifier or recent commands.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	root := resolveRoot(common.Root)

	// Determine base root using the centralized classifier.
	// Falls back to checking if root itself contains session subdirs.
	baseRoot := classifyRootForDisplay(root)
	if baseRoot == "" {
		// classifyRoot couldn't determine from env or sibling sessions.
		// Check if root IS the base root (contains session subdirs).
		if hasSessionSubdirs(root) {
			baseRoot = root
		} else {
			baseRoot = filepath.Dir(root)
		}
	}
	// Only a resolved base lends its roster to the sessions below it; a
	// display guess picks the tree to list, never declarations.
	_, baseKnown := classifyRootWithEvidence(root)

	scopes, err := inventoryProject(baseRoot, baseKnown)
	if err != nil {
		return err
	}

	type agentInfo struct {
		Handle             string `json:"handle"`
		Kind               string `json:"kind"`
		PresenceApplicable bool   `json:"presence_applicable"`
		Active             bool   `json:"active"`
		PresenceSource     string `json:"presence_source,omitempty"`
		NotifierStatus     string `json:"notifier_status,omitempty"`
		NotifierMode       string `json:"notifier_mode,omitempty"`
		NotifierReason     string `json:"notifier_reason,omitempty"`
		Note               string `json:"note,omitempty"`
		agentRegistrationJSON
		MailboxStatus string   `json:"mailbox_status"`
		MailboxIssues []string `json:"mailbox_issues,omitempty"`
	}
	type sessionInfo struct {
		// Name is the session name, or "" for the base queue's own agents
		// (scope "base"), which has no session name to route by.
		Name   string      `json:"name"`
		Scope  string      `json:"scope"`
		Root   string      `json:"root"`
		Agents []agentInfo `json:"agents"`
	}

	var sessions []sessionInfo
	currentSession := sessionName(root)
	currentRoot := absPath(root)

	for _, scope := range scopes {
		var agents []agentInfo
		for _, row := range scope.Agents {
			ai := agentInfo{
				Handle:                row.Handle,
				Kind:                  row.Registration.Kind,
				PresenceApplicable:    true,
				agentRegistrationJSON: row.Registration.json(),
				MailboxStatus:         inventoryMailboxStatus(row),
				MailboxIssues:         row.MailboxIssues,
			}
			if ai.Kind == "human" {
				ai.PresenceApplicable = false
				if row.Presence != nil {
					ai.Note = row.Presence.Note
				}
				agents = append(agents, ai)
				continue
			}

			// Recent presence proves activity only; a verified wake lock separately
			// proves that a prompt notifier is currently attached.
			recentActivity := false
			if p := row.Presence; p != nil {
				if t, err := time.Parse(time.RFC3339Nano, p.LastSeen); err == nil {
					recentActivity = time.Since(t) < 10*time.Minute
				}
				ai.Note = p.Note
				ai.NotifierStatus = p.NotifierStatus
				ai.NotifierMode = p.NotifierMode
				ai.NotifierReason = p.NotifierReason
			}
			ai.PresenceSource = resolvePresenceSource(scope.Root, row.Handle, recentActivity)
			ai.Active = recentActivity || ai.PresenceSource == presenceSourceNotifierLive
			agents = append(agents, ai)
		}

		if len(agents) > 0 {
			info := sessionInfo{Name: scope.Session, Scope: "session", Root: absPath(scope.Root), Agents: agents}
			if scope.Kind == inventoryScopeBase {
				info.Scope = inventoryScopeBase
			}
			sessions = append(sessions, info)
		}
	}

	if common.JSON {
		return writeJSON(os.Stdout, sessions)
	}

	// Presence in this output is scoped to one base root. Show which tree is
	// being read so "active/stale" is never mistaken for global liveness when
	// the same session name exists in another root.
	displayBase := baseRoot
	if abs, err := filepath.Abs(baseRoot); err == nil {
		displayBase = abs
	}
	if err := writeStdout("Base root: %s\n", displayBase); err != nil {
		return err
	}

	if len(sessions) == 0 {
		return writeStdoutLine("No sessions found.")
	}

	for _, s := range sessions {
		marker := ""
		if s.Root == currentRoot || (s.Scope != inventoryScopeBase && s.Name == currentSession) {
			marker = " (current)"
		}
		name := s.Name
		if s.Scope == inventoryScopeBase {
			name = "(base root)"
		}
		if err := writeStdout("  %s%s\n", name, marker); err != nil {
			return err
		}
		for _, a := range s.Agents {
			status := "stale"
			if !a.PresenceApplicable {
				status = "human"
			} else if a.Active {
				status = "active"
				if a.PresenceSource != "" {
					status += " (" + a.PresenceSource + ")"
				}
				if a.NotifierStatus != "" {
					status += "; " + a.NotifierStatus +
						" mode=" + a.NotifierMode +
						" reason=" + a.NotifierReason
				}
			}
			if a.RegistrationStatus != registrationDeclared {
				status += " [" + string(a.RegistrationStatus) + "]"
			}
			if a.MailboxStatus != mailboxStatusComplete {
				status += " [mailbox " + a.MailboxStatus + "]"
			}
			note := ""
			if a.Note != "" {
				note = " — " + a.Note
			}
			if err := writeStdout("    %s  %s%s\n", a.Handle, status, note); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasSessionSubdirs returns true if dir contains at least one subdirectory
// that itself contains an agents/ directory (i.e., dir is a base root).
func hasSessionSubdirs(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		if dirExists(filepath.Join(dir, e.Name(), "agents")) {
			return true
		}
	}
	return false
}
