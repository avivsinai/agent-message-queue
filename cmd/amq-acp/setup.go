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
  amq-acp setup --machine [--out <file>]
  amq-acp setup [--out <file>]
  amq-acp setup --session <name> [--out <file>]
  amq-acp setup --helper [--out <file>]

With --machine it writes the machine agent, the one Buzz agent of this
machine: each session that runs /amq-remote then gets a private Buzz channel
that this agent makes, with the owner added. Import it once per machine. Its
harness is custom_harnesses/amq_remote_machine.json, whose env keeps every
worker running (BUZZ_ACP_LAZY_POOL=false) and makes every owner message in
its channels a prompt (BUZZ_ACP_SUBSCRIBE=all, BUZZ_ACP_KINDS=9). The import
file is "AMQ Remote @ <machine>.agent.json".

Without --machine the harness is
~/Library/Application Support/xyz.block.buzz.app/custom_harnesses/amq_remote.json
with mode 0600. Its env is only AMQ_ACP_REMOTE=binding. A file this command did
not write is left unchanged.

The import file is "AMQ Remote.agent.json" in the current directory, or --out.
With --session it is "AMQ <name>.agent.json", an agent for that one session.
With --helper it is "AMQ Remote helper.agent.json", the per-machine helper
that asks Buzz Desktop to create each new session's agent. Import the file in
Buzz Desktop, then Start; turn on Auto-start. A Desktop that is open sees a
new harness after Settings, Agents, Check again.
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
	// machineHarnessID, machineLine and machineParallelism make the machine
	// agent (internal/acp machine.go): one agent per machine with a private
	// channel per session. Its workers run eagerly, so parallelism bounds
	// the concurrent turns across all sessions.
	machineHarnessID   = "amq_remote_machine"
	machineLine        = "AMQ Remote machine"
	machineParallelism = 4
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
	machine := flags.Bool("machine", false, "write the machine agent, which gives each session a private Buzz channel")
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
	if *machine && (*helper || name != "") {
		fmt.Fprintln(os.Stderr, "amq-acp setup: --machine serves every session; give it without --session or --helper")
		return exitUsage
	}
	snapshotPath := *out
	if snapshotPath == "" {
		switch {
		case *machine:
			snapshotPath = machineAgentName() + ".agent.json"
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
	harness := remoteHarness(command)
	body, err := remoteSnapshotBody(name, *helper)
	if *machine {
		harness = machineHarness(command)
		body, err = snapshotBody(machineAgentName(), machineLine, machineHarnessID, machineParallelism)
	}
	harnessPath := filepath.Join(buzzHarnessDir(home), harness.ID+".json")
	if err := writeHarness(harnessPath, harness); err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	if err := writeSnapshot(snapshotPath, body); err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	fmt.Print(setupSteps(snapshotPath))
	if *machine {
		fmt.Println("Do this once per machine: each session that runs /amq-remote then gets its own private channel in Buzz.")
	}
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
	return snapshotBody(agentName, systemPrompt, remoteHarnessID, 1)
}

// machineHarness is the machine agent's harness. Buzz applies its env to
// the parent buzz-acp: workers start with Desktop and never sleep, and every
// ordinary message in a channel the agent belongs to is a prompt, not only
// a mention.
func machineHarness(command string) buzzHarness {
	return buzzHarness{
		ID:      machineHarnessID,
		Label:   "AMQ Remote machine",
		Command: command,
		Args:    []string{},
		Env: map[string]string{
			"AMQ_ACP_REMOTE":     "binding",
			"BUZZ_ACP_LAZY_POOL": "false",
			"BUZZ_ACP_SUBSCRIBE": "all",
			"BUZZ_ACP_KINDS":     "9",
		},
		InstallInstructionsURL: harnessDocsURL,
		InstallHint:            harnessHintPrefix + " AMQ Remote machine gives each session a private channel.",
	}
}

// machineAgentName is "AMQ Remote @ <machine>", or "AMQ Remote machine"
// when the host name is unknown.
func machineAgentName() string {
	if machine := binding.Machine(); machine != "" {
		return "AMQ Remote @ " + machine
	}
	return "AMQ Remote machine"
}

// snapshotBody is one Import file: an owner-only agent on runtime.
func snapshotBody(agentName, systemPrompt, runtime string, parallelism int) ([]byte, error) {
	body, err := json.MarshalIndent(agentSnapshot{
		Format:  "buzz-agent-snapshot",
		Version: 1,
		Definition: agentSnapshotDefinition{
			Name:         agentName,
			SystemPrompt: systemPrompt,
			Runtime:      runtime,
			Model:        remoteModelID,
			Parallelism:  parallelism,
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
