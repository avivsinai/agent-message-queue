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
	if !wakeReadyFileMatches(correct, generation, digest) {
		t.Fatal("correct marker did not match schema/generation/digest")
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
	if wakeReadyFileMatches(wrongGeneration, generation, digest) {
		t.Fatal("wrong generation matched expected marker")
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
	if wakeReadyFileMatches(wrongDigest, generation, digest) {
		t.Fatal("wrong digest matched expected marker")
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
