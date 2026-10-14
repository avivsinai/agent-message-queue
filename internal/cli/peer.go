package cli

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var lookAmqBridge = findAmqBridge

func runPeer(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printGroupUsage(findCommand("peer"))
	}
	switch args[0] {
	case "pair":
		return runPeerPair(args[1:])
	default:
		return formatUnknownSubcommand("peer", args[0])
	}
}

func runPeerPair(args []string) error {
	fs := flag.NewFlagSet("peer pair", flag.ContinueOnError)
	common := addCommonFlags(fs)
	fromLine := fs.String("from-line", "", "pairing line from amq-bridge peer line")
	confirm := fs.String("confirm-fingerprint", "", "26-character fingerprint that must match before the pin is written")
	allowIn := fs.String("allow-in", "", "comma-separated inbound handles")
	usage := usageWithFlags(fs, "amq peer pair [line] [options]",
		"Exec amq-bridge peer add, then print the local pairing line.",
		"Writes nothing until --confirm-fingerprint matches the peer line.")
	if handled, err := parseFlagsAllowPositionals(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	line := strings.TrimSpace(*fromLine)
	if line == "" && fs.NArg() == 1 {
		line = strings.TrimSpace(fs.Arg(0))
	}
	if line == "" {
		return UsageError("peer pair requires a pairing line")
	}
	if fs.NArg() > 1 || (fs.NArg() == 1 && *fromLine != "") {
		return UsageError("peer pair accepts one pairing line")
	}
	if strings.TrimSpace(*confirm) == "" {
		return UsageError("peer pair writes nothing until --confirm-fingerprint is set")
	}
	root := resolveRoot(common.Root)
	if root == "" {
		return UsageError("peer pair requires a queue root")
	}
	bridgeBin, err := lookAmqBridge()
	if err != nil {
		return err
	}
	addArgs := []string{"peer", "add", "--root", root, "--from-line", line, "--confirm-fingerprint", *confirm}
	if strings.TrimSpace(*allowIn) != "" {
		addArgs = append(addArgs, "--allow-in", *allowIn)
	}
	add := exec.Command(bridgeBin, addArgs...)
	add.Stderr = os.Stderr
	add.Stdout = os.Stdout
	if err := add.Run(); err != nil {
		return fmt.Errorf("amq-bridge peer add: %w", err)
	}
	lineCmd := exec.Command(bridgeBin, "peer", "line", "--root", root)
	var out bytes.Buffer
	lineCmd.Stdout = &out
	lineCmd.Stderr = os.Stderr
	if err := lineCmd.Run(); err != nil {
		return fmt.Errorf("amq-bridge peer line: %w", err)
	}
	fmt.Print(out.String())
	return nil
}

func findAmqBridge() (string, error) {
	if path, err := exec.LookPath("amq-bridge"); err == nil {
		return path, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("amq-bridge is not on PATH")
	}
	sibling := filepath.Join(filepath.Dir(exe), "amq-bridge")
	info, err := os.Stat(sibling)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("amq-bridge is not on PATH")
	}
	return sibling, nil
}
