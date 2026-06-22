package cli

import (
	"flag"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

type presenceListItem struct {
	Schema             int    `json:"schema,omitempty"`
	Handle             string `json:"handle"`
	Status             string `json:"status"`
	LastSeen           string `json:"last_seen,omitempty"`
	Note               string `json:"note,omitempty"`
	Kind               string `json:"kind"`
	PresenceApplicable bool   `json:"presence_applicable"`
}

func runPresence(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printGroupUsage(findCommand("presence"))
	}
	switch args[0] {
	case "set":
		return runPresenceSet(args[1:])
	case "list":
		return runPresenceList(args[1:])
	default:
		return formatUnknownSubcommand("presence", args[0])
	}
}

func runPresenceSet(args []string) error {
	fs := flag.NewFlagSet("presence set", flag.ContinueOnError)
	common := addCommonFlags(fs)
	statusFlag := fs.String("status", "", "Status string")
	noteFlag := fs.String("note", "", "Optional note")
	usage := usageWithFlags(fs, "amq presence set --me <agent> --status <status> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
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

	// Validate handle against config.json
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	status := strings.TrimSpace(*statusFlag)
	if status == "" {
		return UsageError("--status is required")
	}
	p := presence.New(common.Me, status, strings.TrimSpace(*noteFlag), time.Now())
	if err := presence.Write(root, p); err != nil {
		return err
	}
	if common.JSON {
		return writeJSON(os.Stdout, p)
	}
	if err := writeStdout("Presence updated for %s\n", common.Me); err != nil {
		return err
	}
	return nil
}

func runPresenceList(args []string) error {
	fs := flag.NewFlagSet("presence list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(fs, "amq presence list [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	root := resolveRoot(common.Root)

	agents, err := loadKnownAgents(root, false)
	if err != nil {
		return err
	}
	if agents == nil {
		var listErr error
		agents, listErr = fsq.ListAgents(root)
		if listErr != nil && !os.IsNotExist(listErr) {
			return listErr
		}
	}

	items := make([]presenceListItem, 0, len(agents))
	for _, raw := range agents {
		agent, err := normalizeHandle(raw)
		if err != nil {
			if err := writeStderr("warning: skipping invalid handle %s: %v\n", raw, err); err != nil {
				return err
			}
			continue
		}

		if agent == reservedHumanHandle {
			p, err := presence.Read(root, agent)
			if err != nil {
				if os.IsNotExist(err) {
					items = append(items, presenceListItem{
						Handle:             reservedHumanHandle,
						Status:             "human",
						Kind:               "human",
						PresenceApplicable: false,
					})
					continue
				}
				return err
			}
			items = append(items, presenceListItemFromPresence(p, "human", false))
			continue
		}

		p, err := presence.Read(root, agent)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		items = append(items, presenceListItemFromPresence(p, "agent", true))
	}

	if common.JSON {
		return writeJSON(os.Stdout, items)
	}
	if len(items) == 0 {
		if err := writeStdoutLine("No presence data."); err != nil {
			return err
		}
		return nil
	}
	for _, item := range items {
		if !item.PresenceApplicable {
			if err := writeStdout("%s  human\n", item.Handle); err != nil {
				return err
			}
			if item.Note != "" {
				if err := writeStdout("  %s\n", item.Note); err != nil {
					return err
				}
			}
			continue
		}
		if err := writeStdout("%s  %s  %s\n", item.Handle, item.Status, item.LastSeen); err != nil {
			return err
		}
		if item.Note != "" {
			if err := writeStdout("  %s\n", item.Note); err != nil {
				return err
			}
		}
	}
	return nil
}

func presenceListItemFromPresence(p presence.Presence, kind string, presenceApplicable bool) presenceListItem {
	status := p.Status
	if status == "" && kind == "human" {
		status = "human"
	}
	return presenceListItem{
		Schema:             p.Schema,
		Handle:             p.Handle,
		Status:             status,
		LastSeen:           p.LastSeen,
		Note:               p.Note,
		Kind:               kind,
		PresenceApplicable: presenceApplicable,
	}
}
