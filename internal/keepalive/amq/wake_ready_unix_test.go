//go:build unix

package amq

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWakeReadyFileParseContract(t *testing.T) {
	const generation = "gen-1"
	const digest = "digest-1"
	dir := t.TempDir()

	correct := filepath.Join(dir, "correct")
	writeWakeReadyMarker(t, correct, wakeReadyMarker{
		Schema: wakeReadySchema, Generation: generation, TargetDigest: digest,
	})
	if !wakeReadyFileExists(correct) {
		t.Fatal("correct marker is not ready")
	}
	got, err := readWakeReadyFile(correct)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != generation || got.TargetDigest != digest {
		t.Fatalf("correct marker = %#v, want generation/digest", got)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if wakeReadyFileExists(empty) {
		t.Fatal("empty file is ready")
	}

	asDir := filepath.Join(dir, "as-dir")
	if err := os.Mkdir(asDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if wakeReadyFileExists(asDir) {
		t.Fatal("directory is ready")
	}

	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink(correct, symlink); err != nil {
		t.Fatal(err)
	}
	if wakeReadyFileExists(symlink) {
		t.Fatal("symlink is ready")
	}

	malformed := filepath.Join(dir, "malformed")
	if err := os.WriteFile(malformed, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if wakeReadyFileExists(malformed) {
		t.Fatal("malformed marker is ready")
	}

	wrongSchema := filepath.Join(dir, "wrong-schema")
	writeWakeReadyMarker(t, wrongSchema, wakeReadyMarker{
		Schema: 2, Generation: generation, TargetDigest: digest,
	})
	if wakeReadyFileExists(wrongSchema) {
		t.Fatal("wrong schema is ready")
	}

	wrongGeneration := filepath.Join(dir, "wrong-generation")
	writeWakeReadyMarker(t, wrongGeneration, wakeReadyMarker{
		Schema: wakeReadySchema, Generation: "other-gen", TargetDigest: digest,
	})
	got, err = readWakeReadyFile(wrongGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation == generation {
		t.Fatal("wrong generation parsed as expected generation")
	}

	emptyGeneration := filepath.Join(dir, "empty-generation")
	writeWakeReadyMarker(t, emptyGeneration, wakeReadyMarker{
		Schema: wakeReadySchema, TargetDigest: digest,
	})
	if wakeReadyFileExists(emptyGeneration) {
		t.Fatal("empty generation is ready")
	}

	wrongDigest := filepath.Join(dir, "wrong-digest")
	writeWakeReadyMarker(t, wrongDigest, wakeReadyMarker{
		Schema: wakeReadySchema, Generation: generation, TargetDigest: "other-digest",
	})
	got, err = readWakeReadyFile(wrongDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetDigest == digest {
		t.Fatal("wrong digest parsed as expected digest")
	}

	mode0644 := filepath.Join(dir, "mode-0644")
	writeWakeReadyMarker(t, mode0644, wakeReadyMarker{
		Schema: wakeReadySchema, Generation: generation, TargetDigest: digest,
	})
	if err := os.Chmod(mode0644, 0o644); err != nil {
		t.Fatal(err)
	}
	if wakeReadyFileExists(mode0644) {
		t.Fatal("0644 marker is ready")
	}

	swapped := filepath.Join(dir, "swapped")
	writeWakeReadyMarker(t, swapped, wakeReadyMarker{
		Schema: wakeReadySchema, Generation: generation, TargetDigest: digest,
	})
	replacement := filepath.Join(dir, "replacement")
	writeWakeReadyMarker(t, replacement, wakeReadyMarker{
		Schema: wakeReadySchema, Generation: "other-gen", TargetDigest: digest,
	})
	t.Cleanup(func() { afterWakeReadyLstatForTest = nil })
	afterWakeReadyLstatForTest = func(path string) {
		if path != swapped {
			return
		}
		if err := os.Remove(path); err != nil {
			t.Errorf("remove swapped marker: %v", err)
			return
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Errorf("swap marker inode: %v", err)
		}
	}
	if _, err := readWakeReadyFile(swapped); err == nil {
		t.Fatal("inode swap during open was accepted")
	}
}

func writeWakeReadyMarker(t *testing.T, path string, marker wakeReadyMarker) {
	t.Helper()
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
