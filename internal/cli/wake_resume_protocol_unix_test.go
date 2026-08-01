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
	if err := validateWakeImageEvidence(evidence); err != nil {
		t.Fatalf("captured evidence is invalid: %v", err)
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
		ControlSocket:        wakeControlSocketPath(root, agent, generation),
		ImagePath:            evidence.ExecutionPath,
		ImageVersion:         evidence.EmbeddedVersion,
		ResumeSchema:         wakeResumeSchemaV2,
		ResumeOwner:          &owner,
		RunningImageEvidence: &evidence,
	}
}

func TestValidateWakeResumeAdvertisementAcceptsOnlyCompleteExactV2(t *testing.T) {
	if runtime.GOOS != "darwin" {
		err := validateWakeResumeAdvertisement(validWakeResumeLockForTest())
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("non-Darwin resume advertisement error = %v, want unsupported", err)
		}
		return
	}
	if err := validateWakeResumeAdvertisement(validWakeResumeLockForTest()); err != nil {
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
		{name: "missing control endpoint", mutate: func(l *wakeLock) { l.ControlSocket = "" }, want: "control"},
		{name: "relative control endpoint", mutate: func(l *wakeLock) { l.ControlSocket = ".w.relative" }, want: "control"},
		{name: "outside-agent control endpoint", mutate: func(l *wakeLock) {
			l.ControlSocket = filepath.Join(l.Root, ".w.outside")
		}, want: "control"},
		{name: "wrong-prefix control endpoint", mutate: func(l *wakeLock) {
			l.ControlSocket = filepath.Join(fsq.AgentBase(l.Root, l.Agent), ".wake.sock")
		}, want: "control"},
		{name: "wrong-generation control endpoint", mutate: func(l *wakeLock) {
			l.ControlSocket = wakeControlSocketPath(l.Root, l.Agent, "other-generation")
		}, want: "control"},
		{name: "wrong-agent control endpoint", mutate: func(l *wakeLock) {
			l.ControlSocket = wakeControlSocketPath(l.Root, "other", l.Generation)
		}, want: "control"},
		{name: "wrong-root control endpoint", mutate: func(l *wakeLock) {
			l.ControlSocket = wakeControlSocketPath(filepath.Join(l.Root, "other"), l.Agent, l.Generation)
		}, want: "control"},
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
			err := validateWakeResumeAdvertisement(lock)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
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
	if err := validateWakeResumeAdvertisement(inspection.Lock); runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("round-tripped advertisement invalid: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("non-Darwin round-tripped advertisement error = %v, want unsupported", err)
	}

	data, err := json.Marshal(inspection.Lock)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"resume_schema", "resume_owner", "running_image_evidence"} {
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
			if err := validateWakeResumeAdvertisement(inspection.Lock); err == nil {
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
	if runtime.GOOS == "linux" {
		// Wave 1 deliberately does not advertise the protocol before Wave 2
		// supplies Linux's authenticated control endpoint.
		if lock.ResumeSchema != 0 || lock.ResumeOwner != nil || lock.RunningImageEvidence != nil {
			t.Fatalf("Linux advertised resume before its control transport exists: %#v", lock)
		}
		return
	}
	if lock.ResumeSchema != wakeResumeSchemaV2 || !sameWakeOwner(lock.ResumeOwner, &owner) ||
		lock.RunningImageEvidence == nil || *lock.RunningImageEvidence != evidence {
		t.Fatalf("resume advertisement = %#v", lock)
	}
	if lock.ControlSocket == "" {
		t.Fatal("Darwin resume advertisement has no control endpoint")
	}
	if err := validateWakeResumeAdvertisement(lock); err != nil {
		t.Fatalf("new lock advertisement invalid: %v", err)
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
	if notEligible.RunningImageEvidence == nil || *notEligible.RunningImageEvidence != evidence {
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
		{name: "ordinary inject via", owner: &owner, mode: wakeTargetInjectVia, want: true},
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
