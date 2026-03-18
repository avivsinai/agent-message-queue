package cli

import (
	"flag"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/resolve"
)

type resolveOutput struct {
	Input    string          `json:"input"`
	Parsed   resolveParsed   `json:"parsed"`
	Targets  []resolveTarget `json:"targets,omitempty"`
	Error    string          `json:"error,omitempty"`
	IsLocal  bool            `json:"is_local"`
	IsCross  bool            `json:"is_cross_project"`
	IsCrossS bool            `json:"is_cross_session"`
}

type resolveParsed struct {
	Kind    string `json:"kind"`
	Agent   string `json:"agent,omitempty"`
	Channel string `json:"channel,omitempty"`
	Session string `json:"session,omitempty"`
	Project string `json:"project,omitempty"`
}

type resolveTarget struct {
	Agent       string `json:"agent"`
	Session     string `json:"session"`
	SessionRoot string `json:"session_root"`
	Project     string `json:"project,omitempty"`
}

func runResolve(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	common := addCommonFlags(fs)

	usage := usageWithFlags(fs, "amq resolve <address> [--json]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return UsageError("address argument is required")
	}
	address := fs.Arg(0)

	root := resolveRoot(common.Root)

	// Parse the address
	ep, err := resolve.ParseAddress(address)
	if err != nil {
		out := resolveOutput{
			Input: address,
			Error: err.Error(),
		}
		if common.JSON {
			return writeJSON(os.Stdout, out)
		}
		return err
	}

	out := resolveOutput{
		Input: address,
		Parsed: resolveParsed{
			Kind:    ep.Kind,
			Agent:   ep.Agent,
			Channel: ep.Channel,
			Session: ep.Session,
			Project: ep.Project,
		},
		IsLocal:  ep.IsLocal(),
		IsCross:  ep.IsCrossProject(),
		IsCrossS: ep.IsCrossSession(),
	}

	// Build resolver context using the same resolution helpers as send.go.
	baseRoot := resolveBaseRootForFederation(root)
	projectDir := resolveProjectDir()

	resolver := resolve.NewResolver(root, baseRoot, projectDir)

	// Resolve
	targets, resolveErr := resolver.Resolve(ep)
	if resolveErr != nil {
		out.Error = resolveErr.Error()
		if common.JSON {
			return writeJSON(os.Stdout, out)
		}
		if err := writeStdout("Address: %s\n", address); err != nil {
			return err
		}
		if err := writeStdout("Parsed:  %s (kind=%s)\n", ep.String(), ep.Kind); err != nil {
			return err
		}
		return resolveErr
	}

	out.Targets = make([]resolveTarget, 0, len(targets))
	for _, t := range targets {
		out.Targets = append(out.Targets, resolveTarget{
			Agent:       t.Agent,
			Session:     t.Session,
			SessionRoot: t.SessionRoot,
			Project:     t.Project,
		})
	}

	if common.JSON {
		return writeJSON(os.Stdout, out)
	}

	// Human-readable output
	if err := writeStdout("Address: %s\n", address); err != nil {
		return err
	}
	if err := writeStdout("Parsed:  %s (kind=%s)\n", ep.String(), ep.Kind); err != nil {
		return err
	}
	if ep.IsLocal() {
		if err := writeStdout("Scope:   local\n"); err != nil {
			return err
		}
	} else if ep.IsCrossProject() {
		if err := writeStdout("Scope:   cross-project (%s)\n", ep.Project); err != nil {
			return err
		}
	} else if ep.IsCrossSession() {
		if err := writeStdout("Scope:   cross-session (%s)\n", ep.Session); err != nil {
			return err
		}
	}
	if err := writeStdout("Targets: %d\n", len(targets)); err != nil {
		return err
	}
	for _, t := range targets {
		proj := ""
		if t.Project != "" {
			proj = " (project: " + t.Project + ")"
		}
		if err := writeStdout("  %s @ %s  [%s]%s\n", t.Agent, t.Session, t.SessionRoot, proj); err != nil {
			return err
		}
	}
	return nil
}
