//go:build darwin || linux

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

func TestCaptureWakeImageEvidenceRejectsUnsafeFinalObject(t *testing.T) {
	dir := secureTempDirForTest(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("image"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "amq-link")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	nonExecutable := filepath.Join(dir, "amq-non-executable")
	if err := os.WriteFile(nonExecutable, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(dir, "amq-writable")
	if err := os.WriteFile(writable, []byte("image"), 0o722); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o722); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "final symlink", path: symlink, want: "symlink"},
		{name: "not executable", path: nonExecutable, want: "not executable"},
		{name: "group or world writable", path: writable, want: "group/world-writable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := captureWakeImageEvidence(test.path, "0.50.2")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
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

func TestValidateWakeImageEvidenceRejectsNonCanonicalExecutionPath(t *testing.T) {
	evidence := validWakeImageEvidenceForTest()
	evidence.ExecutionPath = " " + evidence.ExecutionPath
	if err := validateWakeImageEvidence(evidence); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("non-canonical execution path error = %v, want canonical", err)
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

func TestValidateWakeResumeAdvertisementBindsTrustedRootAndAgent(t *testing.T) {
	tests := []struct {
		name          string
		expectedRoot  string
		expectedAgent string
		mutate        func(*wakeLock)
		want          string
	}{
		{name: "empty trusted root", expectedAgent: "codex", want: "trusted wake resume root is empty"},
		{name: "invalid trusted agent", expectedRoot: "/queue", expectedAgent: "-codex", want: "trusted wake resume agent is invalid"},
		{name: "mismatched root", expectedRoot: "/queue", expectedAgent: "codex", mutate: func(lock *wakeLock) {
			lock.Root = canonicalWakeRoot("/other-queue")
			lock.ControlSocket = wakeControlSocketPath(lock.Root, lock.Agent, lock.Generation)
		}, want: "does not match the trusted root"},
		{name: "omitted agent", expectedRoot: "/queue", expectedAgent: "codex", mutate: func(lock *wakeLock) {
			lock.Agent = ""
			lock.ControlSocket = wakeControlSocketPath(lock.Root, lock.Agent, lock.Generation)
		}, want: "wake resume agent is invalid"},
		{name: "mismatched agent", expectedRoot: "/queue", expectedAgent: "codex", mutate: func(lock *wakeLock) {
			lock.Agent = "claude"
			lock.ControlSocket = wakeControlSocketPath(lock.Root, lock.Agent, lock.Generation)
		}, want: "does not match the trusted agent"},
		{name: "invalid artifact agent", expectedRoot: "/queue", expectedAgent: "codex", mutate: func(lock *wakeLock) {
			lock.Agent = "-legacy"
			lock.ControlSocket = wakeControlSocketPath(lock.Root, lock.Agent, lock.Generation)
		}, want: "wake resume agent is invalid"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lock := validWakeResumeLockForTest()
			if test.mutate != nil {
				test.mutate(&lock)
			}
			err := validateWakeResumeAdvertisement(lock, test.expectedRoot, test.expectedAgent)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateWakeResumeAdvertisementAcceptsOnlyCompleteExactV2(t *testing.T) {
	lock := validWakeResumeLockForTest()
	if err := validateWakeResumeAdvertisement(lock, lock.Root, lock.Agent); err != nil {
		t.Fatalf("valid resume advertisement rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*wakeLock)
		want   string
	}{
		{name: "missing schema", mutate: func(l *wakeLock) { l.ResumeSchema = 0 }, want: "schema"},
		{name: "future schema", mutate: func(l *wakeLock) { l.ResumeSchema = wakeResumeSchemaV2 + 1 }, want: "unsupported"},
		{name: "missing owner", mutate: func(l *wakeLock) { l.ResumeOwner = nil }, want: "owner"},
		{name: "invalid owner", mutate: func(l *wakeLock) { l.ResumeOwner.SessionID = 0 }, want: "session"},
		{name: "missing generation", mutate: func(l *wakeLock) { l.Generation = "" }, want: "generation"},
		{name: "missing wake process start", mutate: func(l *wakeLock) { l.ProcessStart = "" }, want: "process start"},
		{name: "missing wake boot id", mutate: func(l *wakeLock) { l.BootID = "" }, want: "boot id"},
		{name: "missing request endpoint", mutate: func(l *wakeLock) { l.ResumeSignal = "" }, want: "control"},
		{name: "unsupported signal", mutate: func(l *wakeLock) { l.ResumeSignal = "SIGUSR2" }, want: "signal"},
		{name: "repair lineage", mutate: func(l *wakeLock) { l.SourceGeneration = "dead-generation" }, want: "repair"},
		{name: "missing evidence", mutate: func(l *wakeLock) { l.RunningImageEvidence = nil }, want: "image evidence"},
		{name: "wrong evidence schema", mutate: func(l *wakeLock) { l.RunningImageEvidence.Schema++ }, want: "image evidence schema"},
		{name: "wrong platform", mutate: func(l *wakeLock) { l.RunningImageEvidence.Platform = "plan9" }, want: "platform"},
		{name: "wrong method", mutate: func(l *wakeLock) { l.RunningImageEvidence.Method = "pathname" }, want: "method"},
		{name: "relative execution path", mutate: func(l *wakeLock) { l.RunningImageEvidence.ExecutionPath = "bin/amq" }, want: "absolute"},
		{name: "non-canonical execution path", mutate: func(l *wakeLock) {
			l.RunningImageEvidence.ExecutionPath = " " + l.RunningImageEvidence.ExecutionPath
			l.ImagePath = l.RunningImageEvidence.ExecutionPath
		}, want: "canonical"},
		{name: "missing device", mutate: func(l *wakeLock) { l.RunningImageEvidence.Device = 0 }, want: "device"},
		{name: "missing inode", mutate: func(l *wakeLock) { l.RunningImageEvidence.Inode = 0 }, want: "inode"},
		{name: "empty image", mutate: func(l *wakeLock) { l.RunningImageEvidence.Size = 0 }, want: "size"},
		{name: "missing ctime", mutate: func(l *wakeLock) { l.RunningImageEvidence.CTimeNS = 0 }, want: "ctime"},
		{name: "malformed digest", mutate: func(l *wakeLock) { l.RunningImageEvidence.SHA256 = "sha256:abc" }, want: "sha256"},
		{name: "missing version", mutate: func(l *wakeLock) { l.RunningImageEvidence.EmbeddedVersion = "" }, want: "version"},
		{name: "image path disagreement", mutate: func(l *wakeLock) { l.ImagePath += ".other" }, want: "image path"},
		{name: "image version disagreement", mutate: func(l *wakeLock) { l.ImageVersion += ".other" }, want: "image version"},
		{
			name: "authoritative owner mismatch",
			mutate: func(l *wakeLock) {
				l.WakeMode = wakeOwnerWakeMode
				l.OwnerSchema = wakeOwnerLockSchema
				other := *l.ResumeOwner
				other.PID++
				l.Owner = &other
			},
			want: "does not match",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lock := validWakeResumeLockForTest()
			test.mutate(&lock)
			err := validateWakeResumeAdvertisement(lock, "/queue", "codex")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateWakeResumeAdvertisementRetainsLegacyControlEndpointValidation(t *testing.T) {
	lock := validWakeResumeLockForTest()
	lock.ResumeSignal = ""
	lock.ControlSocket = "/queue/agents/codex/.w.resume-generation"
	lock.RunningImageEvidence.Platform = "darwin"
	lock.RunningImageEvidence.Method = wakeImageMethodPathnameExecVerified
	lock.RunningImageEvidence.ExecutionPath = "/opt/homebrew/bin/amq"
	lock.ImagePath = lock.RunningImageEvidence.ExecutionPath
	expected := lock.ControlSocket
	if err := validateWakeResumeAdvertisementWithContext(lock, lock.Root, lock.Agent, "darwin", expected); err != nil {
		t.Fatalf("legacy control advertisement rejected: %v", err)
	}
	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{name: "missing", want: "control endpoint is missing"},
		{name: "mismatched", path: expected + ".other", want: "control endpoint does not match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := lock
			candidate.ControlSocket = test.path
			err := validateWakeResumeAdvertisementWithContext(candidate, lock.Root, lock.Agent, "darwin", expected)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("legacy control error = %v, want %q", err, test.want)
			}
		})
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

func TestMalformedOrFutureResumeMetadataDoesNotPoisonLiveNotifier(t *testing.T) {
	for _, schema := range []int{wakeResumeSchemaV2, wakeResumeSchemaV2 + 1} {
		t.Run(strconv.Itoa(schema), func(t *testing.T) {
			root := secureTempDirForTest(t)
			lock := validWakeResumeLockForTest()
			lock.Root = canonicalWakeRoot(root)
			lock.ResumeSchema = schema
			lock.RunningImageEvidence = nil
			writeWakeLockForTest(t, root, "codex", lock)
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return wakeProcessInfo{
					PID: pid, Running: true, StartToken: lock.ProcessStart, BootID: lock.BootID,
					Executable: "/opt/amq", Args: []string{"amq", "wake"},
				}
			})

			inspection := inspectWakeLock(root, "codex")
			if inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
				t.Fatalf("resume metadata poisoned notifier: status=%q reason=%q", inspection.Status, inspection.Reason)
			}
			if err := validateWakeResumeAdvertisement(inspection.Lock, root, "codex"); err == nil {
				t.Fatal("invalid resume metadata unexpectedly authorized reload")
			}
		})
	}
}

func TestNewWakeLockAdvertisesResumeOnlyWhenTheProtocolIsComplete(t *testing.T) {
	owner := validWakeResumeOwnerForTest()
	evidence := validWakeImageEvidenceForTest()
	oldCapture := captureCurrentWakeImageEvidence
	captureCurrentWakeImageEvidence = func() (wakeImageEvidenceV1, error) { return evidence, nil }
	t.Cleanup(func() { captureCurrentWakeImageEvidence = oldCapture })

	lock, err := newWakeLock("/queue", "codex", wakeLockAcquireOptions{
		wakeMode:       wakeInjectModeRaw,
		requestedOwner: &owner,
		resumeEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSignal := wakeResumeSignalUSR1
	wantControlSocket := ""
	if runtime.GOOS == "darwin" {
		wantSignal = ""
		wantControlSocket = wakeControlSocketPath("/queue", "codex", lock.Generation)
	}
	if lock.ResumeSchema != wakeResumeSchemaV2 || !sameWakeOwner(lock.ResumeOwner, &owner) ||
		lock.ResumeSignal != wantSignal || lock.ControlSocket != wantControlSocket ||
		lock.RunningImageEvidence == nil || *lock.RunningImageEvidence != evidence {
		t.Fatalf("resume advertisement = %#v", lock)
	}
	if err := validateWakeResumeAdvertisement(lock, "/queue", "codex"); err != nil {
		t.Fatalf("new lock advertisement invalid: %v", err)
	}

	targetRoot := secureTempDirForTest(t)
	injector := filepath.Join(targetRoot, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(t, targetRoot, "codex", injector, nil)
	targetLock, err := newWakeLock(targetRoot, "codex", wakeLockAcquireOptions{
		target:         &target,
		wakeMode:       wakeTargetInjectVia,
		requestedOwner: &owner,
		resumeEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if targetLock.ResumeSchema != 0 || targetLock.ResumeOwner != nil || targetLock.ResumeSignal != "" {
		t.Fatalf("inject-via target advertised resume despite target guard: %#v", targetLock)
	}

	notEligible, err := newWakeLock("/queue", "codex", wakeLockAcquireOptions{
		wakeMode:       wakeInjectModeRaw,
		requestedOwner: &owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if notEligible.ResumeSchema != 0 || notEligible.ResumeOwner != nil {
		t.Fatalf("ineligible wake advertised resume: %#v", notEligible)
	}
	if runtime.GOOS == "darwin" &&
		(notEligible.RunningImageEvidence == nil || *notEligible.RunningImageEvidence != evidence) {
		t.Fatalf("ordinary Darwin wake omitted additive image evidence: %#v", notEligible)
	}

	captureCurrentWakeImageEvidence = func() (wakeImageEvidenceV1, error) {
		return wakeImageEvidenceV1{}, errors.New("image unavailable")
	}
	missingEvidence, err := newWakeLock("/queue", "codex", wakeLockAcquireOptions{
		wakeMode:       wakeInjectModeRaw,
		requestedOwner: &owner,
		resumeEligible: true,
	})
	if err != nil {
		t.Fatalf("image evidence failure blocked notifier startup: %v", err)
	}
	if missingEvidence.ResumeSchema != 0 || missingEvidence.ResumeOwner != nil || missingEvidence.RunningImageEvidence != nil {
		t.Fatalf("incomplete evidence advertised resume: %#v", missingEvidence)
	}
}

func TestWakeResumeStartupEligibilityIsNarrowAndOwnerBound(t *testing.T) {
	owner := validWakeResumeOwnerForTest()
	tests := []struct {
		name         string
		owner        *wakeOwner
		repair       bool
		injectCmd    string
		interruptKey string
		mode         string
		want         bool
	}{
		{name: "raw coop", owner: &owner, mode: wakeInjectModeRaw, want: true},
		{name: "paste coop", owner: &owner, mode: wakeInjectModePaste, want: true},
		{name: "none coop", owner: &owner, mode: wakeInjectModeNone, want: true},
		{name: "ordinary inject via", owner: &owner, mode: wakeTargetInjectVia},
		{name: "ownerless", mode: wakeInjectModeRaw},
		{name: "repair lineage", owner: &owner, mode: wakeTargetInjectVia, repair: true},
		{name: "arbitrary inject command", owner: &owner, mode: wakeInjectModeRaw, injectCmd: "helper"},
		{name: "destructive interrupt", owner: &owner, mode: wakeInjectModeRaw, interruptKey: "\x03"},
		{name: "unknown mode", owner: &owner, mode: "future-mode"},
		{name: "authoritative persisted mode is not a startup mode", owner: &owner, mode: wakeOwnerWakeMode},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := wakeResumeStartupEligible(
				test.owner,
				test.repair,
				test.injectCmd,
				test.interruptKey,
				test.mode,
			); got != test.want {
				t.Fatalf("wakeResumeStartupEligible() = %v, want %v", got, test.want)
			}
		})
	}
}
