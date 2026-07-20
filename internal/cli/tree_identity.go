package cli

import (
	"fmt"
	"os"
	"strings"
)

// TreeRelation is a physical-filesystem relationship. Unknown is deliberately
// distinct from Different: callers must choose whether unverifiable identity is
// advisory or authority-bearing instead of accidentally collapsing failures.
type TreeRelation uint8

// Identity tokens are a pathname snapshot. ABA reuse (delete/recreate of a
// directory with the same file ID beneath an untrusted ancestor) remains an
// accepted single-user residual; a durable-handle capability is deferred to W2
// if multi-tenant deployments make this material.

const (
	TreeRelationUnknown TreeRelation = iota
	TreeRelationSame
	TreeRelationDifferent
)

func (r TreeRelation) String() string {
	switch r {
	case TreeRelationSame:
		return "Same"
	case TreeRelationDifferent:
		return "Different"
	default:
		return "Unknown"
	}
}

type treeIdentity struct {
	info  os.FileInfo
	token string
}

func resolveTreeIdentity(path string) (treeIdentity, error) {
	if strings.TrimSpace(path) == "" {
		return treeIdentity{}, fmt.Errorf("empty tree path")
	}
	path = absPath(resolveRoot(path))
	info, err := os.Stat(path)
	if err != nil {
		return treeIdentity{}, err
	}
	if !info.IsDir() {
		return treeIdentity{}, fmt.Errorf("tree path is not a directory: %s", path)
	}
	token, err := platformTreeIdentityToken(path, info)
	if err != nil {
		return treeIdentity{}, err
	}
	return treeIdentity{info: info, token: token}, nil
}

// resolveTreeIdentityToken returns an opaque, versioned, platform-tagged
// identity. Consumers must compare it through verifyTreeIdentityToken and must
// not parse or persist assumptions about its payload encoding.
func resolveTreeIdentityToken(path string) (string, error) {
	identity, err := resolveTreeIdentity(path)
	if err != nil {
		return "", err
	}
	return identity.token, nil
}

func relateTrees(a, b string) TreeRelation {
	left, leftErr := resolveTreeIdentity(a)
	right, rightErr := resolveTreeIdentity(b)
	if leftErr != nil || rightErr != nil {
		return TreeRelationUnknown
	}
	// Authority decisions must use the platform token, not os.SameFile's
	// potentially lossy FileInfo comparison (notably on Windows/ReFS).
	if left.token == right.token {
		return TreeRelationSame
	}
	return TreeRelationDifferent
}

func verifyTreeIdentityToken(path, token string) TreeRelation {
	if !validTreeIdentityToken(token) {
		return TreeRelationUnknown
	}
	current, err := resolveTreeIdentityToken(path)
	if err != nil {
		return TreeRelationUnknown
	}
	if current == token {
		return TreeRelationSame
	}
	return TreeRelationDifferent
}

func validTreeIdentityToken(token string) bool {
	return validPlatformTreeIdentityToken(token)
}
