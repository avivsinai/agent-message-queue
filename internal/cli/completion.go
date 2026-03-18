package cli

import (
	"fmt"
	"os"
	"strings"
)

func runCompletion(args []string) error {
	if len(args) == 0 {
		return UsageError("usage: amq completion <bash|zsh|fish>")
	}
	shell := args[0]
	switch shell {
	case "bash":
		return generateBashCompletion()
	case "zsh":
		return generateZshCompletion()
	case "fish":
		return generateFishCompletion()
	default:
		return UsageError("unsupported shell: %s (supported: bash, zsh, fish)", shell)
	}
}

func generateBashCompletion() error {
	// Build top-level command names from the registry.
	// "completion" is already in the registry; also include "help".
	topNames := commandNames()
	allNames := append(topNames, "help")
	topList := strings.Join(allNames, " ")

	// Build case branches for subcommand groups.
	var caseBranches strings.Builder
	for _, cmd := range commands {
		if len(cmd.Children) == 0 {
			continue
		}
		names := childNames(&cmd)
		fmt.Fprintf(&caseBranches, "            %s) COMPREPLY=($(compgen -W \"%s\" -- \"${cur}\")) ;;\n",
			cmd.Name, strings.Join(names, " "))
	}

	script := fmt.Sprintf(`#!/usr/bin/env bash
# amq shell completions for bash
# Usage: eval "$(amq completion bash)"

_amq() {
    local cur prev
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"

    local commands="%s"

    # Complete subcommands for groups
    case "${prev}" in
%s            *) ;;
    esac

    # If we already set completions (subcommand case), return
    if [ ${#COMPREPLY[@]} -gt 0 ]; then
        return 0
    fi

    # Default: complete top-level commands at position 1
    if [ "${COMP_CWORD}" -eq 1 ]; then
        COMPREPLY=($(compgen -W "${commands}" -- "${cur}"))
    fi
}
complete -F _amq amq
`, topList, caseBranches.String())

	_, err := fmt.Fprint(os.Stdout, script)
	return err
}

func generateZshCompletion() error {
	// Build top-level command descriptions.
	var cmdEntries strings.Builder
	for _, cmd := range commands {
		// Escape single quotes in summary.
		summary := strings.ReplaceAll(cmd.Summary, "'", "'\\''")
		fmt.Fprintf(&cmdEntries, "        '%s:%s'\n", cmd.Name, summary)
	}
	// "completion" is already in the registry; also include "help".
	fmt.Fprintf(&cmdEntries, "        'help:Show help for a command'\n")

	// Build subcommand cases for groups.
	var subcmdCases strings.Builder
	for _, cmd := range commands {
		if len(cmd.Children) == 0 {
			continue
		}
		fmt.Fprintf(&subcmdCases, "        %s)\n", cmd.Name)
		fmt.Fprintf(&subcmdCases, "            local -a subcmds\n")
		fmt.Fprintf(&subcmdCases, "            subcmds=(\n")
		for _, child := range cmd.Children {
			summary := strings.ReplaceAll(child.Summary, "'", "'\\''")
			fmt.Fprintf(&subcmdCases, "                '%s:%s'\n", child.Name, summary)
		}
		fmt.Fprintf(&subcmdCases, "            )\n")
		fmt.Fprintf(&subcmdCases, "            _describe 'subcommand' subcmds\n")
		fmt.Fprintf(&subcmdCases, "            ;;\n")
	}

	script := fmt.Sprintf(`#compdef amq
# amq shell completions for zsh
# Usage: amq completion zsh > "${fpath[1]}/_amq" && compinit

_amq() {
    local -a commands
    commands=(
%s    )

    if (( CURRENT == 2 )); then
        _describe 'command' commands
        return
    fi

    case "${words[2]}" in
%s        *)
            ;;
    esac
}
_amq "$@"
`, cmdEntries.String(), subcmdCases.String())

	_, err := fmt.Fprint(os.Stdout, script)
	return err
}

func generateFishCompletion() error {
	var buf strings.Builder

	buf.WriteString("# amq shell completions for fish\n")
	buf.WriteString("# Usage: amq completion fish | source\n")
	buf.WriteString("#    or: amq completion fish > ~/.config/fish/completions/amq.fish\n\n")

	// Top-level commands.
	for _, cmd := range commands {
		desc := strings.ReplaceAll(cmd.Summary, "'", "\\'")
		fmt.Fprintf(&buf, "complete -c amq -n '__fish_use_subcommand' -a %s -d '%s'\n",
			cmd.Name, desc)
	}
	// "completion" is already in the registry; also include "help".
	fmt.Fprintf(&buf, "complete -c amq -n '__fish_use_subcommand' -a help -d 'Show help for a command'\n")

	// Subcommands for groups.
	for _, cmd := range commands {
		if len(cmd.Children) == 0 {
			continue
		}
		buf.WriteString("\n")
		for _, child := range cmd.Children {
			desc := strings.ReplaceAll(child.Summary, "'", "\\'")
			fmt.Fprintf(&buf, "complete -c amq -n '__fish_seen_subcommand_from %s' -a %s -d '%s'\n",
				cmd.Name, child.Name, desc)
		}
	}

	_, err := fmt.Fprint(os.Stdout, buf.String())
	return err
}
