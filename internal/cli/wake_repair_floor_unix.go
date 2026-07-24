//go:build darwin || linux

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	wakeRepairFloorSchema       = 1
	wakeRepairFloorFileName     = ".wake.repair-floor"
	maxWakeRepairFloorFileBytes = 8 * 1024 * 1024
)

var currentWakeBootID = func() string {
	return inspectWakeProcess(os.Getpid()).BootID
}

// wakeRepairFloor is private continuity state for one ownerless inject-via
// lineage. Existing contains only the exact local file identities that the
// running wake deliberately suppresses. It never records or compares message
// IDs, so a DLQ retry written as a new file instance remains eligible.
type wakeRepairFloor struct {
	Schema            int                         `json:"schema"`
	Root              string                      `json:"root"`
	RootIdentity      string                      `json:"root_identity"`
	Agent             string                      `json:"agent"`
	Generation        string                      `json:"generation"`
	SourceGeneration  string                      `json:"source_generation,omitempty"`
	SourceFloorDigest string                      `json:"source_floor_digest,omitempty"`
	TargetDigest      string                      `json:"target_digest"`
	BootID            string                      `json:"boot_id"`
	Owner             *wakeOwner                  `json:"owner,omitempty"`
	Existing          map[string]wakeFileIdentity `json:"existing"`
}

type wakeRepairSource struct {
	Root               string
	RootIdentity       string
	Agent              string
	DeadGeneration     string
	BootID             string
	Owner              *wakeOwner
	SourceTargetDigest string
	SourceFloorDigest  string
	AgentDirDevice     uint64
	AgentDirInode      uint64
	InboxDirDevice     uint64
	InboxDirInode      uint64
}

type wakeRepairLineage struct {
	source wakeRepairSource
	floor  wakeRepairFloor
}

func wakeRepairFloorPath(root, me string) string {
	return filepath.Join(fsq.AgentBase(root, me), wakeRepairFloorFileName)
}

func newWakeRepairFloor(
	root, me string,
	lock wakeLock,
	target wakeTarget,
	existing map[string]wakeFileIdentity,
) (wakeRepairFloor, error) {
	rootIdentity, err := resolveTreeIdentityToken(root)
	if err != nil {
		return wakeRepairFloor{}, fmt.Errorf("snapshot wake repair root identity: %w", err)
	}
	targetDigest, err := wakeTargetDigest(target)
	if err != nil {
		return wakeRepairFloor{}, err
	}
	floor := wakeRepairFloor{
		Schema:       wakeRepairFloorSchema,
		Root:         canonicalWakeRoot(root),
		RootIdentity: rootIdentity,
		Agent:        me,
		Generation:   lock.Generation,
		TargetDigest: targetDigest,
		BootID:       lock.BootID,
		Existing:     cloneWakeFileIdentities(existing),
	}
	if target.Owner != nil {
		owner := *target.Owner
		floor.Owner = &owner
	}
	if err := validateWakeRepairFloor(floor, root, me, lock, target); err != nil {
		return wakeRepairFloor{}, err
	}
	return floor, nil
}

func newInheritedWakeRepairFloor(
	source wakeRepairSource,
	lock wakeLock,
	target wakeTarget,
	existing map[string]wakeFileIdentity,
) (wakeRepairFloor, error) {
	targetDigest, err := wakeTargetDigest(target)
	if err != nil {
		return wakeRepairFloor{}, err
	}
	if targetDigest != source.SourceTargetDigest {
		return wakeRepairFloor{}, fmt.Errorf("wake repair inherited target digest mismatch")
	}
	floor := wakeRepairFloor{
		Schema:            wakeRepairFloorSchema,
		Root:              source.Root,
		RootIdentity:      source.RootIdentity,
		Agent:             source.Agent,
		Generation:        lock.Generation,
		SourceGeneration:  source.DeadGeneration,
		SourceFloorDigest: source.SourceFloorDigest,
		TargetDigest:      targetDigest,
		BootID:            source.BootID,
		Existing:          cloneWakeFileIdentities(existing),
	}
	if source.Owner != nil {
		owner := *source.Owner
		floor.Owner = &owner
	}
	if floor.Root == "" || floor.RootIdentity == "" || floor.Agent == "" ||
		floor.Generation == "" || floor.SourceGeneration == "" ||
		floor.SourceFloorDigest == "" {
		return wakeRepairFloor{}, fmt.Errorf("wake repair inherited lineage is incomplete")
	}
	return floor, nil
}

func cloneWakeFileIdentities(existing map[string]wakeFileIdentity) map[string]wakeFileIdentity {
	cloned := make(map[string]wakeFileIdentity, len(existing))
	for name, identity := range existing {
		cloned[name] = identity
	}
	return cloned
}

func wakeRepairFloorDigest(floor wakeRepairFloor) (string, error) {
	data, err := json.Marshal(floor)
	if err != nil {
		return "", fmt.Errorf("marshal wake repair floor digest: %w", err)
	}
	return wakeMetadataDigest(data), nil
}

func wakeMetadataDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sameWakeRepairSuppression(first, second wakeRepairFloor) bool {
	if len(first.Existing) != len(second.Existing) {
		return false
	}
	for name, identity := range first.Existing {
		if second.Existing[name] != identity {
			return false
		}
	}
	return true
}

func writeWakeRepairFloor(root, me string, floor wakeRepairFloor) error {
	data, err := json.MarshalIndent(floor, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal wake repair floor: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxWakeRepairFloorFileBytes {
		return fmt.Errorf("wake repair floor has too many existing messages (%d-byte limit)", maxWakeRepairFloorFileBytes)
	}
	return writeWakeMetadataFile(wakeRepairFloorPath(root, me), data, "wake repair floor")
}

func readWakeRepairFloor(root, me string) (wakeRepairFloor, bool, error) {
	path := wakeRepairFloorPath(root, me)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return wakeRepairFloor{}, false, nil
		}
		return wakeRepairFloor{}, true, fmt.Errorf("stat wake repair floor: %w", err)
	}
	if err := validateWakeRepairFloorFile(path, info); err != nil {
		return wakeRepairFloor{}, true, err
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return wakeRepairFloor{}, false, nil
		}
		return wakeRepairFloor{}, true, fmt.Errorf("open wake repair floor: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return wakeRepairFloor{}, true, fmt.Errorf("stat wake repair floor: %w", err)
	}
	if err := validateWakeRepairFloorFile(path, openedInfo); err != nil {
		return wakeRepairFloor{}, true, err
	}
	if !os.SameFile(info, openedInfo) {
		return wakeRepairFloor{}, true, fmt.Errorf("wake repair floor %s changed while opening", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxWakeRepairFloorFileBytes+1))
	if err != nil {
		return wakeRepairFloor{}, true, fmt.Errorf("read wake repair floor: %w", err)
	}
	if len(data) > maxWakeRepairFloorFileBytes {
		return wakeRepairFloor{}, true, fmt.Errorf("wake repair floor %s is too large", path)
	}
	var floor wakeRepairFloor
	if err := json.Unmarshal(data, &floor); err != nil {
		return wakeRepairFloor{}, true, fmt.Errorf("parse wake repair floor: %w", err)
	}
	return floor, true, nil
}

func validateWakeRepairFloorFile(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wake repair floor %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake repair floor %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("wake repair floor %s mode is %o, want 0600", path, got)
	}
	if err := validateWakeTargetPathOwnership("wake repair floor", path, info); err != nil {
		return err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve wake repair floor: %w", err)
	}
	return validateWakeTargetParentDirs("wake repair floor", resolvedPath)
}

func validateWakeRepairFloor(
	floor wakeRepairFloor,
	root, me string,
	lock wakeLock,
	target wakeTarget,
) error {
	if floor.Schema != wakeRepairFloorSchema {
		return fmt.Errorf("wake repair floor schema %d unsupported", floor.Schema)
	}
	if floor.Root != canonicalWakeRoot(root) {
		return fmt.Errorf("wake repair floor root mismatch")
	}
	if verifyTreeIdentityToken(root, floor.RootIdentity) != TreeRelationSame {
		return fmt.Errorf("wake repair floor root identity mismatch")
	}
	if floor.Agent != me {
		return fmt.Errorf("wake repair floor agent mismatch")
	}
	if strings.TrimSpace(floor.Generation) == "" || floor.Generation != lock.Generation {
		return fmt.Errorf("wake repair floor generation mismatch")
	}
	if (floor.SourceGeneration == "") != (floor.SourceFloorDigest == "") {
		return fmt.Errorf("wake repair floor source lineage is incomplete")
	}
	if floor.SourceGeneration != "" {
		if floor.SourceGeneration == floor.Generation {
			return fmt.Errorf("wake repair floor source generation matches child generation")
		}
		if !strings.HasPrefix(floor.SourceFloorDigest, "sha256:") {
			return fmt.Errorf("wake repair floor source digest is invalid")
		}
	}
	targetDigest, err := wakeTargetDigest(target)
	if err != nil {
		return err
	}
	if floor.TargetDigest == "" || floor.TargetDigest != targetDigest || floor.TargetDigest != lock.TargetDigest {
		return fmt.Errorf("wake repair floor target mismatch")
	}
	if floor.BootID == "" || lock.BootID == "" ||
		compareWakeBootID(floor.BootID, wakeProcessInfo{BootID: lock.BootID}) != bootIDMatch {
		return fmt.Errorf("wake repair floor boot identity mismatch")
	}
	if !sameWakeOwner(floor.Owner, target.Owner) || !sameWakeOwner(floor.Owner, lock.Owner) {
		return fmt.Errorf("wake repair floor owner mismatch")
	}
	if floor.Existing == nil {
		return fmt.Errorf("wake repair floor existing-message map is missing")
	}
	for name := range floor.Existing {
		if filepath.Base(name) != name || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			return fmt.Errorf("wake repair floor contains invalid message filename %q", name)
		}
	}
	return nil
}

func validateWakeRepairFloorCurrentBoot(floor wakeRepairFloor) error {
	currentBootID := currentWakeBootID()
	if floor.BootID == "" || currentBootID == "" ||
		compareWakeBootID(floor.BootID, wakeProcessInfo{BootID: currentBootID}) != bootIDMatch {
		return fmt.Errorf("wake repair floor boot identity does not match the current boot")
	}
	return nil
}

func validateWakeRepairFloorAvailable(
	root, me string,
	inspection wakeLockInspection,
	target wakeTarget,
) error {
	floor, exists, err := readWakeRepairFloor(root, me)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("wake repair floor is missing")
	}
	if err := validateWakeRepairFloor(floor, root, me, inspection.Lock, target); err != nil {
		return err
	}
	return validateWakeRepairFloorCurrentBoot(floor)
}

func validateWakeRepairLineageGuarded(
	root, me string,
	target wakeTarget,
	lineage *wakeRepairLineage,
) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return agentDir.withFD(func(dirfd int) error {
		return validateWakeRepairLineageGuardedAt(
			dirfd,
			agentDir,
			root,
			me,
			target,
			lineage,
		)
	})
}

func validateWakeRepairLineageGuardedAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root, me string,
	target wakeTarget,
	lineage *wakeRepairLineage,
) error {
	if lineage == nil {
		return nil
	}
	if lineage.source.Root != canonicalWakeRoot(root) ||
		lineage.source.Agent != me ||
		lineage.source.DeadGeneration != lineage.floor.Generation ||
		lineage.source.RootIdentity != lineage.floor.RootIdentity ||
		lineage.source.BootID != lineage.floor.BootID ||
		!sameWakeOwner(lineage.source.Owner, lineage.floor.Owner) {
		return fmt.Errorf("wake repair source lineage is inconsistent")
	}
	if err := revalidateWakeRepairRootIdentity(root, lineage.source.RootIdentity); err != nil {
		return err
	}
	current, exists, err := readWakeRepairFloorAt(dirfd, agentDir)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("wake repair floor disappeared before acquisition")
	}
	proc := inspectWakeProcess(os.Getpid())
	source := wakeLock{
		Root:         canonicalWakeRoot(root),
		Agent:        me,
		Generation:   lineage.floor.Generation,
		TargetDigest: lineage.floor.TargetDigest,
		BootID:       proc.BootID,
		Owner:        target.Owner,
	}
	if err := validateWakeRepairFloor(current, root, me, source, target); err != nil {
		return err
	}
	digest, err := wakeRepairFloorDigest(current)
	if err != nil {
		return err
	}
	if digest != lineage.source.SourceFloorDigest {
		return fmt.Errorf("wake repair floor changed before acquisition")
	}
	targetDigest, err := wakeTargetDigest(target)
	if err != nil {
		return err
	}
	if targetDigest != lineage.source.SourceTargetDigest {
		return fmt.Errorf("wake repair target changed before acquisition")
	}
	return nil
}

func removeWakeRepairFloorGuarded(root, me string) error {
	if err := os.Remove(wakeRepairFloorPath(root, me)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove wake repair floor: %w", err)
	}
	return nil
}

func removeWakeRepairFloorIfGenerationGuarded(root, me, generation string) error {
	floor, exists, err := readWakeRepairFloor(root, me)
	if err != nil {
		return err
	}
	if !exists || floor.Generation != generation {
		return nil
	}
	return removeWakeRepairFloorGuarded(root, me)
}
