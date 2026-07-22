package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	wakeBaselineSchema       = 1
	wakeBaselineFilePrefix   = ".wake-baseline-"
	wakeBaselineFileSuffix   = ".json"
	maxWakeBaselineMessages  = 512
	envWakeBaselineFile      = "AMQ_WAKE_BASELINE_FILE"
	envWakeBaselineError     = "AMQ_WAKE_BASELINE_ERROR"
	wakeBaselineCaptureError = "baseline_capture_failed"
)

type wakeBaselineMessage struct {
	ID       string `json:"id,omitempty"`
	Filename string `json:"filename"`
}

type wakeBaselineManifest struct {
	Schema   int                   `json:"schema"`
	Root     string                `json:"root"`
	RootID   string                `json:"root_id"`
	Agent    string                `json:"agent"`
	LaunchID string                `json:"launch_id"`
	Created  string                `json:"created"`
	Messages []wakeBaselineMessage `json:"messages"`
}

type wakeBaseline struct {
	Manifest wakeBaselineManifest
	Path     string
	Digest   string
	IDs      map[string]struct{}
	Files    map[string]struct{}
}

func captureWakeBaseline(root, me string) (wakeBaseline, error) {
	root = canonicalWakeRoot(root)
	rootID, err := resolveTreeIdentityToken(root)
	if err != nil {
		return wakeBaseline{}, fmt.Errorf("resolve baseline root identity: %w", err)
	}
	launchID, err := newWakeBaselineLaunchID()
	if err != nil {
		return wakeBaseline{}, err
	}
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, me))
	if err != nil {
		return wakeBaseline{}, fmt.Errorf("scan baseline inbox: %w", err)
	}
	manifest := wakeBaselineManifest{
		Schema:   wakeBaselineSchema,
		Root:     root,
		RootID:   rootID,
		Agent:    me,
		LaunchID: launchID,
		Created:  time.Now().UTC().Format(time.RFC3339Nano),
		Messages: make([]wakeBaselineMessage, 0, len(entries)),
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if len(manifest.Messages) >= maxWakeBaselineMessages {
			return wakeBaseline{}, fmt.Errorf("baseline contains more than %d messages", maxWakeBaselineMessages)
		}
		item := wakeBaselineMessage{Filename: entry.Name()}
		if header, readErr := format.ReadHeaderFile(filepath.Join(fsq.AgentInboxNew(root, me), entry.Name())); readErr == nil {
			item.ID = strings.TrimSpace(header.ID)
		}
		manifest.Messages = append(manifest.Messages, item)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return wakeBaseline{}, fmt.Errorf("marshal wake baseline: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxWakeMetadataFileBytes {
		return wakeBaseline{}, fmt.Errorf("wake baseline exceeds %d bytes", maxWakeMetadataFileBytes)
	}
	path := filepath.Join(fsq.AgentBase(root, me), wakeBaselineFilePrefix+launchID+wakeBaselineFileSuffix)
	if err := writeWakeMetadataFile(path, data, "wake baseline"); err != nil {
		return wakeBaseline{}, err
	}
	baseline, err := readWakeBaseline(path, root, me)
	if err != nil {
		_ = os.Remove(path)
		return wakeBaseline{}, err
	}
	return baseline, nil
}

func newWakeBaselineLaunchID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate wake baseline launch id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func readWakeBaseline(path, root, me string) (wakeBaseline, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return wakeBaseline{}, fmt.Errorf("wake baseline path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return wakeBaseline{}, fmt.Errorf("stat wake baseline: %w", err)
	}
	if err := validateWakeBaselineFile(path, root, me, info); err != nil {
		return wakeBaseline{}, err
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return wakeBaseline{}, fmt.Errorf("open wake baseline: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return wakeBaseline{}, fmt.Errorf("stat opened wake baseline: %w", err)
	}
	if err := validateWakeBaselineFile(path, root, me, openedInfo); err != nil {
		return wakeBaseline{}, err
	}
	if !os.SameFile(info, openedInfo) {
		return wakeBaseline{}, fmt.Errorf("wake baseline %s changed while opening", path)
	}
	data, err := readWakeMetadata(file, "wake baseline", path)
	if err != nil {
		return wakeBaseline{}, err
	}
	var manifest wakeBaselineManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return wakeBaseline{}, fmt.Errorf("parse wake baseline: %w", err)
	}
	if err := validateWakeBaselineManifest(manifest, path, root, me); err != nil {
		return wakeBaseline{}, err
	}
	ids := make(map[string]struct{}, len(manifest.Messages))
	files := make(map[string]struct{}, len(manifest.Messages))
	for _, message := range manifest.Messages {
		if message.ID != "" {
			ids[message.ID] = struct{}{}
		}
		files[message.Filename] = struct{}{}
	}
	return wakeBaseline{
		Manifest: manifest,
		Path:     path,
		Digest:   wakeBaselineDigest(data),
		IDs:      ids,
		Files:    files,
	}, nil
}

func validateWakeBaselineFile(path, root, me string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wake baseline %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake baseline %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("wake baseline %s mode is %o, want 0600", path, got)
	}
	if err := validateWakeTargetPathOwnership("wake baseline", path, info); err != nil {
		return err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve wake baseline: %w", err)
	}
	resolvedAgent, err := filepath.EvalSymlinks(fsq.AgentBase(canonicalWakeRoot(root), me))
	if err != nil {
		return fmt.Errorf("resolve wake baseline agent root: %w", err)
	}
	if filepath.Dir(resolvedPath) != resolvedAgent {
		return fmt.Errorf("wake baseline must be directly under the exact agent root")
	}
	return validateWakeTargetParentDirs("wake baseline", resolvedPath)
}

func validateWakeBaselineManifest(manifest wakeBaselineManifest, path, root, me string) error {
	if manifest.Schema != wakeBaselineSchema {
		return fmt.Errorf("wake baseline schema %d unsupported", manifest.Schema)
	}
	if canonicalWakeRoot(manifest.Root) != canonicalWakeRoot(root) {
		return fmt.Errorf("wake baseline root mismatch")
	}
	if manifest.Agent != me {
		return fmt.Errorf("wake baseline agent mismatch")
	}
	if verifyTreeIdentityToken(root, manifest.RootID) != TreeRelationSame {
		return fmt.Errorf("wake baseline root identity mismatch")
	}
	if len(manifest.LaunchID) != 32 {
		return fmt.Errorf("wake baseline launch id is invalid")
	}
	if _, err := hex.DecodeString(manifest.LaunchID); err != nil {
		return fmt.Errorf("wake baseline launch id is invalid")
	}
	wantName := wakeBaselineFilePrefix + manifest.LaunchID + wakeBaselineFileSuffix
	if filepath.Base(path) != wantName {
		return fmt.Errorf("wake baseline launch id does not match its filename")
	}
	if len(manifest.Messages) > maxWakeBaselineMessages {
		return fmt.Errorf("wake baseline contains more than %d messages", maxWakeBaselineMessages)
	}
	seenFiles := make(map[string]struct{}, len(manifest.Messages))
	for _, message := range manifest.Messages {
		if message.Filename == "" || filepath.Base(message.Filename) != message.Filename ||
			strings.HasPrefix(message.Filename, ".") || !strings.HasSuffix(message.Filename, ".md") {
			return fmt.Errorf("wake baseline contains invalid message filename")
		}
		if strings.ContainsRune(message.ID, 0) || strings.ContainsAny(message.ID, "/\\") {
			return fmt.Errorf("wake baseline contains invalid message id")
		}
		if _, exists := seenFiles[message.Filename]; exists {
			return fmt.Errorf("wake baseline contains duplicate message filename")
		}
		seenFiles[message.Filename] = struct{}{}
	}
	return nil
}

func wakeBaselineDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sameWakeBaselineMessage(baseline *wakeBaseline, id, filename string) bool {
	if baseline == nil {
		return false
	}
	if id != "" {
		_, ok := baseline.IDs[id]
		return ok
	}
	_, ok := baseline.Files[filename]
	return ok
}

func removeWakeBaselineIfUnreferenced(root, me, path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if target, exists, err := readWakeTarget(root, me); err == nil && exists && target.BaselineFile == path {
		return
	}
	if info, err := os.Lstat(path); err == nil && validateWakeBaselineFile(path, root, me, info) == nil {
		_ = os.Remove(path)
	}
}
