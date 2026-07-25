//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

func replaceWakeRepairFloorWithNewInodeForTest(
	t *testing.T,
	root, me string,
	mutate bool,
) []byte {
	t.Helper()
	path := wakeRepairFloorPath(root, me)
	original, err := os.Open(path)
	if err != nil {
		t.Fatalf("open original wake repair floor: %v", err)
	}
	defer func() { _ = original.Close() }()
	originalInfo, err := original.Stat()
	if err != nil {
		t.Fatalf("stat original wake repair floor: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original wake repair floor: %v", err)
	}
	replacement := append([]byte(nil), raw...)
	if mutate {
		var floor wakeRepairFloor
		if err := json.Unmarshal(raw, &floor); err != nil {
			t.Fatalf("decode original wake repair floor: %v", err)
		}
		if floor.Existing == nil {
			floor.Existing = make(map[string]wakeFileIdentity)
		}
		floor.Existing["replacement.md"] = wakeFileIdentity{
			Device:    91,
			Inode:     92,
			CTimeSec:  93,
			CTimeNsec: 94,
		}
		replacement, err = json.MarshalIndent(floor, "", "  ")
		if err != nil {
			t.Fatalf("encode replacement wake repair floor: %v", err)
		}
		replacement = append(replacement, '\n')
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("unlink original wake repair floor: %v", err)
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatalf("write replacement wake repair floor: %v", err)
	}
	replacementInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat replacement wake repair floor: %v", err)
	}
	if sameWakeFileIdentity(originalInfo, replacementInfo) {
		t.Fatal("replacement wake repair floor reused the original file identity")
	}
	if !mutate && !bytes.Equal(raw, replacement) {
		t.Fatal("byte-identical replacement changed floor bytes")
	}
	return replacement
}

func TestWakeRepairMetadataAtUsesRetainedAgentDirectory(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatalf("openWakeAgentDir: %v", err)
	}
	defer func() { _ = agentDir.Close() }()

	injector := writeExecutableForTest(t, "repair-at-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-a"})
	targetDigest := mustWakeTargetDigest(target)
	floor := wakeRepairFloor{
		Schema:            wakeRepairFloorSchema,
		Root:              canonicalWakeRoot(root),
		RootIdentity:      mustTreeIdentityTokenForTest(t, root),
		Agent:             "codex",
		Generation:        "child-generation",
		SourceGeneration:  "source-generation",
		SourceFloorDigest: "sha256:source-floor",
		TargetDigest:      targetDigest,
		BootID:            wakeRepairTestBootID,
		Existing:          map[string]wakeFileIdentity{},
	}
	lock := bindWakeLockToTarget(wakeLock{
		PID:               os.Getpid(),
		Root:              canonicalWakeRoot(root),
		Agent:             "codex",
		Generation:        floor.Generation,
		SourceGeneration:  floor.SourceGeneration,
		SourceFloorDigest: floor.SourceFloorDigest,
		BootID:            wakeRepairTestBootID,
	}, target)

	originalAgentPath := fsq.AgentBase(root, "codex")
	detachedAgentPath := filepath.Join(filepath.Dir(originalAgentPath), "codex-detached")
	if err := os.Rename(originalAgentPath, detachedAgentPath); err != nil {
		t.Fatalf("detach agent directory: %v", err)
	}
	if err := os.Mkdir(originalAgentPath, 0o700); err != nil {
		t.Fatalf("create replacement agent directory: %v", err)
	}

	err = agentDir.withFD(func(dirfd int) error {
		if err := writeWakeTargetGuardedAt(dirfd, agentDir, root, "codex", target); err != nil {
			return err
		}
		if err := writeWakeRepairFloorAt(dirfd, agentDir, root, floor); err != nil {
			return err
		}
		return createWakeRepairLockAt(
			dirfd,
			agentDir,
			root,
			"codex",
			floor.RootIdentity,
			lock,
		)
	})
	if err != nil {
		t.Fatalf("write retained repair metadata: %v", err)
	}

	for _, name := range []string{wakeTargetFileName, wakeRepairFloorFileName, ".wake.lock"} {
		if _, err := os.Stat(filepath.Join(originalAgentPath, name)); !os.IsNotExist(err) {
			t.Fatalf("replacement path unexpectedly received %s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(detachedAgentPath, name)); err != nil {
			t.Fatalf("retained directory missing %s: %v", name, err)
		}
	}

	err = agentDir.withFD(func(dirfd int) error {
		persistedTarget, exists, err := readWakeTargetAt(dirfd, agentDir, root, "codex")
		if err != nil || !exists || !sameWakeTarget(persistedTarget, target) {
			t.Fatalf("retained target = (%#v,%v,%v), want exact target", persistedTarget, exists, err)
		}
		persistedFloor, exists, err := readWakeRepairFloorAt(dirfd, agentDir)
		if err != nil || !exists || persistedFloor.Generation != floor.Generation {
			t.Fatalf("retained floor = (%#v,%v,%v), want child floor", persistedFloor, exists, err)
		}
		inspection := inspectWakeLockAt(dirfd, agentDir, root, "codex")
		if !inspection.Exists || inspection.Lock.Generation != lock.Generation {
			t.Fatalf("retained lock = %#v, want child lock", inspection)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read retained repair metadata: %v", err)
	}
}

func TestRemoveWakeRepairFloorAtRequiresChildGenerationAndSourceDigest(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatalf("openWakeAgentDir: %v", err)
	}
	defer func() { _ = agentDir.Close() }()

	floor := wakeRepairFloor{
		Schema:            wakeRepairFloorSchema,
		Root:              canonicalWakeRoot(root),
		RootIdentity:      mustTreeIdentityTokenForTest(t, root),
		Agent:             "codex",
		Generation:        "child-generation",
		SourceGeneration:  "source-generation",
		SourceFloorDigest: "sha256:source-floor",
		TargetDigest:      "sha256:target",
		BootID:            wakeRepairTestBootID,
		Existing:          map[string]wakeFileIdentity{},
	}

	err = agentDir.withFD(func(dirfd int) error {
		if err := writeWakeRepairFloorAt(dirfd, agentDir, root, floor); err != nil {
			return err
		}
		snapshot, exists, err := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
		if err != nil || !exists {
			t.Fatalf("read exact floor authority: exists=%v err=%v", exists, err)
		}
		exactAuthority, err := newWakeRepairFloorAuthority(snapshot)
		if err != nil {
			t.Fatalf("capture exact floor authority: %v", err)
		}
		wrongGeneration := exactAuthority
		wrongGeneration.ChildGeneration = "replacement-generation"
		if err := removeWakeRepairFloorIfGenerationGuardedAt(
			dirfd,
			agentDir,
			wrongGeneration,
		); err != nil {
			return err
		}
		if _, exists, err := readWakeRepairFloorAt(dirfd, agentDir); err != nil || !exists {
			t.Fatalf("wrong child generation removed floor: exists=%v err=%v", exists, err)
		}
		wrongSource := exactAuthority
		wrongSource.SourceFloorDigest = "sha256:replacement-source-floor"
		if err := removeWakeRepairFloorIfGenerationGuardedAt(dirfd, agentDir, wrongSource); err != nil {
			return err
		}
		if _, exists, err := readWakeRepairFloorAt(dirfd, agentDir); err != nil || !exists {
			t.Fatalf("wrong source digest removed floor: exists=%v err=%v", exists, err)
		}
		if err := removeWakeRepairFloorIfGenerationGuardedAt(
			dirfd,
			agentDir,
			exactAuthority,
		); err != nil {
			return err
		}
		if _, exists, err := readWakeRepairFloorAt(dirfd, agentDir); err != nil || exists {
			t.Fatalf("exact lineage floor survived: exists=%v err=%v", exists, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("generation-safe floor removal: %v", err)
	}
}

func TestRemoveWakeRepairFloorQuarantineRestoresInjectedReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate bool
	}{
		{name: "byte-identical new inode"},
		{name: "same generation and source digest changed bytes", mutate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRemoveWakeRepairFloorQuarantineRestoresInjectedReplacement(t, tc.mutate)
		})
	}
}

func testRemoveWakeRepairFloorQuarantineRestoresInjectedReplacement(
	t *testing.T,
	mutate bool,
) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatalf("openWakeAgentDir: %v", err)
	}
	defer func() { _ = agentDir.Close() }()
	floor := wakeRepairFloor{
		Schema:            wakeRepairFloorSchema,
		Root:              canonicalWakeRoot(root),
		RootIdentity:      mustTreeIdentityTokenForTest(t, root),
		Agent:             "codex",
		Generation:        "child-generation",
		SourceGeneration:  "source-generation",
		SourceFloorDigest: "sha256:source-floor",
		TargetDigest:      "sha256:target",
		BootID:            wakeRepairTestBootID,
		Existing:          map[string]wakeFileIdentity{},
	}

	var replacement []byte
	err = agentDir.withFD(func(dirfd int) error {
		if err := writeWakeRepairFloorAt(dirfd, agentDir, root, floor); err != nil {
			return err
		}
		snapshot, exists, err := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
		if err != nil || !exists {
			t.Fatalf("read original floor authority: exists=%v err=%v", exists, err)
		}
		authority, err := newWakeRepairFloorAuthority(snapshot)
		if err != nil {
			t.Fatalf("capture original floor authority: %v", err)
		}
		originalRename := renameWakeRepairFloorNoReplaceAt
		injected := false
		renameWakeRepairFloorNoReplaceAt = func(fromDirFD int, from string, toDirFD int, to string) error {
			if !injected && from == wakeRepairFloorFileName {
				injected = true
				replacement = replaceWakeRepairFloorWithNewInodeForTest(t, root, "codex", mutate)
			}
			return renameWakeRepairNoReplaceAt(fromDirFD, from, toDirFD, to)
		}
		defer func() { renameWakeRepairFloorNoReplaceAt = originalRename }()

		err = removeWakeRepairFloorIfGenerationGuardedAt(dirfd, agentDir, authority)
		if err == nil || !strings.Contains(err.Error(), "changed before cleanup") {
			t.Fatalf("injected replacement removal error = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("quarantine replacement race: %v", err)
	}
	got, err := os.ReadFile(wakeRepairFloorPath(root, "codex"))
	if err != nil {
		t.Fatalf("restored replacement floor is missing: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatal("restored replacement floor bytes changed")
	}
	entries, err := os.ReadDir(fsq.AgentBase(root, "codex"))
	if err != nil {
		t.Fatalf("read wake agent directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), wakeRepairFloorQuarantinePrefix) {
			t.Fatalf("restored replacement left quarantine %s", entry.Name())
		}
	}
}

func TestRemoveWakeRepairFloorQuarantineNeverOverwritesNewerCanonicalFloor(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatalf("openWakeAgentDir: %v", err)
	}
	defer func() { _ = agentDir.Close() }()
	floor := wakeRepairFloor{
		Schema:            wakeRepairFloorSchema,
		Root:              canonicalWakeRoot(root),
		RootIdentity:      mustTreeIdentityTokenForTest(t, root),
		Agent:             "codex",
		Generation:        "child-generation",
		SourceGeneration:  "source-generation",
		SourceFloorDigest: "sha256:source-floor",
		TargetDigest:      "sha256:target",
		BootID:            wakeRepairTestBootID,
		Existing:          map[string]wakeFileIdentity{},
	}
	newer := floor
	newer.Existing = map[string]wakeFileIdentity{
		"newer.md": {Device: 101, Inode: 102, CTimeSec: 103, CTimeNsec: 104},
	}

	var replacement []byte
	var quarantine string
	err = agentDir.withFD(func(dirfd int) error {
		if err := writeWakeRepairFloorAt(dirfd, agentDir, root, floor); err != nil {
			return err
		}
		snapshot, exists, err := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
		if err != nil || !exists {
			t.Fatalf("read original floor authority: exists=%v err=%v", exists, err)
		}
		authority, err := newWakeRepairFloorAuthority(snapshot)
		if err != nil {
			t.Fatalf("capture original floor authority: %v", err)
		}
		originalRename := renameWakeRepairFloorNoReplaceAt
		renameCount := 0
		renameWakeRepairFloorNoReplaceAt = func(fromDirFD int, from string, toDirFD int, to string) error {
			renameCount++
			switch renameCount {
			case 1:
				replacement = replaceWakeRepairFloorWithNewInodeForTest(t, root, "codex", true)
				quarantine = to
			case 2:
				if from != quarantine || to != wakeRepairFloorFileName {
					t.Fatalf("restore rename = %q -> %q, want %q -> %q", from, to, quarantine, wakeRepairFloorFileName)
				}
				if err := writeWakeRepairFloorAt(dirfd, agentDir, root, newer); err != nil {
					t.Fatalf("install newer canonical floor: %v", err)
				}
			}
			return renameWakeRepairNoReplaceAt(fromDirFD, from, toDirFD, to)
		}
		defer func() { renameWakeRepairFloorNoReplaceAt = originalRename }()

		err = removeWakeRepairFloorIfGenerationGuardedAt(dirfd, agentDir, authority)
		if err == nil || !strings.Contains(err.Error(), "preserved mismatch as") {
			t.Fatalf("occupied restore error = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("quarantine occupied restore race: %v", err)
	}
	gotNewer, err := os.ReadFile(wakeRepairFloorPath(root, "codex"))
	if err != nil {
		t.Fatalf("newer canonical floor is missing: %v", err)
	}
	wantNewer, err := json.MarshalIndent(newer, "", "  ")
	if err != nil {
		t.Fatalf("marshal newer canonical floor: %v", err)
	}
	wantNewer = append(wantNewer, '\n')
	if !bytes.Equal(gotNewer, wantNewer) {
		t.Fatal("mismatched quarantine overwrote newer canonical floor")
	}
	gotQuarantine, err := os.ReadFile(filepath.Join(fsq.AgentBase(root, "codex"), quarantine))
	if err != nil {
		t.Fatalf("mismatched quarantine was deleted: %v", err)
	}
	if !bytes.Equal(gotQuarantine, replacement) {
		t.Fatal("preserved mismatch quarantine bytes changed")
	}
}

func TestRenameWakeRepairNoReplaceAtNeverOverwritesDestination(t *testing.T) {
	dir := secureTempDirForTest(t)
	fromPath := filepath.Join(dir, "from")
	toPath := filepath.Join(dir, "to")
	if err := os.WriteFile(fromPath, []byte("from"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(toPath, []byte("to"), 0o600); err != nil {
		t.Fatalf("write destination: %v", err)
	}
	file, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open rename directory: %v", err)
	}
	defer func() { _ = file.Close() }()
	dirfd := int(file.Fd())

	err = renameWakeRepairNoReplaceAt(dirfd, "from", dirfd, "to")
	if !errors.Is(err, unix.EEXIST) {
		t.Fatalf("occupied no-replace rename error = %v, want EEXIST", err)
	}
	for path, want := range map[string]string{fromPath: "from", toPath: "to"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("no-replace result %s = %q, %v; want %q", path, got, err, want)
		}
	}
	if err := os.Remove(toPath); err != nil {
		t.Fatalf("remove destination: %v", err)
	}
	if err := renameWakeRepairNoReplaceAt(dirfd, "from", dirfd, "to"); err != nil {
		t.Fatalf("unoccupied no-replace rename: %v", err)
	}
	if _, err := os.Lstat(fromPath); !os.IsNotExist(err) {
		t.Fatalf("source survived successful rename: %v", err)
	}
	if got, err := os.ReadFile(toPath); err != nil || string(got) != "from" {
		t.Fatalf("renamed destination = %q, %v; want source bytes", got, err)
	}
}

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
