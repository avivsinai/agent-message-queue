//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTreeRelationUsesPhysicalIdentity(t *testing.T) {
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if got := relateTrees(realRoot, alias); got != TreeRelationSame {
		t.Fatalf("relateTrees(real, alias) = %v, want Same", got)
	}
	if got := relateTrees(realRoot, t.TempDir()); got != TreeRelationDifferent {
		t.Fatalf("relateTrees(distinct roots) = %v, want Different", got)
	}
	if got := relateTrees(realRoot, filepath.Join(t.TempDir(), "missing")); got != TreeRelationUnknown {
		t.Fatalf("relateTrees(real, missing) = %v, want Unknown", got)
	}
}

func TestTreeIdentityTokenIsOpaqueAndPlatformTagged(t *testing.T) {
	token, err := resolveTreeIdentityToken(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "v1:" + runtime.GOOS + ":"
	if !strings.HasPrefix(token, wantPrefix) {
		t.Fatalf("token %q does not have platform/version prefix %q", token, wantPrefix)
	}
	if got := verifyTreeIdentityToken(t.TempDir(), wantPrefix+"malformed"); got != TreeRelationUnknown {
		t.Fatalf("malformed token relation = %v, want Unknown", got)
	}
}

func TestTreeIdentityCaseInsensitiveAlias(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, ".probe-case")
	if err := os.MkdirAll(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, ".PROBE-CASE")
	if _, err := os.Stat(alias); err != nil {
		t.Skip("case-sensitive filesystem")
	}

	want, err := resolveTreeIdentityToken(probe)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveTreeIdentityToken(alias)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("case-insensitive aliases have different identity tokens: %q != %q", got, want)
	}
}
