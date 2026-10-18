package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

const setupUsage = `amq-acp setup writes the AMQ Remote Buzz harness and the Desktop import file.

Usage:
  amq-acp setup [--out <file>]
  amq-acp setup --session <name> [--out <file>]
  amq-acp setup --helper [--out <file>]

The harness is ~/Library/Application Support/xyz.block.buzz.app/custom_harnesses/amq_remote.json
with mode 0600. Its env is only AMQ_ACP_REMOTE=binding. A file this command did
not write is left unchanged.

The import file is "AMQ Remote.agent.json" in the current directory, or --out.
With --session it is "AMQ <name>.agent.json", an agent for that one session.
With --helper it is "AMQ Remote helper.agent.json", the per-machine helper
that asks Buzz Desktop to create each new session's agent; import it once per
machine. Import the file in Buzz Desktop, then Start; turn on Auto-start. A
Desktop that is open sees a new harness after Settings, Agents, Check again.
`

const (
	remoteHarnessID = "amq_remote"
	remoteModelID   = "amq-remote"
	// ownerOnlyRespondTo is RespondTo::OwnerOnly in Buzz
	// (managed_agents/types/tests.rs: respond_to_serde_is_kebab_case).
	ownerOnlyRespondTo = "owner-only"
	agentSnapshotName  = "AMQ Remote.agent.json"
	helperSnapshotName = "AMQ Remote helper.agent.json"
	// helperLine makes an imported agent the per-machine helper
	// (internal/acp lifecycle.go).
	helperLine = "AMQ Remote helper"
)

type agentSnapshot struct {
	Format     string                  `json:"format"`
	Version    int                     `json:"version"`
	Definition agentSnapshotDefinition `json:"definition"`
	Profile    agentSnapshotProfile    `json:"profile"`
	Memory     agentSnapshotMemory     `json:"memory"`
}

type agentSnapshotDefinition struct {
	Name         string `json:"name"`
	SystemPrompt string `json:"systemPrompt,omitempty"`
	Runtime      string `json:"runtime"`
	Model        string `json:"model"`
	Parallelism  int    `json:"parallelism"`
	RespondTo    string `json:"respondTo"`
}

type agentSnapshotProfile struct {
	DisplayName string `json:"displayName"`
}

type agentSnapshotMemory struct {
	Level string `json:"level"`
}

func runSetup(args []string) int {
	flags := flag.NewFlagSet("amq-acp setup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	out := flags.String("out", "", "Import file path (default: ./AMQ Remote.agent.json, or ./AMQ <session>.agent.json)")
	session := flags.String("session", "", "binding name from amq-remote attach; the agent is \"AMQ: <session>\" and drives only that session")
	helper := flags.Bool("helper", false, "write the per-machine helper, which asks Buzz Desktop to create each new session's agent")
	flags.Usage = func() { fmt.Fprint(os.Stderr, setupUsage) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "amq-acp setup: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return exitUsage
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	command, err := currentExecutable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	name := strings.TrimSpace(*session)
	if name != "" {
		if err := binding.ValidName(name); err != nil {
			fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
			return exitUsage
		}
		if *helper {
			fmt.Fprintln(os.Stderr, "amq-acp setup: --helper serves no session; give --session or --helper, not both")
			return exitUsage
		}
	}
	snapshotPath := *out
	if snapshotPath == "" {
		switch {
		case *helper:
			snapshotPath = helperSnapshotName
		case name != "":
			snapshotPath = "AMQ " + name + ".agent.json"
		default:
			snapshotPath = agentSnapshotName
		}
	}
	snapshotPath, err = filepath.Abs(snapshotPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	harnessPath := filepath.Join(buzzHarnessDir(home), remoteHarnessID+".json")
	if err := writeHarness(harnessPath, remoteHarness(command)); err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	body, err := remoteSnapshotBody(name, *helper)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	if err := writeSnapshot(snapshotPath, body); err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	fmt.Print(setupSteps(snapshotPath))
	if *helper {
		fmt.Println("Do this once per machine: the helper then asks Buzz Desktop to create the agent of each new session.")
	}
	return 0
}

// setupSteps is what the owner does in Buzz Desktop. Desktop caches its
// harness list, so a Desktop that was open when the harness file appeared
// sees it only after a forced discovery: Settings, Agents, Check again (Buzz
// 0.5.26 HarnessesSettingsPanel). No restart is needed. The step is always
// printed: a file on disk does not show that Desktop has discovered it.
// The steps end at Desktop: /amq-remote runs setup, so they never send the
// owner back to it.
func setupSteps(snapshotPath string) string {
	return fmt.Sprintf("In Buzz Desktop: if AMQ Remote is new and Desktop was open, click Settings, then Agents, then Check again. "+
		"Then Agents, then + then Import, pick %s, then Start. Turn on Auto-start in the agent's menu so it starts with Desktop.\n", snapshotPath)
}

func remoteHarness(command string) buzzHarness {
	return buzzHarness{
		ID:                     remoteHarnessID,
		Label:                  "AMQ Remote",
		Command:                command,
		Args:                   []string{},
		Env:                    map[string]string{"AMQ_ACP_REMOTE": "binding"},
		InstallInstructionsURL: harnessDocsURL,
		InstallHint:            harnessHintPrefix + " AMQ Remote uses the binding file.",
	}
}

// remoteSnapshotBody is the Import file. With a session name the agent is
// "AMQ: <name>" and its system prompt line "AMQ Remote session: <name>" pins
// it to that one binding, so each session has its own Buzz agent. The model
// stays the plain amq-remote: the pin has one source. The helper is
// "AMQ Remote helper @ <machine>" with the line "AMQ Remote helper": it
// serves no session and creates the session agents. Without either the
// agent is unpinned.
func remoteSnapshotBody(session string, helper bool) ([]byte, error) {
	agentName, systemPrompt := "AMQ Remote", ""
	switch {
	case helper:
		agentName, systemPrompt = "AMQ Remote helper", helperLine
		if machine := binding.Machine(); machine != "" {
			agentName += " @ " + machine
		}
	case session != "":
		agentName, systemPrompt = "AMQ: "+session, "AMQ Remote session: "+session
	}
	body, err := json.MarshalIndent(agentSnapshot{
		Format:  "buzz-agent-snapshot",
		Version: 1,
		Definition: agentSnapshotDefinition{
			Name:         agentName,
			SystemPrompt: systemPrompt,
			Runtime:      remoteHarnessID,
			Model:        remoteModelID,
			Parallelism:  1,
			RespondTo:    ownerOnlyRespondTo,
		},
		Profile: agentSnapshotProfile{DisplayName: agentName},
		Memory:  agentSnapshotMemory{Level: "none"},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func writeSnapshot(path string, body []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite %s; it is a symlink", path)
		}
		existing, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, body) {
			return fmt.Errorf("refusing to overwrite %s; amq-acp did not write it", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".amq-agent-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
