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
	return ExecutionTrustDigestWithAuthority(plan, session, root, "")
}

func ExecutionTrustDigestWithAuthority(plan Plan, session string, root *fsq.DeliveryRoot, authorityDigest string) (string, error) {
	if !canonicalSessionPattern.MatchString(session) || strings.HasPrefix(session, "-") {
		return "", fmt.Errorf("invalid trust session %q", session)
	}
	if root == nil {
		return "", fmt.Errorf("missing pinned session root")
	}
	if err := root.VerifyBase(); err != nil {
		return "", err
	}
	planDigest, err := plan.TrustSemanticDigest()
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
	return executionTrustDigestWithAuthority(planDigest, session, rootPath, rootIdentity, authorityDigest)
}

// PrepareTrustDigest binds a nonce-free plan digest to the canonical target
// and its physical identity. For an absent session, rootIdentity is a stable
// intended-child identity derived from the pinned parent and child name.
func PrepareTrustDigest(planDigest, session, rootPath, rootIdentity string) (string, error) {
	return PrepareTrustDigestWithAuthority(planDigest, session, rootPath, rootIdentity, "")
}

func PrepareTrustDigestWithAuthority(planDigest, session, rootPath, rootIdentity, authorityDigest string) (string, error) {
	if !validDigest(planDigest) {
		return "", fmt.Errorf("invalid prepare plan digest")
	}
	if !canonicalSessionPattern.MatchString(session) || strings.HasPrefix(session, "-") {
		return "", fmt.Errorf("invalid trust session %q", session)
	}
	if strings.TrimSpace(rootPath) == "" || strings.TrimSpace(rootIdentity) == "" {
		return "", fmt.Errorf("prepare trust target identity is incomplete")
	}
	canonicalRoot, err := resolvedPath(rootPath)
	if err != nil {
		return "", fmt.Errorf("resolve prepare trust session root: %w", err)
	}
	return executionTrustDigestWithAuthority(planDigest, session, canonicalRoot, rootIdentity, authorityDigest)
}

func executionTrustDigestWithAuthority(planDigest, session, rootPath, rootIdentity, authorityDigest string) (string, error) {
	legacy, err := executionTrustDigest(planDigest, session, rootPath, rootIdentity)
	if err != nil || authorityDigest == "" {
		return legacy, err
	}
	if !validDigest(authorityDigest) {
		return "", fmt.Errorf("invalid base-root authority digest")
	}
	return digestCanonical(struct {
		Version         int    `json:"version"`
		LegacyDigest    string `json:"legacy_digest"`
		AuthorityDigest string `json:"authority_digest"`
	}{Version: 2, LegacyDigest: legacy, AuthorityDigest: authorityDigest})
}

func executionTrustDigest(planDigest, session, rootPath, rootIdentity string) (string, error) {
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
