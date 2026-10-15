package ring

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type grokbotBody struct {
	Handle  string `json:"handle"`
	Pending int    `json:"pending"`
}

func RingGrokbot(keyFile, handle, pendingText string, client Poster, stderr io.Writer) error {
	pending, err := parseRingArgs(handle, pendingText)
	if err != nil {
		return err
	}
	var creds grokbotFile
	if err := readKeyFile(keyFile, &creds); err != nil {
		return err
	}
	if strings.TrimSpace(creds.Key) == "" {
		return fmt.Errorf("ring key file key is required")
	}
	rawURL, err := parseWebhookURL(creds.URL)
	if err != nil {
		return err
	}
	body, err := json.Marshal(grokbotBody{Handle: handle, Pending: pending})
	if err != nil {
		return err
	}
	if err := postJSON(client, rawURL, creds.Key, body); err != nil {
		writeProgress(stderr, ProgressDeferred)
		return err
	}
	writeProgress(stderr, ProgressAccepted)
	return nil
}

func parseRingArgs(handle, pendingText string) (int, error) {
	if err := fsq.ValidateHandle(handle); err != nil {
		return 0, fmt.Errorf("ring handle: %w", err)
	}
	pending, err := strconv.Atoi(pendingText)
	if err != nil || pending < 0 {
		return 0, fmt.Errorf("ring pending %q must be a non-negative integer", pendingText)
	}
	return pending, nil
}
