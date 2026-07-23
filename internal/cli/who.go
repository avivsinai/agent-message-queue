package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func runWho(args []string) error {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	common := addCommonFlags(fs)
	allFlag := fs.Bool("all", false, "Include sessions whose owner-bound agent processes are all conclusively dead")

	usage := usageWithFlags(fs, "amq who [options]",
		"List sessions and agents in the current project.",
		"Shows active/stale status and whether activity comes from a verified notifier or recent commands.",
		"Sessions whose owner-bound agent processes are all conclusively dead are hidden unless --all is used.",
		"Legacy or unverified ownership remains visible.")
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

	// Enumerate sessions by scanning subdirectories.
	entries, err := os.ReadDir(baseRoot)
	if err != nil {
		return err
	}

	type agentInfo struct {
		Handle             string `json:"handle"`
		Kind               string `json:"kind"`
		PresenceApplicable bool   `json:"presence_applicable"`
		Active             bool   `json:"active"`
		PresenceSource     string `json:"presence_source,omitempty"`
		Note               string `json:"note,omitempty"`
	}
	type sessionInfo struct {
		Name   string      `json:"name"`
		Agents []agentInfo `json:"agents"`
	}

	var sessions []sessionInfo
	currentSession := sessionName(root)

	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sessDir := filepath.Join(baseRoot, e.Name())
		agentsDir := filepath.Join(sessDir, "agents")
		agentEntries, err := os.ReadDir(agentsDir)
		if err != nil {
			continue // not a session
		}

		var agents []agentInfo
		agentCount := 0
		conclusivelyDeadCount := 0
		for _, ae := range agentEntries {
			if !ae.IsDir() {
				continue
			}
			inbox := filepath.Join(agentsDir, ae.Name(), "inbox")
			if _, err := os.Stat(inbox); err != nil {
				continue
			}

			ai := agentInfo{
				Handle:             ae.Name(),
				Kind:               "agent",
				PresenceApplicable: true,
			}
			if ai.Handle == reservedHumanHandle {
				ai.Kind = "human"
				ai.PresenceApplicable = false
				if p, err := presence.Read(sessDir, ae.Name()); err == nil {
					ai.Note = p.Note
				}
				agents = append(agents, ai)
				continue
			}

			agentCount++
			ownerState := classifyPersistedWakeTargetOwner(sessDir, ae.Name())
			// Recent presence proves activity only; a verified wake lock separately
			// proves that a prompt notifier is currently attached.
			recentActivity := false
			if p, err := presence.Read(sessDir, ae.Name()); err == nil {
				if t, err := time.Parse(time.RFC3339Nano, p.LastSeen); err == nil {
					recentActivity = time.Since(t) < 10*time.Minute
				}
				ai.Note = p.Note
			}
			ai.PresenceSource = resolvePresenceSource(sessDir, ae.Name(), recentActivity)
			ai.Active = recentActivity || ai.PresenceSource == presenceSourceNotifierLive
			if ownerState == wakeIdentityGoneOrDifferent {
				// Exact owner metadata overrides stale presence and an orphaned
				// notifier. No files are changed by discovery.
				ai.Active = false
				ai.PresenceSource = ""
				conclusivelyDeadCount++
			}
			agents = append(agents, ai)
		}

		if len(agents) > 0 {
			if !*allFlag && agentCount > 0 && conclusivelyDeadCount == agentCount {
				continue
			}
			sessions = append(sessions, sessionInfo{
				Name:   e.Name(),
				Agents: agents,
			})
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
		if s.Name == currentSession {
			marker = " (current)"
		}
		if err := writeStdout("  %s%s\n", s.Name, marker); err != nil {
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

// classifyPersistedWakeTargetOwner is intentionally read-only. Missing,
// legacy, corrupt, or inspection-ambiguous metadata stays unknown so discovery
// cannot hide a session without conclusive process identity evidence.
func classifyPersistedWakeTargetOwner(root, me string) wakeIdentityState {
	target, exists, err := readWakeTarget(root, me)
	if err != nil || !exists || target.Owner == nil {
		return wakeIdentityUnknown
	}
	if err := validateWakeTarget(target, root, me); err != nil {
		return wakeIdentityUnknown
	}
	state, _ := classifyWakeOwnerIdentity(*target.Owner)
	return state
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
