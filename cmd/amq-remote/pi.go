package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/integrations/pi"
	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

const piUsage = "Usage: amq-remote pi bridge-path"

// piSubcommand hands out the pi extension built into this binary. A pi host
// loads the file it prints, so the bridge always matches the amq-remote on
// the machine.
func piSubcommand(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "bridge-path" {
		say(stderr, "%s", piUsage)
		return protocol.ExitUsage
	}
	home, err := amqhome.EnsureDir()
	if err != nil {
		say(stderr, "pi bridge-path: %v", err)
		return 1
	}
	path, err := writePiBridge(home)
	if err != nil {
		say(stderr, "pi bridge-path: %v", err)
		return 1
	}
	say(stdout, "%s", path)
	return 0
}

// writePiBridge writes the built-in extension to <amqHome>/pi/amq-bridge.ts
// when the file there differs, and returns its path. pi reads the file once
// at start, so a replace under a running pi changes nothing until it restarts.
func writePiBridge(amqHome string) (string, error) {
	dir := filepath.Join(amqHome, "pi")
	path := filepath.Join(dir, "amq-bridge.ts")
	// pi runs this file as code: write it only into a plain directory.
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if info, err := os.Lstat(dir); err != nil {
		return "", err
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%s is not a plain directory", dir)
	}
	// Only a plain file is kept as it is; a symlink is replaced by one, so
	// the path pi runs never resolves elsewhere.
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, pibridge.Source) {
			return path, nil
		}
	}
	return fsq.WriteStateFile(amqHome, filepath.Join("pi", "amq-bridge.ts"), pibridge.Source, 0o600)
}
