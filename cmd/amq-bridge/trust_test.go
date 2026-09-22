package main

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
)

// TestTrustAddProvisionsFromIdentityPublicOutput is the ug3 happy path:
// given a fresh destination root, the operator runs `identity public` on the
// source, pipes that output to `trust add --host <h>` on the destination,
// and the written trusted file authenticates under LoadTrusted with no
// hand-written file. Before the fix, the one-line identity-public output was
// rejected by the trusted-file parser ("line 1 is invalid") and WriteTrusted
// had no production caller.
func TestTrustAddProvisionsFromIdentityPublicOutput(t *testing.T) {
	// Source: a real identity, and the exact bytes `identity public` prints.
	srcRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, srcRoot, "src-mac")
	srcKey := testHostKey("src-mac", "g7")
	if err := bridge.WriteIdentity(srcRoot, srcKey); err != nil {
		t.Fatal(err)
	}
	publicOutput := fmt.Sprintf("host=src-mac generation=%s public=%x\n", srcKey.Generation, srcKey.Public())

	// Destination: fresh root, pipe the record through trust add.
	dstRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, dstRoot, "dst-mac")
	if err := runTrustAdd([]string{"--root", dstRoot, "--host", "src-mac"}, strings.NewReader(publicOutput)); err != nil {
		t.Fatalf("runTrustAdd: %v", err)
	}

	got, generation, err := bridge.LoadTrusted(dstRoot, "src-mac")
	if err != nil {
		t.Fatalf("LoadTrusted after provisioning: %v", err)
	}
	if generation != srcKey.Generation {
		t.Fatalf("trusted generation = %q, want %q", generation, srcKey.Generation)
	}
	if string(got) != string(srcKey.Public()) {
		t.Fatalf("trusted public key mismatch")
	}
	// File mode must be private (writePrivateFile contract, 0600).
	info, err := os.Lstat(bridge.TrustedPath(dstRoot, "src-mac"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("trusted file mode = %o, want 600", perm)
	}
}

// TestTrustAddAcceptsKeyFileForm pins that the two-line record the loader
// itself accepts ("generation <g>" / "public <hex>") provisions too — the
// same command must take either shape.
func TestTrustAddAcceptsKeyFileForm(t *testing.T) {
	key := testHostKey("src-two", "g9")
	record := fmt.Sprintf("generation %s\npublic %x\n", key.Generation, key.Public())
	dstRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, dstRoot, "dst-mac")
	if err := runTrustAdd([]string{"--root", dstRoot, "--host", "src-two"}, strings.NewReader(record)); err != nil {
		t.Fatalf("runTrustAdd: %v", err)
	}
	got, generation, err := bridge.LoadTrusted(dstRoot, "src-two")
	if err != nil {
		t.Fatalf("LoadTrusted: %v", err)
	}
	if generation != "g9" || string(got) != string(key.Public()) {
		t.Fatalf("trusted = (%x, %s), want the provisioned key", got, generation)
	}
}

// TestTrustAddFromFileAndRejections covers the --from path and the refusal
// shapes: a bad host alias, a malformed record, and a short public key must
// all refuse without writing a trusted file.
func TestTrustAddFromFileAndRejections(t *testing.T) {
	key := testHostKey("src-file", "g1")
	from := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(from, []byte(fmt.Sprintf("host=src-file generation=%s public=%x\n", key.Generation, key.Public())), 0o600); err != nil {
		t.Fatal(err)
	}
	dstRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, dstRoot, "dst-mac")
	if err := runTrustAdd([]string{"--root", dstRoot, "--host", "src-file", "--from", from}, nil); err != nil {
		t.Fatalf("runTrustAdd --from: %v", err)
	}
	if _, _, err := bridge.LoadTrusted(dstRoot, "src-file"); err != nil {
		t.Fatalf("LoadTrusted: %v", err)
	}

	// Refusals leave no trusted file behind.
	for name, tc := range map[string]struct {
		host string
		data string
	}{
		"bad-host":    {host: "Bad Host", data: fmt.Sprintf("host=x generation=%s public=%x\n", key.Generation, key.Public())},
		"bad-record":  {host: "src-x", data: "not a record at all\n"},
		"short-pub":   {host: "src-x", data: fmt.Sprintf("host=x generation=%s public=%x\n", key.Generation, []byte("short"))},
		"missing-pub": {host: "src-x", data: "host=x\n"},
	} {
		t.Run(name, func(t *testing.T) {
			root := newBridgeRoot(t, "claude")
			ensureHostID(t, root, "dst-mac")
			err := runTrustAdd([]string{"--root", root, "--host", tc.host}, strings.NewReader(tc.data))
			if err == nil {
				t.Fatalf("runTrustAdd(%s) = nil, want refusal", name)
			}
			if _, err := os.Lstat(bridge.TrustedPath(root, tc.host)); !os.IsNotExist(err) {
				t.Fatalf("trusted file written despite refusal: %v", err)
			}
		})
	}
	_ = ed25519.PublicKeySize
}
