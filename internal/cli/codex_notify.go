package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/pelletier/go-toml/v2"
)

const codexNotifyForwardTimeout = 10 * time.Second

var (
	codexNotifyExecutable = os.Executable
	codexNotifyForward    = forwardOperatorCodexNotify
)

func runCodexNotify(args []string) error {
	if len(args) != 7 || args[0] != "--root" || args[2] != "--handle" || args[4] != "--nonce" ||
		strings.TrimSpace(args[1]) == "" || strings.TrimSpace(args[3]) == "" || strings.TrimSpace(args[5]) == "" {
		return fmt.Errorf("codex notify requires --root, --handle, --nonce, and one payload")
	}
	rootPath, handle, nonce := args[1], args[3], args[5]
	payload := []byte(args[6])
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		return fmt.Errorf("snapshot codex notify root: %w", err)
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		return fmt.Errorf("open codex notify root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := fsq.ValidateExistingMailboxLayout(root, handle); err != nil {
		return fmt.Errorf("validate codex notify root: %w", err)
	}
	amqExecutable, err := codexNotifyExecutable()
	if err != nil {
		return fmt.Errorf("resolve codex notify executable: %w", err)
	}
	if _, err := launch.RecordCodexNotify(root, handle, nonce, amqExecutable, payload); err != nil {
		return err
	}
	if err := codexNotifyForward(amqExecutable, payload); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "amq: codex operator notify failed: %v\n", err)
	}
	return nil
}

type codexOperatorConfig struct {
	Notify []string `toml:"notify"`
}

func forwardOperatorCodexNotify(amqExecutable string, payload []byte) error {
	argv, err := loadOperatorCodexNotify()
	if err != nil {
		return err
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil
	}
	target, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("resolve operator notify executable: %w", err)
	}
	targetIdentity, err := fsq.StableFileIdentity(target)
	if err != nil {
		return fmt.Errorf("identify operator notify executable: %w", err)
	}
	amqIdentity, err := fsq.StableFileIdentity(amqExecutable)
	if err != nil {
		return fmt.Errorf("identify AMQ executable: %w", err)
	}
	if targetIdentity == amqIdentity {
		return nil
	}
	forwardArgs := append(append([]string(nil), argv[1:]...), string(payload))
	ctx, cancel := context.WithTimeout(context.Background(), codexNotifyForwardTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, target, forwardArgs...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("operator notify timed out: %w", ctx.Err())
		}
		return fmt.Errorf("operator notify failed: %w", err)
	}
	return nil
}

func loadOperatorCodexNotify() ([]string, error) {
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve Codex home: %w", err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Codex config: %w", err)
	}
	var config codexOperatorConfig
	if err := toml.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("decode Codex config: %w", err)
	}
	return append([]string(nil), config.Notify...), nil
}
