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
	envWakeOwner        = "AMQ_WAKE_OWNER"
)

type wakeOwner struct {
	PID          int    `json:"pid"`
	ProcessStart string `json:"process_start,omitempty"`
	BootID       string `json:"boot_id,omitempty"`
	SessionID    int    `json:"session_id,omitempty"`
}

type wakeTarget struct {
	Schema     int        `json:"schema"`
	Mode       string     `json:"mode"`
	Root       string     `json:"root"`
	Agent      string     `json:"agent"`
	Created    string     `json:"created"`
	InjectVia  string     `json:"inject_via"`
	InjectArgs []string   `json:"inject_args,omitempty"`
	Owner      *wakeOwner `json:"owner,omitempty"`
}

func wakeTargetPath(root, me string) string {
	return filepath.Join(fsq.AgentBase(root, me), wakeTargetFileName)
}

func newWakeTarget(root, me, injectVia string, injectArgs []string) (wakeTarget, error) {
	resolvedInjectVia, err := wakeTargetInjectViaPath(injectVia)
	if err != nil {
		return wakeTarget{}, err
	}
	return wakeTarget{
		Schema:     wakeTargetSchema,
		Mode:       wakeTargetInjectVia,
		Root:       canonicalWakeRoot(root),
		Agent:      me,
		Created:    time.Now().UTC().Format(time.RFC3339),
		InjectVia:  resolvedInjectVia,
		InjectArgs: append([]string{}, injectArgs...),
	}, nil
}

func wakeTargetInjectViaPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	info, err := os.Lstat(trimmed)
	if err != nil {
		return "", fmt.Errorf("stat inject_via: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return trimmed, nil
	}
	if resolved, err := filepath.EvalSymlinks(trimmed); err == nil {
		return resolved, nil
	} else {
		return "", fmt.Errorf("resolve inject_via: %w", err)
	}
}

func wakeTargetDigest(target wakeTarget) string {
	data, _ := json.Marshal(target)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateWakeTargetMatchesLock(lock wakeLock, target wakeTarget) error {
	if lock.WakeMode != wakeTargetInjectVia || lock.TargetDigest == "" {
		return fmt.Errorf("wake lock was not created for an inject-via repair target")
	}
	if strings.TrimSpace(lock.Root) == "" || canonicalWakeRoot(lock.Root) != canonicalWakeRoot(target.Root) {
		return fmt.Errorf("wake lock root mismatch")
	}
	if lock.Agent != target.Agent {
		return fmt.Errorf("wake lock agent mismatch")
	}
	if got := wakeTargetDigest(target); got != lock.TargetDigest {
		return fmt.Errorf("wake target does not match wake lock")
	}
	return nil
}

func readWakeTarget(root, me string) (wakeTarget, bool, error) {
	path := wakeTargetPath(root, me)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return wakeTarget{}, false, nil
		}
		return wakeTarget{}, true, fmt.Errorf("stat wake target: %w", err)
	}
	if err := validateWakeTargetFile(path, info); err != nil {
		return wakeTarget{}, true, err
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return wakeTarget{}, false, nil
		}
		return wakeTarget{}, true, fmt.Errorf("open wake target: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return wakeTarget{}, true, fmt.Errorf("stat wake target: %w", err)
	}
	if err := validateWakeTargetFile(path, openedInfo); err != nil {
		return wakeTarget{}, true, err
	}
	data, err := readWakeMetadata(file, "wake target", path)
	if err != nil {
		return wakeTarget{}, true, err
	}
	var target wakeTarget
	if err := json.Unmarshal(data, &target); err != nil {
		return wakeTarget{}, true, fmt.Errorf("parse wake target: %w", err)
	}
	return target, true, nil
}

func validateWakeTargetFile(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wake target %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake target %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("wake target %s mode is %o, want 0600", path, got)
	}
	if err := validateWakeTargetPathOwnership("wake target", path, info); err != nil {
		return err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve wake target: %w", err)
	}
	return validateWakeTargetParentDirs("wake target", resolvedPath)
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
	return writeWakeMetadataFile(wakeTargetPath(root, me), data, "wake target")
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
	if target.Mode != wakeTargetInjectVia {
		return fmt.Errorf("wake target mode %q unsupported", target.Mode)
	}
	if canonicalWakeRoot(target.Root) != canonicalWakeRoot(root) {
		return fmt.Errorf("wake target root mismatch")
	}
	if target.Agent != me {
		return fmt.Errorf("wake target agent mismatch")
	}
	if err := validateWakeInjectViaPath(target.InjectVia); err != nil {
		return err
	}
	for _, arg := range target.InjectArgs {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("wake target inject arg contains NUL")
		}
	}
	if target.Owner != nil {
		if err := validateWakeOwner(*target.Owner); err != nil {
			return err
		}
	}
	return nil
}

func encodeWakeOwnerEnv(owner wakeOwner) (string, error) {
	if err := validateWakeOwner(owner); err != nil {
		return "", err
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return "", fmt.Errorf("marshal wake owner: %w", err)
	}
	return string(data), nil
}

func wakeOwnerFromEnv() (*wakeOwner, error) {
	raw := strings.TrimSpace(os.Getenv(envWakeOwner))
	if raw == "" {
		return nil, nil
	}
	var owner wakeOwner
	if err := json.Unmarshal([]byte(raw), &owner); err != nil {
		return nil, fmt.Errorf("parse %s: %w", envWakeOwner, err)
	}
	if err := validateWakeOwner(owner); err != nil {
		return nil, fmt.Errorf("parse %s: %w", envWakeOwner, err)
	}
	return &owner, nil
}

func validateWakeOwner(owner wakeOwner) error {
	if owner.PID <= 0 {
		return fmt.Errorf("wake owner pid must be > 0")
	}
	if strings.ContainsRune(owner.ProcessStart, 0) {
		return fmt.Errorf("wake owner process start contains NUL")
	}
	if strings.ContainsRune(owner.BootID, 0) {
		return fmt.Errorf("wake owner boot id contains NUL")
	}
	if owner.SessionID < 0 {
		return fmt.Errorf("wake owner session id must be >= 0")
	}
	return nil
}

func validateWakeInjectViaPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("inject_via must not be blank")
	}
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("inject_via contains NUL")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("inject_via must be an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat inject_via: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("inject_via must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("inject_via must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("inject_via is not executable")
	}
	if err := validateWakeTargetPathOwnership("inject_via", path, info); err != nil {
		return err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve inject_via: %w", err)
	}
	if err := validateWakeTargetParentDirs("inject_via", resolvedPath); err != nil {
		return err
	}
	return nil
}

func validateWakeTargetPathOwnership(label, path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s %s is group/world-writable", label, path)
	}
	ownerUID, ownerOK := wakeTargetFileOwnerUID(info)
	currentUID, currentOK := wakeTargetCurrentUID()
	if ownerOK && currentOK && ownerUID != currentUID {
		return fmt.Errorf("%s %s is owned by uid %d, want current uid %d", label, path, ownerUID, currentUID)
	}
	return nil
}

func validateWakeTargetParentDirs(label, path string) error {
	cleanParent := filepath.Dir(filepath.Clean(path))
	start, err := filepath.EvalSymlinks(cleanParent)
	if err != nil {
		return fmt.Errorf("resolve %s parent %s: %w", label, cleanParent, err)
	}
	for dir := start; ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("stat %s parent %s: %w", label, dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s parent %s must be a directory", label, dir)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s parent %s is group/world-writable", label, dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
	}
}
