package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/discover"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/metadata"
)

const activeTTL = 10 * time.Minute

type whoSession struct {
	Name   string     `json:"name"`
	Topic  string     `json:"topic,omitempty"`
	Branch string     `json:"branch,omitempty"`
	Claims []string   `json:"claims,omitempty"`
	Agents []whoAgent `json:"agents"`
}

type whoAgent struct {
	Handle   string   `json:"handle"`
	Status   string   `json:"status"` // "active" or "stale"
	LastSeen string   `json:"last_seen,omitempty"`
	Channels []string `json:"channels,omitempty"`
}

func runWho(args []string) error {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	common := addCommonFlags(fs)

	usage := usageWithFlags(fs, "amq who [--json]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}

	root := resolveRoot(common.Root)

	// Find the base root with the following precedence:
	// 1. AM_BASE_ROOT env var (set by coop exec)
	// 2. .amqrc discovery from cwd
	// 3. Heuristic: walk up from root if it looks like a session dir
	var baseRoot string
	if envBase := strings.TrimSpace(os.Getenv(envBaseRoot)); envBase != "" {
		baseRoot = envBase
	}

	if baseRoot == "" {
		cwd, err := os.Getwd()
		if err == nil {
			proj, projErr := discover.DiscoverProject(cwd)
			if projErr == nil {
				baseRoot = proj.BaseRoot
			}
		}
	}

	if baseRoot == "" {
		baseRoot = root
		// If root looks like a session dir (has agents/ subdir), try going up one level.
		if dirExists(filepath.Join(root, "agents")) {
			parentCandidate := filepath.Dir(root)
			if parentCandidate != root {
				// Check if the parent has multiple session dirs
				entries, err := os.ReadDir(parentCandidate)
				if err == nil {
					for _, e := range entries {
						if e.IsDir() && dirExists(filepath.Join(parentCandidate, e.Name(), "agents")) {
							baseRoot = parentCandidate
							break
						}
					}
				}
			}
		}
	}

	// Enumerate sessions from the base root
	sessions, err := enumerateWhoSessions(baseRoot)
	if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, sessions)
	}

	if len(sessions) == 0 {
		return writeStdoutLine("No sessions found.")
	}

	for i, sess := range sessions {
		if i > 0 {
			if err := writeStdoutLine(""); err != nil {
				return err
			}
		}
		header := "Session: " + sess.Name
		if sess.Topic != "" {
			header += "  (" + sess.Topic + ")"
		}
		if err := writeStdoutLine(header); err != nil {
			return err
		}
		if sess.Branch != "" {
			if err := writeStdout("  Branch: %s\n", sess.Branch); err != nil {
				return err
			}
		}
		if len(sess.Claims) > 0 {
			if err := writeStdout("  Claims: %s\n", strings.Join(sess.Claims, ", ")); err != nil {
				return err
			}
		}
		if len(sess.Agents) == 0 {
			if err := writeStdout("  (no agents)\n"); err != nil {
				return err
			}
		}
		for _, agent := range sess.Agents {
			channelInfo := ""
			if len(agent.Channels) > 0 {
				channelInfo = "  channels: " + strings.Join(agent.Channels, ",")
			}
			if err := writeStdout("  %-12s  %-6s  %s%s\n", agent.Handle, agent.Status, agent.LastSeen, channelInfo); err != nil {
				return err
			}
		}
	}
	return nil
}

func enumerateWhoSessions(baseRoot string) ([]whoSession, error) {
	entries, err := os.ReadDir(baseRoot)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var sessions []whoSession

	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		sessDir := filepath.Join(baseRoot, e.Name())
		agentsDir := filepath.Join(sessDir, "agents")
		if !dirExists(agentsDir) {
			continue
		}

		sess := whoSession{Name: e.Name()}

		// Read session.json if available
		sessionJSONPath := fsq.SessionJSON(sessDir)
		sessMeta, err := metadata.ReadSessionMeta(sessionJSONPath)
		if err == nil {
			sess.Topic = sessMeta.Topic
			sess.Branch = sessMeta.Branch
			sess.Claims = sessMeta.Claims
		}

		// Enumerate agents
		agentEntries, err := os.ReadDir(agentsDir)
		if err != nil {
			continue
		}
		for _, ae := range agentEntries {
			if !ae.IsDir() {
				continue
			}
			handle := ae.Name()
			// Verify inbox exists
			inbox := filepath.Join(agentsDir, handle, "inbox")
			if !dirExists(inbox) {
				continue
			}

			agent := whoAgent{
				Handle: handle,
				Status: "stale",
			}

			// Read agent.json if available
			agentJSONPath := fsq.AgentJSON(sessDir, handle)
			agentMeta, err := metadata.ReadAgentMeta(agentJSONPath)
			if err == nil {
				if !agentMeta.LastSeen.IsZero() {
					agent.LastSeen = agentMeta.LastSeen.Format(time.RFC3339)
					if now.Sub(agentMeta.LastSeen) < activeTTL {
						agent.Status = "active"
					}
				}
				agent.Channels = agentMeta.Channels
			}

			sess.Agents = append(sess.Agents, agent)
		}

		sessions = append(sessions, sess)
	}

	return sessions, nil
}
