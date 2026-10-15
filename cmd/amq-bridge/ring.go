package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge/ring"
)

func runRing(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("ring adapter is required (grokbot, slack)")
	}
	adapter := args[0]
	fs := flag.NewFlagSet("amq-bridge ring", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keyFile := fs.String("key-file", "", "mode-0600 webhook credential file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*keyFile) == "" {
		return usageError("--key-file is required")
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return usageError("handle and pending are required")
	}
	handle := rest[0]
	pending := rest[1]
	switch adapter {
	case "grokbot":
		return ring.RingGrokbot(*keyFile, handle, pending, nil, os.Stderr)
	case "slack":
		return ring.RingSlack(*keyFile, handle, pending, nil, os.Stderr)
	default:
		return fmt.Errorf("unknown ring adapter %q", adapter)
	}
}
