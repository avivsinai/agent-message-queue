package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/format"
)

func runEnqueue(args []string) error {
	if err := rejectPromptControlledEnqueueFlags(args); err != nil {
		return err
	}

	fs := flag.NewFlagSet("amq-bridge enqueue", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "Private mode-0600 enqueue config JSON")
	destAlias := fs.String("dest-alias", "", "Receiver-owned destination alias host/agent")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return usageError("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*configPath) == "" {
		return usageError("--config is required")
	}
	if strings.TrimSpace(*destAlias) == "" {
		return usageError("--dest-alias is required")
	}

	cfg, err := bridge.LoadEnqueueConfig(*configPath)
	if err != nil {
		return err
	}

	message, err := io.ReadAll(io.LimitReader(os.Stdin, format.MaxMessageSize+1))
	if err != nil {
		return fmt.Errorf("read message from stdin: %w", err)
	}
	if len(message) > format.MaxMessageSize {
		return usageError("message exceeds maximum size")
	}

	result, err := bridge.Enqueue(cfg, *destAlias, message)
	if err != nil {
		return err
	}
	fmt.Println(result.Path)
	return nil
}

func rejectPromptControlledEnqueueFlags(args []string) error {
	for _, arg := range args {
		name := arg
		if i := strings.IndexByte(arg, '='); i >= 0 {
			name = arg[:i]
		}
		switch name {
		case "--root", "-root":
			return usageError("--root is not allowed with enqueue; use enqueue config root")
		case "--rendezvous":
			return usageError("--rendezvous is not allowed with enqueue")
		case "--me":
			return usageError("--me is not allowed with enqueue")
		case "--spool":
			return usageError("--spool is not allowed with enqueue")
		}
	}
	return nil
}

func usageError(format string, args ...any) error {
	return fmt.Errorf("%s", fmt.Sprintf(format, args...))
}
