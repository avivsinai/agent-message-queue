//go:build darwin || linux

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCaptureWakeImageEvidenceBindsStableRegularExecutable(t *testing.T) {
	dir := secureTempDirForTest(t)
	path := filepath.Join(dir, "amq-0.50.2")
	content := []byte("test executable image\n")
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatal(err)
	}

	evidence, err := captureWakeImageEvidence(path, "0.50.2")
	if err != nil {
		t.Fatalf("capture image evidence: %v", err)
	}
	wantDigest := sha256.Sum256(content)
	if evidence.ExecutionPath != path || evidence.Size != int64(len(content)) ||
		evidence.SHA256 != "sha256:"+hex.EncodeToString(wantDigest[:]) ||
		evidence.EmbeddedVersion != "0.50.2" || evidence.Device == 0 ||
		evidence.Inode == 0 || evidence.CTimeNS <= 0 {
		t.Fatalf("evidence = %#v", evidence)
	}
	if evidence.Method != wakeImageMethodPathnameObserved {
		t.Fatalf("path-opened image method = %q, want observational evidence", evidence.Method)
	}
	if err := validateWakeImageEvidence(evidence); err != nil {
		t.Fatalf("captured evidence is invalid: %v", err)
	}
}

func TestLinuxFabricatedPathEvidenceCannotAuthorizeResume(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pathname evidence invariant")
	}
	dir := secureTempDirForTest(t)
	path := filepath.Join(dir, "fabricated-amq")
	if err := os.WriteFile(path, []byte("not the running image\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	evidence, err := captureWakeImageEvidence(path, "0.50.2")
	if err != nil {
		t.Fatalf("capture fabricated path evidence: %v", err)
	}
	evidence.Platform = "linux"
	if evidence.Method != wakeImageMethodPathnameObserved {
		t.Fatalf("fabricated path evidence method = %q, want observational evidence", evidence.Method)
	}
	if err := validateWakeImageEvidenceForPlatform(evidence, "linux"); err != nil {
		t.Fatalf("Linux observational image evidence should remain valid diagnostics: %v", err)
	}

	lock := validWakeResumeLockForTest()
	lock.Root = canonicalWakeRoot("/queue")
	lock.RunningImageEvidence = &evidence
	lock.ImagePath = evidence.ExecutionPath
	lock.ImageVersion = evidence.EmbeddedVersion
	lock.ResumeSignal = wakeResumeSignalUSR1
	lock.ControlSocket = ""

	err = validateWakeResumeAdvertisementWithContext(
		lock,
		lock.Root,
		lock.Agent,
		"linux",
		"",
	)
	if err != nil {
		t.Fatalf("Linux persisted pathname diagnostics should remain a valid advertisement: %v", err)
	}
	bootstrap := wakeResumeBootstrap{
		Schema:     wakeRestartSchemaV1,
		RequestID:  "0123456789abcdef0123456789abcdef",
		Generation: lock.Generation,
	}
	if err := preflightWakeRestartCandidate(evidence, []string{
		evidence.ExecutionPath,
		"wake",
		"--root", lock.Root,
		"--me", lock.Agent,
	}, bootstrap); err == nil {
		t.Fatal("persisted pathname evidence alone authorized executing a mismatched candidate")
	}
}

func validWakeResumeOwnerForTest() wakeOwner {
	return wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
}

func validWakeImageEvidenceForTest() wakeImageEvidenceV1 {
	method := wakeImageMethodFDExec
	path := "/opt/amq/0.50.2/bin/amq"
	if runtime.GOOS == "darwin" {
		method = wakeImageMethodPathnameExecVerified
		path = "/opt/homebrew/Cellar/amq/0.50.2/bin/amq"
	}
	return wakeImageEvidenceV1{
		Schema:          wakeImageEvidenceSchemaV1,
		Platform:        runtime.GOOS,
		Method:          method,
		ExecutionPath:   path,
		Device:          1,
		Inode:           2,
		Size:            3,
		CTimeNS:         4,
		SHA256:          "sha256:" + strings.Repeat("a", 64),
		EmbeddedVersion: "0.50.2",
	}
}

func validWakeResumeLockForTest() wakeLock {
	owner := validWakeResumeOwnerForTest()
	evidence := validWakeImageEvidenceForTest()
	root := canonicalWakeRoot("/queue")
	agent := "codex"
	generation := "resume-generation"
	return wakeLock{
		PID:                  5151,
		TTY:                  "/dev/ttys001",
		Root:                 root,
		Agent:                agent,
		ProcessStart:         "67890",
		BootID:               owner.BootID,
		WakeMode:             wakeInjectModeRaw,
		Generation:           generation,
		ResumeSignal:         wakeResumeSignalUSR1,
		ImagePath:            evidence.ExecutionPath,
		ImageVersion:         evidence.EmbeddedVersion,
		ResumeSchema:         wakeResumeSchemaV2,
		ResumeOwner:          &owner,
		RunningImageEvidence: &evidence,
	}
}

func TestWakeResumeMetadataRoundTripsWithoutChangingGenericClaim(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	lock := validWakeResumeLockForTest()
	lock.Root = canonicalWakeRoot(root)
	lock.ControlSocket = wakeControlSocketPath(lock.Root, lock.Agent, lock.Generation)
	writeWakeLockForTest(t, root, "codex", lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: lock.ProcessStart,
			BootID:     lock.BootID,
			Executable: lock.RunningImageEvidence.ExecutionPath,
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
		t.Fatalf("inspection = status %q reason %q", inspection.Status, inspection.Reason)
	}
	if got := classifyWakeClaimForGenericTransition(inspection); got != wakeClaimGeneric {
		t.Fatalf("claim = %v, want generic", got)
	}
	if err := validateWakeResumeAdvertisement(inspection.Lock, root, "codex"); err != nil {
		t.Fatalf("round-tripped advertisement invalid: %v", err)
	}

	data, err := json.Marshal(inspection.Lock)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"resume_schema", "resume_owner", "resume_signal", "running_image_evidence"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Fatalf("lock JSON missing %q: %s", field, data)
		}
	}
}
