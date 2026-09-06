//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRevalidateWakeRepairRootIdentityRejectsPathReplacement(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create root: %v", err)
	}
	identity := mustTreeIdentityTokenForTest(t, root)
	if err := revalidateWakeRepairRootIdentity(root, identity); err != nil {
		t.Fatalf("revalidate unchanged root: %v", err)
	}
	if err := os.Rename(root, filepath.Join(parent, "root-detached")); err != nil {
		t.Fatalf("detach root: %v", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create replacement root: %v", err)
	}
	err := revalidateWakeRepairRootIdentity(root, identity)
	if err == nil || !strings.Contains(err.Error(), "root identity changed") {
		t.Fatalf("replacement root error = %v, want identity-change refusal", err)
	}
}

func mustTreeIdentityTokenForTest(t *testing.T, path string) string {
	t.Helper()
	token, err := resolveTreeIdentityToken(path)
	if err != nil {
		t.Fatalf("resolveTreeIdentityToken(%s): %v", path, err)
	}
	return token
}
