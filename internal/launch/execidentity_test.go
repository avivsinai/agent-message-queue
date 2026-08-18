//go:build unix

package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeExecutableIdentitySamePathInodeSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool")
	writeExec(t, path, "#!/bin/sh\necho one\n")
	first, err := ProbeExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".next"
	writeExec(t, replacement, "#!/bin/sh\necho two\n")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	second, err := ProbeExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.CanonicalPath != second.CanonicalPath || first.Type != ExecutableTypeFile {
		t.Fatalf("path/type changed: first=%#v second=%#v", first, second)
	}
	if first.Inode == second.Inode && first.FileID == second.FileID {
		t.Fatalf("inode swap kept identity first=%#v second=%#v", first, second)
	}
}

func TestProbeExecutableIdentitySymlinkRetarget(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	link := filepath.Join(dir, "tool")
	writeExec(t, a, "#!/bin/sh\necho a\n")
	writeExec(t, b, "#!/bin/sh\necho b\n")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	first, err := ProbeExecutableIdentity(link)
	if err != nil {
		t.Fatal(err)
	}
	resolvedA, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	if first.CanonicalPath != resolvedA || len(first.SymlinkChain) != 1 || first.SymlinkChain[0].Type != ExecutableTypeSymlink {
		t.Fatalf("first symlink probe = %#v", first)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b, link); err != nil {
		t.Fatal(err)
	}
	second, err := ProbeExecutableIdentity(link)
	if err != nil {
		t.Fatal(err)
	}
	resolvedB, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatal(err)
	}
	if second.CanonicalPath != resolvedB || second.Inode == first.Inode {
		t.Fatalf("retarget did not change identity first=%#v second=%#v", first, second)
	}
}

func TestProbeExecutableIdentityPATHRetarget(t *testing.T) {
	dir := t.TempDir()
	firstDir := filepath.Join(dir, "first")
	secondDir := filepath.Join(dir, "second")
	if err := os.MkdirAll(firstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(secondDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "amq-xgc-probe-shim"
	firstPath := filepath.Join(firstDir, name)
	secondPath := filepath.Join(secondDir, name)
	writeExec(t, firstPath, "#!/bin/sh\necho first\n")
	writeExec(t, secondPath, "#!/bin/sh\necho second\n")

	t.Setenv("PATH", secondDir+string(os.PathListSeparator)+firstDir)
	looked, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	viaSecond, err := ProbeExecutableIdentity(looked)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", firstDir)
	looked, err = exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	viaFirst, err := ProbeExecutableIdentity(looked)
	if err != nil {
		t.Fatal(err)
	}
	if viaSecond.CanonicalPath == viaFirst.CanonicalPath || viaSecond.Inode == viaFirst.Inode {
		t.Fatalf("PATH retarget kept identity second=%#v first=%#v", viaSecond, viaFirst)
	}
}

func TestProbeExecutableIdentityInPlaceRewriteChangesSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	writeExec(t, path, "#!/bin/sh\necho small\n")
	first, err := ProbeExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	writeExec(t, path, "#!/bin/sh\necho a-much-longer-payload-for-size\n")
	second, err := ProbeExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Size <= first.Size {
		t.Fatalf("in-place rewrite size first=%d second=%d", first.Size, second.Size)
	}
	if first.CanonicalPath != second.CanonicalPath {
		t.Fatalf("path changed first=%s second=%s", first.CanonicalPath, second.CanonicalPath)
	}
}

func TestMarshalExecutableIdentityFixedDecimalFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	writeExec(t, path, "#!/bin/sh\n")
	identity, err := ProbeExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalExecutableIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"canonical_path", "type", "dev", "inode", "volume_id", "file_id", "size", "mtime_ns", "symlink_chain",
	} {
		if !bytes.Contains(raw, []byte(`"`+field+`"`)) {
			t.Fatalf("canonical JSON missing %s: %s", field, raw)
		}
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if string(document["inode"]) == `""` || strings.Contains(string(document["inode"]), "e") ||
		strings.Contains(string(document["mtime_ns"]), "e") {
		t.Fatalf("inode/mtime must be decimal integers: %s", raw)
	}
	if !bytes.Equal(document["symlink_chain"], []byte("[]")) {
		t.Fatalf("empty chain = %s", document["symlink_chain"])
	}
}

func TestResolveConsultedExecutableMissingOmitsIdentity(t *testing.T) {
	result, err := ResolveConsultedExecutable("amq-xgc-missing-binary")
	if err != nil {
		t.Fatal(err)
	}
	if result.Requested != "amq-xgc-missing-binary" || result.Consulted != "amq-xgc-missing-binary" || result.Identity != nil {
		t.Fatalf("missing path = %#v", result)
	}
}

func TestPrepareV2SubjectBindsExecutableIdentityReplacements(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tool")
		writeExec(t, path, "#!/bin/sh\necho one\n")
		fixture := fixtureWithExecutable(t, path)
		first := prepareFixture(t, fixture, 0)
		second := prepareFixture(t, fixture, 0)
		if first.SubjectDigest != second.SubjectDigest || first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest {
			t.Fatalf("unchanged files churned digests first=%s/%s/%s second=%s/%s/%s",
				first.SubjectDigest, first.PlanDigest, first.TrustDigest, second.SubjectDigest, second.PlanDigest, second.TrustDigest)
		}
		assertCanonicalIdentity(t, first, path)
	})
	t.Run("same-path-rename-swap", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tool")
		writeExec(t, path, "#!/bin/sh\necho one\n")
		assertV2ReplacementChangesSubjectOnly(t, path, func() {
			replacement := path + ".next"
			writeExec(t, replacement, "#!/bin/sh\necho two\n")
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("symlink-retarget", func(t *testing.T) {
		dir := t.TempDir()
		a := filepath.Join(dir, "a")
		b := filepath.Join(dir, "b")
		link := filepath.Join(dir, "tool")
		writeExec(t, a, "#!/bin/sh\necho a\n")
		writeExec(t, b, "#!/bin/sh\necho b\n")
		if err := os.Symlink(a, link); err != nil {
			t.Fatal(err)
		}
		assertV2ReplacementChangesSubjectOnly(t, link, func() {
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(b, link); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("path-retarget", func(t *testing.T) {
		dir := t.TempDir()
		firstDir := filepath.Join(dir, "first")
		secondDir := filepath.Join(dir, "second")
		if err := os.MkdirAll(firstDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(secondDir, 0o700); err != nil {
			t.Fatal(err)
		}
		name := "amq-xgc-prepare-shim"
		writeExec(t, filepath.Join(firstDir, name), "#!/bin/sh\necho first\n")
		writeExec(t, filepath.Join(secondDir, name), "#!/bin/sh\necho second\n")
		t.Setenv("PATH", firstDir)
		fixture := fixtureWithExecutable(t, name)
		first := prepareFixture(t, fixture, 0)
		t.Setenv("PATH", secondDir)
		second := prepareFixture(t, fixture, 0)
		if first.SubjectDigest == second.SubjectDigest {
			t.Fatal("PATH retarget kept v2 subject digest")
		}
		if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest {
			t.Fatalf("PATH retarget churned plan/trust first=%s/%s second=%s/%s",
				first.PlanDigest, first.TrustDigest, second.PlanDigest, second.TrustDigest)
		}
	})
	t.Run("in-place-size-rewrite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tool")
		writeExec(t, path, "#!/bin/sh\necho small\n")
		assertV2ReplacementChangesSubjectOnly(t, path, func() {
			writeExec(t, path, "#!/bin/sh\necho a-much-longer-payload-for-size\n")
		})
	})
	t.Run("mtime-only-same-inode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tool")
		body := "#!/bin/sh\necho one\n"
		writeExec(t, path, body)
		fixture := fixtureWithExecutable(t, path)
		first := prepareFixture(t, fixture, 0)
		before, err := ProbeExecutableIdentity(path)
		if err != nil {
			t.Fatal(err)
		}
		writeExec(t, path, body)
		later := time.Unix(0, before.MtimeNS).Add(time.Second)
		if err := os.Chtimes(path, later, later); err != nil {
			t.Fatal(err)
		}
		second := prepareFixture(t, fixture, 0)
		after, err := ProbeExecutableIdentity(path)
		if err != nil {
			t.Fatal(err)
		}
		if before.Inode != after.Inode || before.Size != after.Size {
			t.Fatalf("mtime-only rewrite changed inode/size before=%#v after=%#v", before, after)
		}
		if before.MtimeNS == after.MtimeNS {
			t.Fatal("equal-length rewrite+Chtimes did not change mtime")
		}
		if first.SubjectDigest == second.SubjectDigest {
			t.Fatal("mtime-only rewrite kept v2 subject digest")
		}
		if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest {
			t.Fatalf("mtime-only rewrite churned plan/trust first=%s/%s second=%s/%s",
				first.PlanDigest, first.TrustDigest, second.PlanDigest, second.TrustDigest)
		}
	})
	t.Run("same-target-hop-retarget", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		hop1 := filepath.Join(dir, "hop1")
		hop2 := filepath.Join(dir, "hop2")
		link := filepath.Join(dir, "tool")
		writeExec(t, target, "#!/bin/sh\necho target\n")
		if err := os.Symlink(target, hop1); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, hop2); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(hop1, link); err != nil {
			t.Fatal(err)
		}
		fixture := fixtureWithExecutable(t, link)
		first := prepareFixture(t, fixture, 0)
		before, err := ProbeExecutableIdentity(link)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(hop2, link); err != nil {
			t.Fatal(err)
		}
		second := prepareFixture(t, fixture, 0)
		after, err := ProbeExecutableIdentity(link)
		if err != nil {
			t.Fatal(err)
		}
		if before.CanonicalPath != after.CanonicalPath || len(before.SymlinkChain) != 2 || len(after.SymlinkChain) != 2 {
			t.Fatalf("same-target hop retarget changed leaf or chain length before=%#v after=%#v", before, after)
		}
		if before.SymlinkChain[1].Inode == after.SymlinkChain[1].Inode {
			t.Fatalf("intermediate hop inode unchanged before=%#v after=%#v", before.SymlinkChain[1], after.SymlinkChain[1])
		}
		if first.SubjectDigest == second.SubjectDigest {
			t.Fatal("same-target hop retarget kept v2 subject digest")
		}
		if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest {
			t.Fatalf("same-target hop retarget churned plan/trust first=%s/%s second=%s/%s",
				first.PlanDigest, first.TrustDigest, second.PlanDigest, second.TrustDigest)
		}
	})
}

func TestPrepareV1SubjectIgnoresExecutableReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	writeExec(t, path, "#!/bin/sh\necho one\n")
	fixture := fixtureWithExecutable(t, path)
	first := prepareFixture(t, fixture, SubjectSchemaV1)
	replacement := path + ".next"
	writeExec(t, replacement, "#!/bin/sh\necho two\n")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	second := prepareFixture(t, fixture, SubjectSchemaV1)
	if first.SubjectDigest != second.SubjectDigest || first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest {
		t.Fatalf("v1 replacement churned digests first=%s/%s/%s second=%s/%s/%s",
			first.SubjectDigest, first.PlanDigest, first.TrustDigest, second.SubjectDigest, second.PlanDigest, second.TrustDigest)
	}
	if first.Participants[0].Executable != nil {
		t.Fatalf("v1 subject included executable identity: %#v", first.Participants[0].Executable)
	}
}

func TestApplyStaleV2SubjectAfterInodeSwapWritesNothing(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	path := filepath.Join(fixture.root, "tool")
	writeExec(t, path, "#!/bin/sh\necho one\n")
	fixture.request.Participants[0].Executable = path
	backend := &prepareTestBackend{}
	first, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Prepare: fixture.request, SubjectDigest: first.SubjectDigest}
	for _, action := range first.RequiredActions {
		request.Decisions = append(request.Decisions, ApplyDecision{ActionID: action.ActionID, Choice: action.AllowedDecisions[0]})
	}
	replacement := path + ".next"
	writeExec(t, replacement, "#!/bin/sh\necho two\n")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), request, ApplyDependencies{PrepareDependencies: fixture.dependencies(backend)})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReasonCode != "subject_changed" {
		t.Fatalf("Apply = %#v, want subject_changed", result)
	}
	if backend.creates != 0 {
		t.Fatalf("stale Apply created backend resources: %d", backend.creates)
	}
	for _, name := range []string{bindingFilename, journalFilename} {
		if _, err := os.Stat(filepath.Join(fixture.sessionRoot, bindingDirectory, name)); !os.IsNotExist(err) {
			t.Fatalf("stale Apply wrote %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.sessionRoot, executionDirectory)); !os.IsNotExist(err) {
		t.Fatalf("stale Apply wrote tickets: %v", err)
	}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func fixtureWithExecutable(t *testing.T, executable string) internalPrepareFixture {
	t.Helper()
	fixture := newInternalPrepareFixture(t)
	fixture.request.Participants[0].Executable = executable
	return fixture
}

func prepareFixture(t *testing.T, fixture internalPrepareFixture, schema int) PrepareResult {
	t.Helper()
	request := fixture.request
	request.SubjectSchema = schema
	result, err := Prepare(context.Background(), request, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertV2ReplacementChangesSubjectOnly(t *testing.T, executable string, mutate func()) {
	t.Helper()
	fixture := fixtureWithExecutable(t, executable)
	first := prepareFixture(t, fixture, 0)
	mutate()
	second := prepareFixture(t, fixture, 0)
	if first.SubjectDigest == second.SubjectDigest {
		t.Fatal("replacement kept v2 subject digest")
	}
	if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest {
		t.Fatalf("replacement churned plan/trust first=%s/%s second=%s/%s",
			first.PlanDigest, first.TrustDigest, second.PlanDigest, second.TrustDigest)
	}
}

func assertCanonicalIdentity(t *testing.T, result PrepareResult, path string) {
	t.Helper()
	if len(result.Participants) != 1 || result.Participants[0].Executable == nil {
		t.Fatalf("missing executable identity: %#v", result.Participants)
	}
	got := result.Participants[0].Executable
	probed, err := ProbeExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := MarshalExecutableIdentity(probed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Identity, want) {
		t.Fatalf("subject identity %s != marshal %s", got.Identity, want)
	}
}
