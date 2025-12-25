package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type commonFlags struct {
	Root string
	Me   string
	JSON bool
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	flags := &commonFlags{}
	fs.StringVar(&flags.Root, "root", defaultRoot(), "Root directory for the queue")
	fs.StringVar(&flags.Me, "me", defaultMe(), "Agent handle (or AM_ME)")
	fs.BoolVar(&flags.JSON, "json", false, "Emit JSON output")
	return flags
}

func defaultRoot() string {
	if env := strings.TrimSpace(os.Getenv(envRoot)); env != "" {
		return env
	}
	return ".agent-mail"
}

func defaultMe() string {
	if env := strings.TrimSpace(os.Getenv(envMe)); env != "" {
		return env
	}
	return ""
}

func requireMe(handle string) error {
	if strings.TrimSpace(handle) == "" {
		return errors.New("--me is required (or set AM_ME)")
	}
	return nil
}

func normalizeHandle(raw string) (string, error) {
	handle := strings.TrimSpace(raw)
	if handle == "" {
		return "", errors.New("agent handle cannot be empty")
	}
	if strings.ContainsAny(handle, "/\\") {
		return "", fmt.Errorf("invalid handle (slashes not allowed): %s", handle)
	}
	normalized := strings.ToLower(handle)
	for _, r := range normalized {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' {
			continue
		}
		return "", fmt.Errorf("invalid handle (allowed: a-z, 0-9, -, _): %s", handle)
	}
	return normalized, nil
}

func parseHandles(raw string) ([]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		handle, err := normalizeHandle(part)
		if err != nil {
			return nil, err
		}
		out = append(out, handle)
	}
	return out, nil
}

func splitRecipients(raw string) ([]string, error) {
	out, err := parseHandles(raw)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("--to is required")
	}
	return out, nil
}

func splitList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func readBody(bodyFlag string) (string, error) {
	if bodyFlag == "" || bodyFlag == "@-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if strings.HasPrefix(bodyFlag, "@") {
		path := strings.TrimPrefix(bodyFlag, "@")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return bodyFlag, nil
}

func isHelp(arg string) bool {
	switch arg {
	case "-h", "--help", "help":
		return true
	default:
		return false
	}
}

func confirmPrompt(prompt string) (bool, error) {
	if err := writeStdout("%s", prompt); err != nil {
		return false, err
	}
	if err := writeStdout(" [y/N]: "); err != nil {
		return false, err
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}

func ensureFilename(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("message id is required")
	}
	if filepath.Base(id) != id {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if !strings.HasSuffix(id, ".md") {
		id += ".md"
	}
	return id, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeStdout(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stdout, format, args...)
	return err
}

func writeStdoutLine(args ...any) error {
	_, err := fmt.Fprintln(os.Stdout, args...)
	return err
}

func writeStderr(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stderr, format, args...)
	return err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
