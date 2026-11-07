package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/wakelock"
)

const (
	wakeTargetSchema    = 1
	wakeTargetFileName  = ".wake.target"
	wakeTargetInjectVia = wakelock.InjectViaMode
	envWakeOwner        = "AMQ_WAKE_OWNER"
)

type wakeOwner = wakelock.Owner

func validateWakeOwner(owner wakeOwner) error {
	return wakelock.ValidateOwner(owner)
}

func validateAuthoritativeWakeOwner(owner wakeOwner) error {
	return wakelock.ValidateAuthoritativeOwner(owner)
}

func validateWakeTargetPathOwnership(label, path string, info os.FileInfo) error {
	return wakelock.ValidatePathOwnership(label, path, info)
}

type wakeTarget struct {
	Schema     int        `json:"schema"`
	Mode       string     `json:"mode"`
	Root       string     `json:"root"`
	Agent      string     `json:"agent"`
	Created    string     `json:"created"`
	InjectVia  string     `json:"inject_via"`
	InjectArgs []string   `json:"inject_args,omitempty"`
	RetryUntil string     `json:"retry_until,omitempty"`
	Owner      *wakeOwner `json:"owner,omitempty"`
}

// sameWakeInjectorIdentity compares the behavior fixed at launch: the resolved
// executable and ordered fixed arguments. Provider target identity must
// therefore be encoded in InjectArgs; ambient environment or provider config is
// intentionally invisible. Created and owner metadata describe an instance,
// not the injector semantics it provides.
func sameWakeInjectorIdentity(first, second wakeTarget) bool {
	firstRetry, firstErr := normalizeWakeRetryUntil(first.RetryUntil)
	secondRetry, secondErr := normalizeWakeRetryUntil(second.RetryUntil)
	return firstErr == nil && secondErr == nil &&
		first.InjectVia == second.InjectVia &&
		slices.Equal(first.InjectArgs, second.InjectArgs) &&
		firstRetry == secondRetry
}

func sameWakeTarget(first, second wakeTarget) bool {
	firstDigest, firstErr := wakeTargetDigest(first)
	secondDigest, secondErr := wakeTargetDigest(second)
	return firstErr == nil && secondErr == nil && firstDigest == secondDigest
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
	return validateWakeInjectViaPath(path)
}

func wakeTargetDigest(target wakeTarget) (string, error) {
	data, err := json.Marshal(target)
	if err != nil {
		return "", fmt.Errorf("marshal wake target digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateWakeTargetMatchesLock(lock wakeLock, target wakeTarget) error {
	if (lock.WakeMode != wakeTargetInjectVia && lock.WakeMode != wakeOwnerWakeMode) || lock.TargetDigest == "" {
		return fmt.Errorf("wake lock was not created for an inject-via repair target")
	}
	if lock.WakeMode == wakeOwnerWakeMode {
		if lock.OwnerSchema != wakeOwnerLockSchema || lock.Owner == nil {
			return fmt.Errorf("wake owner lock schema is incomplete")
		}
		if err := validateAuthoritativeWakeOwner(*lock.Owner); err != nil {
			return fmt.Errorf("wake lock owner is invalid: %w", err)
		}
		if target.Owner == nil {
			return fmt.Errorf("wake target owner is missing")
		}
		if err := validateAuthoritativeWakeOwner(*target.Owner); err != nil {
			return fmt.Errorf("wake target owner is invalid: %w", err)
		}
		if !sameWakeOwner(lock.Owner, target.Owner) {
			return fmt.Errorf("wake target owner does not match wake lock owner")
		}
	}
	if strings.TrimSpace(lock.Root) == "" || canonicalWakeRoot(lock.Root) != canonicalWakeRoot(target.Root) {
		return fmt.Errorf("wake lock root mismatch")
	}
	if lock.Agent != target.Agent {
		return fmt.Errorf("wake lock agent mismatch")
	}
	got, err := wakeTargetDigest(target)
	if err != nil {
		return err
	}
	if got != lock.TargetDigest {
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
	if err := validateResolvedWakeInjectViaPath(target.InjectVia); err != nil {
		return err
	}
	for _, arg := range target.InjectArgs {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("wake target inject arg contains NUL")
		}
	}
	if err := validateStoredWakeRetryUntil(target.RetryUntil); err != nil {
		return fmt.Errorf("wake target %w", err)
	}
	if target.Owner != nil {
		if err := validateWakeOwner(*target.Owner); err != nil {
			return err
		}
	}
	return nil
}

func encodeWakeOwnerEnv(owner wakeOwner) (string, error) {
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
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

func validateWakeInjectViaPath(path string) (string, error) {
	resolvedPath, err := resolveWakeInjectViaPath(path)
	if err != nil {
		return "", err
	}
	if err := validateResolvedWakeInjectViaTarget(resolvedPath); err != nil {
		return "", err
	}
	return resolvedPath, nil
}

func resolveWakeInjectViaPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("inject_via must not be blank")
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("inject_via contains NUL")
	}
	if !filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("inject_via must be an absolute path")
	}
	resolvedPath, err := filepath.EvalSymlinks(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve inject_via: %w", err)
	}
	return resolvedPath, nil
}

func validateResolvedWakeInjectViaTarget(resolvedPath string) error {
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return fmt.Errorf("stat inject_via: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("inject_via must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("inject_via is not executable")
	}
	if err := validateWakeTargetPathOwnership("inject_via", resolvedPath, info); err != nil {
		return err
	}
	if err := validateWakeTargetParentDirs("inject_via", resolvedPath); err != nil {
		return err
	}
	return nil
}

func validateResolvedWakeInjectViaPath(path string) error {
	resolvedPath, err := resolveWakeInjectViaPath(path)
	if err != nil {
		return err
	}
	if resolvedPath != strings.TrimSpace(path) {
		return fmt.Errorf("inject_via must be a resolved path")
	}
	if err := validateResolvedWakeInjectViaTarget(resolvedPath); err != nil {
		return err
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
