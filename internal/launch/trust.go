// Package launch trust state uses physical project identity and fails closed
// where that identity is unavailable. Windows callers must surface exit 6
// (action_required); they must never substitute a path-based identity.
package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const TrustVersion = 1

type TrustStore struct {
	dir             string
	projectIdentity string
}

// TrustRecord contains all local execution authority for one project. Replace
// overwrites the prior digest, so a semantic change invalidates every prior
// bypass argument and arbitrary-command grant.
type TrustRecord struct {
	Version           int                     `json:"version"`
	ProjectIdentity   string                  `json:"project_identity"`
	SemanticDigest    string                  `json:"semantic_digest"`
	BypassArgs        map[string][]string     `json:"bypass_args,omitempty"`
	ArbitraryCommands []ArbitraryCommandGrant `json:"arbitrary_commands,omitempty"`
}

type ArbitraryCommandGrant struct {
	Name       string            `json:"name"`
	Argv       []string          `json:"argv"`
	EnvOverlay map[string]string `json:"env_overlay,omitempty"`
	Cwd        string            `json:"cwd"`
}

func OpenTrustStore(userStateDir, projectRoot string) (*TrustStore, error) {
	if strings.TrimSpace(userStateDir) == "" {
		return nil, fmt.Errorf("user state directory is required")
	}
	projectAbs, err := resolvedPath(projectRoot)
	if err != nil {
		return nil, err
	}
	identity, err := fsq.StableTreeIdentity(projectAbs)
	if err != nil {
		return nil, fmt.Errorf("resolve project identity: %w", err)
	}
	stateAbs, err := resolvedPath(userStateDir)
	if err != nil {
		return nil, err
	}
	if pathWithin(stateAbs, projectAbs) {
		return nil, fmt.Errorf("trust state directory must be outside the project worktree")
	}
	sum := sha256.Sum256([]byte(identity))
	return &TrustStore{
		dir:             filepath.Join(stateAbs, "launch", "projects", hex.EncodeToString(sum[:])),
		projectIdentity: identity,
	}, nil
}

// resolvedPath resolves symlinks in the existing prefix while preserving a
// missing tail. This prevents an apparently external state path from being an
// alias back into the project.
func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	prefix := abs
	var tail []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(prefix)
		if resolveErr == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return "", resolveErr
		}
		tail = append(tail, filepath.Base(prefix))
		prefix = parent
	}
}

func pathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (store *TrustStore) Path() string {
	if store == nil {
		return ""
	}
	return filepath.Join(store.dir, "trust.json")
}

// Replace atomically installs the only active authority record for a project.
func (store *TrustStore) Replace(record TrustRecord) error {
	if store == nil {
		return fmt.Errorf("missing trust store")
	}
	record.Version = TrustVersion
	record.ProjectIdentity = store.projectIdentity
	if err := validateTrustRecord(record); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = fsq.WriteFileAtomic(store.dir, "trust.json", data, 0o600)
	return err
}

// LoadForDigest returns no record when the plan changed. Malformed, unreadable,
// cross-project, or overly permissive state is an error and must fail closed.
func (store *TrustStore) LoadForDigest(digest string) (TrustRecord, bool, error) {
	if store == nil {
		return TrustRecord{}, false, fmt.Errorf("missing trust store")
	}
	if !validDigest(digest) {
		return TrustRecord{}, false, fmt.Errorf("invalid requested semantic digest")
	}
	identity, err := fsq.SnapshotDeliveryRoot(store.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TrustRecord{}, false, nil
		}
		return TrustRecord{}, false, err
	}
	root, err := fsq.OpenDeliveryRoot(store.dir, identity)
	if err != nil {
		return TrustRecord{}, false, err
	}
	defer func() { _ = root.Close() }()
	file, info, err := root.OpenRegularNoFollow("trust.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TrustRecord{}, false, nil
		}
		return TrustRecord{}, false, err
	}
	defer func() { _ = file.Close() }()
	if info.Mode().Perm() != 0o600 {
		return TrustRecord{}, false, fmt.Errorf("trust record permissions are %04o, want 0600", info.Mode().Perm())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return TrustRecord{}, false, err
	}
	var record TrustRecord
	if err := decodeStrict(data, &record); err != nil {
		return TrustRecord{}, false, fmt.Errorf("decode trust record: %w", err)
	}
	if err := validateTrustRecord(record); err != nil {
		return TrustRecord{}, false, err
	}
	if record.ProjectIdentity != store.projectIdentity {
		return TrustRecord{}, false, fmt.Errorf("trust record belongs to a different project identity")
	}
	if record.SemanticDigest != digest {
		return TrustRecord{}, false, nil
	}
	return record, true, nil
}

func validateTrustRecord(record TrustRecord) error {
	if record.Version != TrustVersion {
		return fmt.Errorf("unsupported trust record version %d", record.Version)
	}
	if strings.TrimSpace(record.ProjectIdentity) == "" {
		return fmt.Errorf("project identity is required")
	}
	if !validDigest(record.SemanticDigest) {
		return fmt.Errorf("invalid semantic digest")
	}
	for handle, args := range record.BypassArgs {
		if fsq.ValidateHandle(handle) != nil || len(args) == 0 {
			return fmt.Errorf("invalid bypass arguments for %q", handle)
		}
	}
	seen := make(map[string]struct{}, len(record.ArbitraryCommands))
	for i, grant := range record.ArbitraryCommands {
		if strings.TrimSpace(grant.Name) == "" || len(grant.Argv) == 0 || strings.TrimSpace(grant.Argv[0]) == "" || strings.TrimSpace(grant.Cwd) == "" {
			return fmt.Errorf("arbitrary_commands[%d] is incomplete", i)
		}
		if _, ok := seen[grant.Name]; ok {
			return fmt.Errorf("duplicate arbitrary command grant %q", grant.Name)
		}
		seen[grant.Name] = struct{}{}
		for key := range grant.EnvOverlay {
			if key == "" || strings.ContainsRune(key, '=') {
				return fmt.Errorf("arbitrary_commands[%d] has invalid environment key %q", i, key)
			}
		}
	}
	return nil
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
