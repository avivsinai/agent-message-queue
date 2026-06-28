package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	wakeTargetSchema    = 1
	wakeTargetFileName  = ".wake.target"
	wakeTargetInjectVia = "inject-via"
	wakeTargetGhostty   = "ghostty"
)

type wakeTarget struct {
	Schema            int      `json:"schema"`
	Mode              string   `json:"mode"`
	Root              string   `json:"root"`
	Agent             string   `json:"agent"`
	Created           string   `json:"created"`
	InjectVia         string   `json:"inject_via,omitempty"`
	InjectArgs        []string `json:"inject_args,omitempty"`
	GhosttyTerminalID string   `json:"ghostty_terminal_id,omitempty"`
}

func wakeTargetPath(root, me string) string {
	return filepath.Join(fsq.AgentBase(root, me), wakeTargetFileName)
}

func newInjectViaWakeTarget(root, me, injectVia string, injectArgs []string) wakeTarget {
	return wakeTarget{
		Schema:     wakeTargetSchema,
		Mode:       wakeTargetInjectVia,
		Root:       canonicalWakeRoot(root),
		Agent:      me,
		Created:    time.Now().UTC().Format(time.RFC3339),
		InjectVia:  strings.TrimSpace(injectVia),
		InjectArgs: append([]string{}, injectArgs...),
	}
}

func newGhosttyWakeTarget(root, me, terminalID string) wakeTarget {
	return wakeTarget{
		Schema:            wakeTargetSchema,
		Mode:              wakeTargetGhostty,
		Root:              canonicalWakeRoot(root),
		Agent:             me,
		Created:           time.Now().UTC().Format(time.RFC3339),
		GhosttyTerminalID: strings.TrimSpace(terminalID),
	}
}

func wakeTargetDigest(target wakeTarget) string {
	target.Created = ""
	data, _ := json.Marshal(target)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateWakeTargetMatchesLock(lock wakeLock, target wakeTarget) error {
	if lock.WakeMode != target.Mode || lock.TargetDigest == "" {
		return fmt.Errorf("wake lock was not created for target mode %q", target.Mode)
	}
	if got := wakeTargetDigest(target); got != lock.TargetDigest {
		return fmt.Errorf("wake target does not match wake lock")
	}
	return nil
}

func readWakeTarget(root, me string) (wakeTarget, bool, error) {
	path := wakeTargetPath(root, me)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return wakeTarget{}, false, nil
		}
		return wakeTarget{}, true, fmt.Errorf("read wake target: %w", err)
	}
	var target wakeTarget
	if err := json.Unmarshal(data, &target); err != nil {
		return wakeTarget{}, true, fmt.Errorf("parse wake target: %w", err)
	}
	return target, true, nil
}

func writeWakeTarget(root, me string, target wakeTarget) error {
	if err := validateWakeTarget(target, root, me); err != nil {
		return err
	}
	agentBase := fsq.AgentBase(root, me)
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		return fmt.Errorf("create wake target directory: %w", err)
	}
	data, err := json.MarshalIndent(target, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal wake target: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(wakeTargetPath(root, me), data, 0o600); err != nil {
		return fmt.Errorf("write wake target: %w", err)
	}
	return nil
}

func removeWakeTarget(root, me string) error {
	if err := os.Remove(wakeTargetPath(root, me)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove wake target: %w", err)
	}
	return nil
}

func validateWakeTarget(target wakeTarget, root, me string) error {
	if target.Schema != wakeTargetSchema {
		return fmt.Errorf("wake target schema %d unsupported", target.Schema)
	}
	if canonicalWakeRoot(target.Root) != canonicalWakeRoot(root) {
		return fmt.Errorf("wake target root mismatch")
	}
	if target.Agent != me {
		return fmt.Errorf("wake target agent mismatch")
	}
	switch target.Mode {
	case wakeTargetInjectVia:
		if strings.TrimSpace(target.InjectVia) == "" {
			return fmt.Errorf("inject_via must not be blank")
		}
		if strings.ContainsRune(target.InjectVia, 0) {
			return fmt.Errorf("inject_via contains NUL")
		}
		for _, arg := range target.InjectArgs {
			if strings.ContainsRune(arg, 0) {
				return fmt.Errorf("wake target inject arg contains NUL")
			}
		}
	case wakeTargetGhostty:
		if _, err := normalizeGhosttyTerminalID(target.GhosttyTerminalID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("wake target mode %q unsupported", target.Mode)
	}
	return nil
}

func wakeTargetDescription(target wakeTarget) string {
	switch target.Mode {
	case wakeTargetGhostty:
		return ghosttyTargetString(target.GhosttyTerminalID)
	case wakeTargetInjectVia:
		return target.InjectVia
	default:
		return target.Mode
	}
}
