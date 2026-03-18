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

	usage := usageWithFlags(fs, "amq who [options]",
		"List sessions and agents in the current project.",
		"Shows active/stale status based on presence data.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	root := resolveRoot(common.Root)

	// Determine base root (parent of sessions).
	baseRoot := resolveBaseRootForSend(root, "")

	// Enumerate sessions by scanning subdirectories.
	entries, err := os.ReadDir(baseRoot)
	if err != nil {
		return err
	}

	type agentInfo struct {
		Handle string `json:"handle"`
		Active bool   `json:"active"`
		Note   string `json:"note,omitempty"`
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
		for _, ae := range agentEntries {
			if !ae.IsDir() {
				continue
			}
			inbox := filepath.Join(agentsDir, ae.Name(), "inbox")
			if _, err := os.Stat(inbox); err != nil {
				continue
			}

			ai := agentInfo{Handle: ae.Name()}
			// Check presence for activity status.
			if p, err := presence.Read(sessDir, ae.Name()); err == nil {
				if t, err := time.Parse(time.RFC3339Nano, p.LastSeen); err == nil {
					ai.Active = time.Since(t) < 10*time.Minute
				}
				ai.Note = p.Note
			}
			agents = append(agents, ai)
		}

		if len(agents) > 0 {
			sessions = append(sessions, sessionInfo{
				Name:   e.Name(),
				Agents: agents,
			})
		}
	}

	if common.JSON {
		return writeJSON(os.Stdout, sessions)
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
			if a.Active {
				status = "active"
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
