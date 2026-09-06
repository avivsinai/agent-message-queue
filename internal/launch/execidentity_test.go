//go:build unix

package launch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
