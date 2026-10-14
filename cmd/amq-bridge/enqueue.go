package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3"
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

	result, err := enqueueMessage(cfg, *destAlias, message)
	if err != nil {
		return err
	}
	if result.Path != "" {
		fmt.Println(result.Path)
	}
	return nil
}

func enqueueMessage(cfg bridge.EnqueueConfig, destAlias string, message []byte) (bridge.EnqueueResult, error) {
	dest, destErr := xferv3.ParseDest(destAlias)
	if destErr == nil {
		_, peerErr := xferv3.LoadPeer(cfg.Root, dest.Host())
		switch {
		case peerErr == nil:
			if err := putPinned(cfg, dest, message); err != nil {
				return bridge.EnqueueResult{}, err
			}
			if _, _, err := bridge.ParseAlias(destAlias); err != nil {
				return bridge.EnqueueResult{}, nil
			}
			if _, ok := cfg.AllowedDestSet()[destAlias]; !ok {
				return bridge.EnqueueResult{}, nil
			}
			return bridge.Enqueue(cfg, destAlias, message)
		case !errors.Is(peerErr, xferv3.ErrNoPeer):
			return bridge.EnqueueResult{}, peerErr
		}
	}
	return bridge.Enqueue(cfg, destAlias, message)
}

func putPinned(cfg bridge.EnqueueConfig, dest xferv3.Dest, message []byte) error {
	hostID, err := bridge.LoadHostID(cfg.Root)
	if err != nil {
		return err
	}
	if hostID != cfg.SourceHost {
		return fmt.Errorf("host-id %q does not match source host %q", hostID, cfg.SourceHost)
	}
	key, err := bridge.LoadIdentity(cfg.Root)
	if err != nil {
		return err
	}
	parsed, err := format.ParseMessage(message)
	if err != nil {
		return fmt.Errorf("parse message: %w", err)
	}
	source, err := xferv3.ParseHost(cfg.SourceHost)
	if err != nil {
		return err
	}
	handle, err := xferv3.ParseHandle(cfg.SourceHandle)
	if err != nil {
		return err
	}
	messageID, err := xferv3.ParseMessageID(parsed.Header.ID)
	if err != nil {
		return err
	}
	threadID, err := xferv3.ParseThreadID(parsed.Header.Thread)
	if err != nil {
		return err
	}
	rec, err := xferv3.NewRecord(source, handle, dest, messageID, threadID, message)
	if err != nil {
		return err
	}
	_, err = xferv3.PutTx(cfg.Root, key.Private, rec, message)
	return err
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
