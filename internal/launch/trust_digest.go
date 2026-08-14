package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const executionTrustDigestVersion = 1

// ExecutionTrustDigest binds the static provider plan to the session root in
// which the plan will be emitted. The plan digest alone cannot distinguish a
// selector-free launch after default_session changes.
func ExecutionTrustDigest(plan Plan, session string, root *fsq.DeliveryRoot) (string, error) {
	if !canonicalSessionPattern.MatchString(session) || strings.HasPrefix(session, "-") {
		return "", fmt.Errorf("invalid trust session %q", session)
	}
	if root == nil {
		return "", fmt.Errorf("missing pinned session root")
	}
	if err := root.VerifyBase(); err != nil {
		return "", err
	}
	planDigest, err := plan.SemanticDigest()
	if err != nil {
		return "", err
	}
	rootPath, err := filepath.Abs(root.Base())
	if err != nil {
		return "", err
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", fmt.Errorf("resolve trust session root: %w", err)
	}
	rootIdentity, err := fsq.StableTreeIdentityInfo(root.FileInfo())
	if err != nil {
		return "", fmt.Errorf("resolve trust session-root identity: %w", err)
	}
	canonical, err := json.Marshal(struct {
		Version      int    `json:"version"`
		PlanDigest   string `json:"plan_digest"`
		Session      string `json:"session"`
		RootPath     string `json:"root_path"`
		RootIdentity string `json:"root_identity"`
	}{
		Version: executionTrustDigestVersion, PlanDigest: planDigest,
		Session: session, RootPath: filepath.Clean(rootPath), RootIdentity: rootIdentity,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
