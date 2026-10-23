package cli

import (
	"flag"
	"os"
	"strings"

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
	// PresenceError is set when presence.json exists but cannot be read;
	// Status is then "unknown".
	PresenceError string `json:"presence_error,omitempty"`
	agentRegistrationJSON
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
	p, err := presence.SetStatus(root, common.Me, status, strings.TrimSpace(*noteFlag))
	if err != nil {
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

	// One delivery root: every mailbox there, whether or not it has
	// presence, plus the reserved human handle.
	scope := inventoryDeliveryRoot(root, classifyRoot(root))
	if scope.Err != nil {
		return scope.Err
	}
	for _, skipped := range scope.Skipped {
		if err := writeStderr("warning: skipping invalid handle %s: %s\n", skipped.Name, skipped.Reason); err != nil {
			return err
		}
	}
	rows := scope.Agents
	if !inventoryHasHandle(rows, reservedHumanHandle) && scope.Declarations.anyPresent() {
		rows = append(rows, inventoryAgent{
			Handle:       reservedHumanHandle,
			Registration: classifyAgentAt(scope.Declarations, root, reservedHumanHandle),
		})
	}

	items := make([]presenceListItem, 0, len(rows))
	for _, row := range rows {
		human := row.Registration.Kind == "human"
		item := presenceListItem{
			Handle:             row.Handle,
			Status:             "unknown",
			Kind:               row.Registration.Kind,
			PresenceApplicable: !human,
		}
		if row.Presence != nil {
			item = presenceListItemFromPresence(*row.Presence, row.Registration.Kind, !human)
		} else if human {
			item.Status = "human"
		}
		if row.PresenceErr != nil {
			item.PresenceError = row.PresenceErr.Error()
		}
		item.agentRegistrationJSON = row.Registration.json()
		items = append(items, item)
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
		line := item.Handle + "  " + item.Status
		if item.LastSeen != "" {
			line += "  " + item.LastSeen
		}
		if item.RegistrationStatus != registrationDeclared {
			line += "  [" + string(item.RegistrationStatus) + "]"
		}
		if err := writeStdoutLine(line); err != nil {
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
