package ring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const maxKeyFileBytes = 16 * 1024

type grokbotFile struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

type slackFile struct {
	URL string `json:"url"`
}

func readKeyFile(path string, dest any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("ring key file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("ring key file %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ring key file %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("ring key file %s mode is %o, want 0600", path, got)
	}
	file, _, err := fsq.OpenRegularNoFollow(path)
	if err != nil {
		return fmt.Errorf("open ring key file: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil {
		return fmt.Errorf("read ring key file: %w", err)
	}
	if len(data) > maxKeyFileBytes {
		return fmt.Errorf("ring key file exceeds %d bytes", maxKeyFileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("parse ring key file: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("ring key file has trailing JSON")
		}
		return fmt.Errorf("parse ring key file trailer: %w", err)
	}
	return nil
}

func parseWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("ring webhook url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("ring webhook url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("ring webhook url scheme %q is unsupported", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("ring webhook url host is required")
	}
	return raw, nil
}
