package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/presence"
)

// runIntegrationClaude dispatches "amq integration claude" subcommands.
func runIntegrationClaude(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printClaudeIntegrationUsage()
	}
	switch args[0] {
	case "context":
		return runClaudeContext(args[1:])
	default:
		return formatUnknownSubcommand("integration claude", args[0])
	}
}

func printClaudeIntegrationUsage() error {
	lines := []string{
		"amq integration claude - Claude Code session awareness",
		"",
		"Subcommands:",
		"  context  Emit coop session preamble for context re-injection",
		"",
		"Examples:",
		"  amq integration claude context",
		"  amq integration claude context --event session-start --json",
		"",
		`Use "amq integration claude <subcommand> --help" for details.`,
	}
	return writeLines(lines)
}

// claudeContextOutput is the JSON output for amq integration claude context.
type claudeContextOutput struct {
	Me      string            `json:"me"`
	Session string            `json:"session,omitempty"`
	Project string            `json:"project,omitempty"`
	Peers   []claudePeerInfo  `json:"peers,omitempty"`
	Inbox   claudeInboxInfo   `json:"inbox"`
	Preamble string           `json:"preamble"`
}

type claudePeerInfo struct {
	Handle string `json:"handle"`
	Active bool   `json:"active"`
}

type claudeInboxInfo struct {
	New int `json:"new"`
}

func runClaudeContext(args []string) error {
	fs := flag.NewFlagSet("integration claude context", flag.ContinueOnError)
	common := addCommonFlags(fs)
	eventFlag := fs.String("event", "", "Hook event: session-start, clear, compaction (informational)")

	usage := usageWithFlags(fs, "amq integration claude context [options]",
		"Emit a coop session preamble for context re-injection after /clear or compaction.",
		"",
		"Reads durable state (AM_ROOT, AM_ME, .amqrc, peer presence) and outputs",
		"a deterministic preamble that restores agent awareness of the coop session.",
		"",
		"Designed to be called from Claude Code's SessionStart hook.",
		"",
		"Examples:",
		"  amq integration claude context",
		"  amq integration claude context --event session-start --json",
		"  amq integration claude context --me claude --root .agent-mail/collab",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	_ = eventFlag // informational only, no behavior change

	root := resolveRoot(common.Root)
	if root == "" {
		return fmt.Errorf("AMQ root is not configured (set AM_ROOT, use --root, or run amq coop init)")
	}

	me := common.Me
	if me == "" {
		me = "claude" // sensible default for this integration
	}

	// Resolve session name
	session := resolveSessionName(root)

	// Resolve project name
	project := resolveProject(root)

	// Discover peers
	peers := discoverPeers(root, me)

	// Count new inbox messages
	newCount := countNewMessages(root, me)

	// Build preamble text
	preamble := buildPreamble(me, session, project, peers, newCount)

	if common.JSON {
		out := claudeContextOutput{
			Me:       me,
			Session:  session,
			Project:  project,
			Peers:    peers,
			Inbox:    claudeInboxInfo{New: newCount},
			Preamble: preamble,
		}
		return writeJSON(os.Stdout, out)
	}

	// Plain text output (for piping or manual use)
	return writeStdout("%s\n", preamble)
}

// discoverPeers finds other agents in the same session directory.
func discoverPeers(root, me string) []claudePeerInfo {
	agentsDir := filepath.Join(root, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}

	var peers []claudePeerInfo
	for _, e := range entries {
		if !e.IsDir() || e.Name() == me {
			continue
		}
		// Verify it has an inbox (it's a real agent, not a stale dir)
		inbox := filepath.Join(agentsDir, e.Name(), "inbox")
		if _, err := os.Stat(inbox); err != nil {
			continue
		}

		pi := claudePeerInfo{Handle: e.Name()}
		if p, err := presence.Read(root, e.Name()); err == nil {
			if t, err := time.Parse(time.RFC3339Nano, p.LastSeen); err == nil {
				pi.Active = time.Since(t) < 10*time.Minute
			}
		}
		peers = append(peers, pi)
	}
	return peers
}

// countNewMessages counts unread messages in the agent's inbox/new directory.
func countNewMessages(root, me string) int {
	newDir := filepath.Join(root, "agents", me, "inbox", "new")
	entries, err := os.ReadDir(newDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			count++
		}
	}
	return count
}

// buildPreamble constructs the deterministic context string.
func buildPreamble(me, session, project string, peers []claudePeerInfo, newMessages int) string {
	var b strings.Builder

	// Line 1: identity + session
	b.WriteString(fmt.Sprintf("AMQ coop active: me=%s", me))
	if session != "" {
		b.WriteString(fmt.Sprintf(" session=%s", session))
	}
	if project != "" {
		b.WriteString(fmt.Sprintf(" project=%s", project))
	}

	// Peers
	if len(peers) > 0 {
		names := make([]string, len(peers))
		for i, p := range peers {
			status := "stale"
			if p.Active {
				status = "active"
			}
			names[i] = fmt.Sprintf("%s(%s)", p.Handle, status)
		}
		b.WriteString(fmt.Sprintf(" peers=%s", strings.Join(names, ",")))
	}
	b.WriteString(".")

	// Line 2: inbox
	if newMessages > 0 {
		b.WriteString(fmt.Sprintf("\nInbox: %d unread message(s). Run: amq drain --me %s", newMessages, me))
	}

	// Line 3: routing instructions
	b.WriteString("\nUse amq send/reply/drain for peer coordination. Preserve thread IDs on replies.")

	return b.String()
}

// marshalClaudeHookOutput produces the JSON payload for Claude Code's
// hookSpecificOutput format, suitable for SessionStart hooks.
func marshalClaudeHookOutput(preamble string, bannerLine string) ([]byte, error) {
	payload := map[string]any{
		"hookSpecificOutput": map[string]any{
			"additionalContext": preamble,
		},
	}
	if bannerLine != "" {
		payload["systemMessage"] = bannerLine
	}
	return json.Marshal(payload)
}
