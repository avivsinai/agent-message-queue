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
)

const setupUsage = `amq-acp setup writes the AMQ Remote Buzz harness and the Desktop import file.

Usage:
  amq-acp setup [--out <file>]

The harness is ~/Library/Application Support/xyz.block.buzz.app/custom_harnesses/amq_remote.json
with mode 0600. Its env is only AMQ_ACP_REMOTE=binding. A file this command did
not write is left unchanged.

The import file is "AMQ Remote.agent.json" in the current directory, or --out.
Import it in Buzz Desktop, then Start. Then run /amq-remote in a session.
`

const (
	remoteHarnessID = "amq_remote"
	remoteModelID   = "amq-remote"
	// ownerOnlyRespondTo is RespondTo::OwnerOnly in Buzz
	// (managed_agents/types/tests.rs: respond_to_serde_is_kebab_case).
	ownerOnlyRespondTo = "owner-only"
	agentSnapshotName  = "AMQ Remote.agent.json"
)

type agentSnapshot struct {
	Format     string                  `json:"format"`
	Version    int                     `json:"version"`
	Definition agentSnapshotDefinition `json:"definition"`
	Profile    agentSnapshotProfile    `json:"profile"`
	Memory     agentSnapshotMemory     `json:"memory"`
}

type agentSnapshotDefinition struct {
	Name        string `json:"name"`
	Runtime     string `json:"runtime"`
	Model       string `json:"model"`
	Parallelism int    `json:"parallelism"`
	RespondTo   string `json:"respondTo"`
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
	out := flags.String("out", "", "Import file path (default: ./AMQ Remote.agent.json)")
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
	snapshotPath := *out
	if snapshotPath == "" {
		snapshotPath = agentSnapshotName
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
	body, err := remoteSnapshotBody()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	if err := writeSnapshot(snapshotPath, body); err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp setup:", err)
		return exitGeneral
	}
	fmt.Printf("In Buzz Desktop: Agents, then + then Import, pick %s, then Start. Then run /amq-remote in a session.\n", snapshotPath)
	return 0
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

func remoteSnapshotBody() ([]byte, error) {
	body, err := json.MarshalIndent(agentSnapshot{
		Format:  "buzz-agent-snapshot",
		Version: 1,
		Definition: agentSnapshotDefinition{
			Name:        "AMQ Remote",
			Runtime:     remoteHarnessID,
			Model:       remoteModelID,
			Parallelism: 1,
			RespondTo:   ownerOnlyRespondTo,
		},
		Profile: agentSnapshotProfile{DisplayName: "AMQ Remote"},
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
