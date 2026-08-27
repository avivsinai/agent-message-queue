package selfupgrade

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCaptureImageEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	evidence, err := CaptureImageEvidence(path, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Schema != ImageEvidenceSchemaV1 || evidence.Platform != runtime.GOOS ||
		evidence.Method != ImageMethodPathnameObserved || evidence.ExecutionPath != path ||
		evidence.EmbeddedVersion != "1.2.3" {
		t.Fatalf("evidence = %#v", evidence)
	}
	if err := ValidateImageEvidence(evidence); err != nil {
		t.Fatalf("ValidateImageEvidence: %v", err)
	}
}

func TestCaptureImageEvidenceRejectsHashTimeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := CaptureImageEvidenceWithMutator(path, "1.2.3", func() {
		if writeErr := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); writeErr != nil {
			t.Errorf("mutate image: %v", writeErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "changed while hashing") {
		t.Fatalf("error = %v, want hash-time identity refusal", err)
	}
}

func TestVersionStrictlyNewer(t *testing.T) {
	tests := []struct {
		incumbent string
		candidate string
		want      bool
	}{
		{incumbent: "1.2.3", candidate: "1.2.4", want: true},
		{incumbent: "1.2.3", candidate: "1.2.3", want: false},
		{incumbent: "1.2.3", candidate: "1.2.2", want: false},
		{incumbent: "1.2.3-rc.1", candidate: "1.2.3", want: true},
		{incumbent: "1.2.3", candidate: "1.2.3+build.1", want: false},
		{incumbent: "unknown", candidate: "9.9.9", want: false},
	}
	for _, test := range tests {
		if got := VersionStrictlyNewer(test.incumbent, test.candidate); got != test.want {
			t.Errorf("VersionStrictlyNewer(%q, %q) = %t, want %t", test.incumbent, test.candidate, got, test.want)
		}
	}
}

func TestRememberRefusalBoundsAndMovesDuplicates(t *testing.T) {
	template := ImageEvidence{
		Platform:        "darwin",
		Device:          1,
		Size:            10,
		SHA256:          "sha256:" + strings.Repeat("a", 64),
		EmbeddedVersion: "1.0.0",
	}
	var remembered []RefusedCandidate
	var evidence []ImageEvidence
	for index := 0; index < RefusalLimit+2; index++ {
		candidate := template
		candidate.Inode = uint64(index + 1)
		evidence = append(evidence, candidate)
		remembered = RememberRefusal(remembered, candidate)
	}
	if len(remembered) != RefusalLimit {
		t.Fatalf("remembered=%d, want %d", len(remembered), RefusalLimit)
	}
	if RefusedCandidatesContain(remembered, evidence[0]) ||
		!RefusedCandidatesContain(remembered, evidence[len(evidence)-1]) {
		t.Fatalf("bounded refusal memory = %#v", remembered)
	}
	remembered = RememberRefusal(remembered, evidence[2])
	if remembered[len(remembered)-1] != RefusedCandidateFromEvidence(evidence[2]) {
		t.Fatalf("duplicate refusal was not moved to the end: %#v", remembered)
	}
}

func TestSameDarwinStagedImageEvidenceIgnoresPathAndCTime(t *testing.T) {
	first := ImageEvidence{
		Schema:          ImageEvidenceSchemaV1,
		Platform:        "darwin",
		Method:          ImageMethodPathnameObserved,
		ExecutionPath:   "/old/amq",
		Device:          1,
		Inode:           2,
		Size:            3,
		CTimeNS:         4,
		SHA256:          "sha256:" + strings.Repeat("a", 64),
		EmbeddedVersion: "1.0.0",
	}
	second := first
	second.Method = ImageMethodPathnameExecObserved
	second.ExecutionPath = "/stage/amq"
	second.CTimeNS++
	if !SameDarwinStagedImageEvidence(first, second) {
		t.Fatalf("staged evidence was not equivalent: first=%#v second=%#v", first, second)
	}
	second.SHA256 = "sha256:" + strings.Repeat("b", 64)
	if SameDarwinStagedImageEvidence(first, second) {
		t.Fatal("different staged image content was accepted")
	}
}
