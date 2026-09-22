package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Only a completed lookup may report absence. Filesystem failures retain their
// original cause without being translated into a missing message by callers.
var errMessageNotFound = errors.New("message not found")

func requireMailboxDeliveryRoot(root *fsq.DeliveryRoot, displayRoot, me string) error {
	for _, dir := range []string{
		filepath.Join("agents", me, "inbox", "new"),
		filepath.Join("agents", me, "inbox", "cur"),
		filepath.Join("agents", me, "dlq", "new"),
	} {
		info, err := root.Stat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return NotFoundError("mailbox for %q is missing at root %s (missing %s); check AM_ROOT or use --session <name>", me, displayRoot, root.DisplayPath(dir))
			}
			return err
		}
		if !info.IsDir() {
			return NotFoundError("mailbox path for %q is not a directory: %s", me, root.DisplayPath(dir))
		}
	}
	return nil
}

func deliveryAgentExists(root *fsq.DeliveryRoot, agent string) bool {
	info, err := root.Stat(filepath.Join("agents", agent))
	return err == nil && info.IsDir()
}

func findMessageDeliveryRoot(root *fsq.DeliveryRoot, agent, filename string, includeSent bool) (string, string, error) {
	if err := fsq.ValidateMessageFilename(filename); err != nil {
		return "", "", err
	}
	candidates := []struct {
		path string
		box  string
	}{
		{path: filepath.Join("agents", agent, "inbox", "new", filename), box: fsq.BoxNew},
		{path: filepath.Join("agents", agent, "inbox", "cur", filename), box: fsq.BoxCur},
	}
	if includeSent {
		candidates = append(candidates, struct {
			path string
			box  string
		}{path: filepath.Join("agents", agent, "outbox", "sent", filename), box: "sent"})
	}
	for _, candidate := range candidates {
		if _, err := root.Stat(candidate.path); err == nil {
			return candidate.path, candidate.box, nil
		} else if !os.IsNotExist(err) {
			return "", "", err
		}
	}

	// Bridge deliveries retain the message ID in the header but use a transfer
	// filename. Keep the direct filename lookup above for existing CLI callers.
	id := strings.TrimSuffix(filename, ".md")
	var matchedPath, matchedBox string
	for _, candidate := range candidates {
		dir := filepath.Dir(candidate.path)
		entries, err := root.ReadDir(dir)
		if err != nil {
			return "", "", fmt.Errorf("scan messages in %s: %w", dir, err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".md") || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			msg, err := readMessageDeliveryRoot(root, path)
			if err != nil {
				return "", "", fmt.Errorf("look up message ID in %s: %w", path, err)
			}
			if msg.Header.ID != id {
				continue
			}
			if matchedPath != "" {
				return "", "", fmt.Errorf("ambiguous message ID %q: matches %s and %s", id, matchedPath, path)
			}
			matchedPath, matchedBox = path, candidate.box
		}
	}
	if matchedPath != "" {
		return matchedPath, matchedBox, nil
	}
	return "", "", errMessageNotFound
}

func readMessageDeliveryRoot(root *fsq.DeliveryRoot, path string) (format.Message, error) {
	info, err := root.Stat(path)
	if err != nil {
		return format.Message{}, err
	}
	if info.Size() > format.MaxMessageSize {
		return format.Message{}, fmt.Errorf("%w: %d bytes", format.ErrMessageTooLarge, info.Size())
	}
	data, err := root.ReadRegularNoFollow(path)
	if err != nil {
		return format.Message{}, err
	}
	if len(data) > format.MaxMessageSize {
		return format.Message{}, fmt.Errorf("%w: %d bytes", format.ErrMessageTooLarge, len(data))
	}
	return format.ParseMessage(data)
}
