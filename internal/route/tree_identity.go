package route

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
// accepted single-user residual.

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

func (w Walker) resolveTreeIdentity(path string) (treeIdentity, error) {
	if strings.TrimSpace(path) == "" {
		return treeIdentity{}, fmt.Errorf("empty tree path")
	}
	path = absPath(w.ResolveRoot(path))
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

// TreeIdentityToken returns an opaque, versioned, platform-tagged
// identity. Consumers must compare it through VerifyTreeIdentityToken and must
// not parse or persist assumptions about its payload encoding.
func (w Walker) TreeIdentityToken(path string) (string, error) {
	identity, err := w.resolveTreeIdentity(path)
	if err != nil {
		return "", err
	}
	return identity.token, nil
}

// TreeIdentityTokenInfo is the identity token of the directory info
// describes.
func TreeIdentityTokenInfo(info os.FileInfo) (string, error) {
	return platformTreeIdentityToken("", info)
}

// RelateTrees compares the physical identity of two directories.
func (w Walker) RelateTrees(a, b string) TreeRelation {
	left, leftErr := w.resolveTreeIdentity(a)
	right, rightErr := w.resolveTreeIdentity(b)
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

// VerifyTreeIdentityToken compares path with a token from TreeIdentityToken.
func (w Walker) VerifyTreeIdentityToken(path, token string) TreeRelation {
	if !ValidTreeIdentityToken(token) {
		return TreeRelationUnknown
	}
	current, err := w.TreeIdentityToken(path)
	if err != nil {
		return TreeRelationUnknown
	}
	if current == token {
		return TreeRelationSame
	}
	return TreeRelationDifferent
}

// VerifyTreeIdentityInfo compares the directory info describes with a token.
func VerifyTreeIdentityInfo(info os.FileInfo, token string) TreeRelation {
	if info == nil || !ValidTreeIdentityToken(token) {
		return TreeRelationUnknown
	}
	current, err := platformTreeIdentityToken("", info)
	if err != nil {
		return TreeRelationUnknown
	}
	if current == token {
		return TreeRelationSame
	}
	return TreeRelationDifferent
}

// SameTreeIdentity reports whether a and b name the same directory.
func (w Walker) SameTreeIdentity(a, b string) bool {
	left, le := w.resolveTreeIdentity(a)
	right, re := w.resolveTreeIdentity(b)
	return le == nil && re == nil && left.token == right.token
}

// ValidTreeIdentityToken reports whether token is a well-formed identity of
// this platform.
func ValidTreeIdentityToken(token string) bool {
	return validPlatformTreeIdentityToken(token)
}
