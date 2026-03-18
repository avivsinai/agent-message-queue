package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/metadata"
)

func runChannel(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		printChannelUsage()
		return nil
	}
	switch args[0] {
	case "join":
		return runChannelJoin(args[1:])
	case "leave":
		return runChannelLeave(args[1:])
	case "list":
		return runChannelList(args[1:])
	default:
		return fmt.Errorf("unknown channel command: %s", args[0])
	}
}

func runChannelJoin(args []string) error {
	fs := flag.NewFlagSet("channel join", flag.ContinueOnError)
	common := addCommonFlags(fs)
	nameFlag := fs.String("name", "", "Channel name to join")

	usage := usageWithFlags(fs, "amq channel join --name <channel> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me

	channelName := strings.TrimSpace(*nameFlag)
	if channelName == "" {
		// Also accept as positional arg
		if fs.NArg() > 0 {
			channelName = strings.TrimSpace(fs.Arg(0))
		}
	}
	if channelName == "" {
		return UsageError("--name is required (channel name to join)")
	}
	// Strip leading # if present
	channelName = strings.TrimPrefix(channelName, "#")
	if channelName == "" {
		return UsageError("channel name cannot be empty")
	}

	root := resolveRoot(common.Root)
	agentJSONPath := fsq.AgentJSON(root, me)

	// Read existing metadata or create new
	agentMeta, err := metadata.ReadAgentMeta(agentJSONPath)
	if err != nil {
		agentMeta = metadata.AgentMeta{
			Agent:    me,
			LastSeen: time.Now().UTC(),
		}
	}

	// Check if already joined
	for _, ch := range agentMeta.Channels {
		if ch == channelName {
			if common.JSON {
				return writeJSON(os.Stdout, map[string]any{
					"channel": channelName,
					"agent":   me,
					"action":  "already_joined",
				})
			}
			return writeStdout("Already joined #%s\n", channelName)
		}
	}

	agentMeta.Channels = append(agentMeta.Channels, channelName)
	agentMeta.LastSeen = time.Now().UTC()

	if err := metadata.WriteAgentMeta(agentJSONPath, agentMeta); err != nil {
		return fmt.Errorf("write agent.json: %w", err)
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"channel":  channelName,
			"agent":    me,
			"action":   "joined",
			"channels": agentMeta.Channels,
		})
	}
	return writeStdout("Joined #%s\n", channelName)
}

func runChannelLeave(args []string) error {
	fs := flag.NewFlagSet("channel leave", flag.ContinueOnError)
	common := addCommonFlags(fs)
	nameFlag := fs.String("name", "", "Channel name to leave")

	usage := usageWithFlags(fs, "amq channel leave --name <channel> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me

	channelName := strings.TrimSpace(*nameFlag)
	if channelName == "" {
		if fs.NArg() > 0 {
			channelName = strings.TrimSpace(fs.Arg(0))
		}
	}
	if channelName == "" {
		return UsageError("--name is required (channel name to leave)")
	}
	channelName = strings.TrimPrefix(channelName, "#")
	if channelName == "" {
		return UsageError("channel name cannot be empty")
	}

	root := resolveRoot(common.Root)
	agentJSONPath := fsq.AgentJSON(root, me)

	agentMeta, err := metadata.ReadAgentMeta(agentJSONPath)
	if err != nil {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"channel": channelName,
				"agent":   me,
				"action":  "not_joined",
			})
		}
		return writeStdout("Not a member of #%s\n", channelName)
	}

	found := false
	newChannels := make([]string, 0, len(agentMeta.Channels))
	for _, ch := range agentMeta.Channels {
		if ch == channelName {
			found = true
			continue
		}
		newChannels = append(newChannels, ch)
	}

	if !found {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"channel": channelName,
				"agent":   me,
				"action":  "not_joined",
			})
		}
		return writeStdout("Not a member of #%s\n", channelName)
	}

	agentMeta.Channels = newChannels
	agentMeta.LastSeen = time.Now().UTC()

	if err := metadata.WriteAgentMeta(agentJSONPath, agentMeta); err != nil {
		return fmt.Errorf("write agent.json: %w", err)
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"channel":  channelName,
			"agent":    me,
			"action":   "left",
			"channels": agentMeta.Channels,
		})
	}
	return writeStdout("Left #%s\n", channelName)
}

func runChannelList(args []string) error {
	fs := flag.NewFlagSet("channel list", flag.ContinueOnError)
	common := addCommonFlags(fs)

	usage := usageWithFlags(fs, "amq channel list [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me

	root := resolveRoot(common.Root)
	agentJSONPath := fsq.AgentJSON(root, me)

	agentMeta, err := metadata.ReadAgentMeta(agentJSONPath)
	if err != nil {
		if common.JSON {
			return writeJSON(os.Stdout, []string{})
		}
		return writeStdoutLine("No channel memberships.")
	}

	if common.JSON {
		channels := agentMeta.Channels
		if channels == nil {
			channels = []string{}
		}
		return writeJSON(os.Stdout, channels)
	}

	if len(agentMeta.Channels) == 0 {
		return writeStdoutLine("No channel memberships.")
	}

	for _, ch := range agentMeta.Channels {
		if err := writeStdout("#%s\n", ch); err != nil {
			return err
		}
	}
	return nil
}

func printChannelUsage() {
	_ = writeStdoutLine("amq channel <command> [options]")
	_ = writeStdoutLine("")
	_ = writeStdoutLine("Commands:")
	_ = writeStdoutLine("  join   Join a channel")
	_ = writeStdoutLine("  leave  Leave a channel")
	_ = writeStdoutLine("  list   List channel memberships")
}
