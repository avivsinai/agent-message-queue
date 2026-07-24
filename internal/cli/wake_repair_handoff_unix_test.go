//go:build darwin || linux

package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

func wakeRepairFloorAuthorityForTest(
	source wakeRepairHandoffSource,
	generation string,
) wakeRepairFloorAuthority {
	return wakeRepairFloorAuthority{
		ChildGeneration:   generation,
		SourceFloorDigest: source.sourceFloorDigest,
		RawDigest:         "sha256:" + strings.Repeat("5", 64),
		FileIdentity: wakeFileIdentity{
			Device:    1,
			Inode:     2,
			CTimeSec:  3,
			CTimeNsec: 4,
		},
	}
}

func wakeRepairProtocolPreparedForTest(
	t *testing.T,
	generation string,
) (wakeRepairHandoffSource, wakeRepairHandoffPrepared) {
	t.Helper()
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               "/tmp/amq",
		rootIdentity:       "v1:linux:1:2",
		agent:              "codex",
		sourceGeneration:   "source-generation",
		sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
		bootID:             "boot-id",
		agentDirDevice:     1,
		agentDirInode:      2,
		inboxDirDevice:     1,
		inboxDirInode:      3,
	}
	prepared, err := newWakeRepairHandoffPrepared(
		source,
		os.Getpid(),
		generation,
		source.sourceTargetDigest,
		"sha256:"+strings.Repeat("4", 64),
		wakeRepairFloorAuthorityForTest(source, generation),
	)
	if err != nil {
		t.Fatalf("new protocol prepared: %v", err)
	}
	return source, prepared
}

func TestWakeRepairHandoffMessagesBindExactSourcePreparedAndAdmit(t *testing.T) {
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               "/private/tmp/amq",
		rootIdentity:       "v1:darwin:1:2",
		agent:              "codex",
		sourceGeneration:   "source-generation",
		sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
		bootID:             "boot-id",
		agentDirDevice:     1,
		agentDirInode:      2,
		inboxDirDevice:     1,
		inboxDirInode:      3,
	}
	if err := source.validate(); err != nil {
		t.Fatalf("validate source: %v", err)
	}
	sourceDigest, err := source.digest()
	if err != nil {
		t.Fatalf("digest source: %v", err)
	}

	prepared, err := newWakeRepairHandoffPrepared(
		source,
		4242,
		"child-generation",
		source.sourceTargetDigest,
		"sha256:"+strings.Repeat("4", 64),
		wakeRepairFloorAuthorityForTest(source, "child-generation"),
	)
	if err != nil {
		t.Fatalf("new prepared: %v", err)
	}
	if prepared.sourceDigest != sourceDigest {
		t.Fatalf("prepared source digest = %q, want %q", prepared.sourceDigest, sourceDigest)
	}
	admit, err := newWakeRepairHandoffAdmit(prepared)
	if err != nil {
		t.Fatalf("new admit: %v", err)
	}
	if admit.childGeneration != prepared.childGeneration {
		t.Fatalf("admit generation = %q, want %q", admit.childGeneration, prepared.childGeneration)
	}
	preparedDigest, err := prepared.digest()
	if err != nil {
		t.Fatalf("digest prepared: %v", err)
	}
	if admit.preparedDigest != preparedDigest {
		t.Fatalf("admit prepared digest = %q, want %q", admit.preparedDigest, preparedDigest)
	}

	replaced := prepared
	replaced.childGeneration = "replacement"
	if err := admit.validatePrepared(replaced); err == nil {
		t.Fatal("admit accepted a different child generation")
	}
	release, err := newWakeRepairHandoffRelease(admit)
	if err != nil {
		t.Fatalf("new release: %v", err)
	}
	if err := release.validateAdmit(admit); err != nil {
		t.Fatalf("validate exact release: %v", err)
	}
	replacedAdmit, err := newWakeRepairHandoffAdmit(replaced)
	if err == nil {
		if err := release.validateAdmit(replacedAdmit); err == nil {
			t.Fatal("release accepted a different admitted child")
		}
	}
}

func TestWakeRepairHandoffFrameRoundTripIsStrictAndBounded(t *testing.T) {
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               "/tmp/amq",
		rootIdentity:       "v1:linux:1:2",
		agent:              "codex",
		sourceGeneration:   "source-generation",
		sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
		bootID:             "boot-id",
		agentDirDevice:     1,
		agentDirInode:      2,
		inboxDirDevice:     1,
		inboxDirInode:      3,
	}
	var encoded bytes.Buffer
	if err := writeWakeRepairHandoffSource(&encoded, source); err != nil {
		t.Fatalf("write source: %v", err)
	}
	decoded, err := readWakeRepairHandoffSource(&encoded)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if decoded != source {
		t.Fatalf("decoded source = %#v, want %#v", decoded, source)
	}

	unknown := bytes.NewBufferString(`{"schema":1,"kind":"source","unknown":true}` + "\n")
	if _, err := readWakeRepairHandoffSource(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error = %v", err)
	}

	oversized := bytes.NewBuffer(append(bytes.Repeat([]byte{'x'}, wakeRepairHandoffMaxFrameBytes+1), '\n'))
	if _, err := readWakeRepairHandoffSource(oversized); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized-frame error = %v", err)
	}
}

func TestWakeRepairPrivateHandoffRequiresExactAdmittedEcho(t *testing.T) {
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               "/tmp/amq",
		rootIdentity:       "v1:linux:1:2",
		agent:              "codex",
		sourceGeneration:   "source-generation",
		sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
		bootID:             "boot-id",
		agentDirDevice:     1,
		agentDirInode:      2,
		inboxDirDevice:     1,
		inboxDirInode:      3,
	}
	prepared, err := newWakeRepairHandoffPrepared(
		source,
		os.Getpid(),
		"child-generation",
		source.sourceTargetDigest,
		"sha256:"+strings.Repeat("4", 64),
		wakeRepairFloorAuthorityForTest(source, "child-generation"),
	)
	if err != nil {
		t.Fatal(err)
	}

	parentToChildReader, parentToChildWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	childToParentReader, childToParentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = parentToChildReader.Close()
		_ = parentToChildWriter.Close()
		_ = childToParentReader.Close()
		_ = childToParentWriter.Close()
	}()
	parent := newWakeRepairParentHandoffForFiles(parentToChildWriter, childToParentReader)
	child := newWakeRepairChildHandoffForFiles(parentToChildReader, childToParentWriter)

	var validationCalls atomic.Int32
	errs := make(chan error, 1)
	go func() {
		gotSource, receiveErr := child.ReceiveSource()
		if receiveErr != nil {
			errs <- receiveErr
			return
		}
		if gotSource != source {
			errs <- io.ErrUnexpectedEOF
			return
		}
		if sendErr := child.SendPrepared(prepared); sendErr != nil {
			errs <- sendErr
			return
		}
		errs <- child.AwaitAdmitAcknowledgeAndRelease(prepared, func() error {
			validationCalls.Add(1)
			return nil
		})
	}()

	if err := parent.SendSource(source); err != nil {
		t.Fatalf("send source: %v", err)
	}
	gotPrepared, err := parent.ReceivePrepared(source)
	if err != nil {
		t.Fatalf("receive prepared: %v", err)
	}
	if gotPrepared != prepared {
		t.Fatalf("prepared = %#v, want %#v", gotPrepared, prepared)
	}
	if err := parent.Admit(prepared); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if calls := validationCalls.Load(); calls != 1 {
		t.Fatalf("validator calls after ACK = %d, want exactly 1", calls)
	}
	select {
	case err := <-errs:
		t.Fatalf("child passed admission before release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if calls := validationCalls.Load(); calls != 1 {
		t.Fatalf("validator calls before RELEASE = %d, want exactly 1", calls)
	}
	if err := parent.Release(prepared); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("child handoff: %v", err)
	}
	if calls := validationCalls.Load(); calls != 2 {
		t.Fatalf("validator calls after RELEASE = %d, want exactly 2", calls)
	}
	if err := parent.Release(prepared); err == nil ||
		!strings.Contains(err.Error(), "already released") {
		t.Fatalf("duplicate release error = %v", err)
	}
}

func TestWakeRepairPostReleaseValidationFailureDoesNotPassGate(t *testing.T) {
	_, prepared := wakeRepairProtocolPreparedForTest(t, "child-generation")
	parentToChildReader, parentToChildWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	childToParentReader, childToParentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = parentToChildReader.Close()
		_ = parentToChildWriter.Close()
		_ = childToParentReader.Close()
		_ = childToParentWriter.Close()
	}()
	parent := newWakeRepairParentHandoffForFiles(parentToChildWriter, childToParentReader)
	child := newWakeRepairChildHandoffForFiles(parentToChildReader, childToParentWriter)

	outputPath := filepath.Join(t.TempDir(), "gate-effect")
	var validationCalls atomic.Int32
	result := make(chan error, 1)
	go func() {
		gateErr := child.AwaitAdmitAcknowledgeAndRelease(
			prepared,
			func() error {
				if validationCalls.Add(1) == 2 {
					return errors.New("injected post-release validation failure")
				}
				return nil
			},
		)
		if gateErr == nil {
			gateErr = os.WriteFile(outputPath, []byte("effect\n"), 0o600)
		}
		result <- gateErr
	}()

	if err := parent.Admit(prepared); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if calls := validationCalls.Load(); calls != 1 {
		t.Fatalf("validator calls after ACK = %d, want exactly 1", calls)
	}
	if err := parent.Release(prepared); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case err := <-result:
		if err == nil ||
			!strings.Contains(err.Error(), "post-release admission validation failed") ||
			!strings.Contains(err.Error(), "injected post-release validation failure") {
			t.Fatalf("post-release validation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("post-release validation failure did not close the admission gate")
	}
	if calls := validationCalls.Load(); calls != 2 {
		t.Fatalf("validator calls after RELEASE = %d, want exactly 2", calls)
	}
	if output, err := os.ReadFile(outputPath); err == nil {
		t.Fatalf("post-release validation failure allowed gate effect: %q", output)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect post-release gate effect: %v", err)
	}
}

func TestWakeRepairChildCannotPassAdmissionGateOnParentEOF(t *testing.T) {
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               "/tmp/amq",
		rootIdentity:       "v1:linux:1:2",
		agent:              "codex",
		sourceGeneration:   "source-generation",
		sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
		bootID:             "boot-id",
		agentDirDevice:     1,
		agentDirInode:      2,
		inboxDirDevice:     1,
		inboxDirInode:      3,
	}
	prepared, err := newWakeRepairHandoffPrepared(
		source,
		os.Getpid(),
		"child-generation",
		source.sourceTargetDigest,
		"sha256:"+strings.Repeat("4", 64),
		wakeRepairFloorAuthorityForTest(source, "child-generation"),
	)
	if err != nil {
		t.Fatal(err)
	}
	parentToChildReader, parentToChildWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	childToParentReader, childToParentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = childToParentReader.Close() }()
	child := newWakeRepairChildHandoffForFiles(parentToChildReader, childToParentWriter)
	defer func() { _ = child.Close() }()

	if err := parentToChildWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child.AwaitAdmitAcknowledgeAndRelease(prepared, func() error { return nil }); err == nil ||
		!strings.Contains(err.Error(), "admission") {
		t.Fatalf("admission EOF error = %v", err)
	}
}

func TestWakeRepairParentAdmissionAcknowledgementIsBounded(t *testing.T) {
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               "/tmp/amq",
		rootIdentity:       "v1:linux:1:2",
		agent:              "codex",
		sourceGeneration:   "source-generation",
		sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
		bootID:             "boot-id",
		agentDirDevice:     1,
		agentDirInode:      2,
		inboxDirDevice:     1,
		inboxDirInode:      3,
	}
	prepared, err := newWakeRepairHandoffPrepared(
		source,
		os.Getpid(),
		"child-generation",
		source.sourceTargetDigest,
		"sha256:"+strings.Repeat("4", 64),
		wakeRepairFloorAuthorityForTest(source, "child-generation"),
	)
	if err != nil {
		t.Fatal(err)
	}
	parentToChildReader, parentToChildWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	childToParentReader, childToParentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = parentToChildReader.Close()
		_ = parentToChildWriter.Close()
		_ = childToParentReader.Close()
		_ = childToParentWriter.Close()
	}()
	parent := newWakeRepairParentHandoffForFiles(parentToChildWriter, childToParentReader)
	defer func() { _ = parent.Close() }()

	oldTimeout := wakeRepairAdmitTimeout
	wakeRepairAdmitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { wakeRepairAdmitTimeout = oldTimeout })
	started := time.Now()
	err = parent.Admit(prepared)
	if err == nil {
		t.Fatal("admission unexpectedly accepted a missing child acknowledgement")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("admission acknowledgement timeout took %s", elapsed)
	}
	if !strings.Contains(err.Error(), "acknowledgement") {
		t.Fatalf("admission timeout error = %v", err)
	}
}

func TestWakeRepairParentReleaseRequiresExactAcknowledgement(t *testing.T) {
	_, prepared := wakeRepairProtocolPreparedForTest(t, "child-generation")
	parentToChildReader, parentToChildWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	childToParentReader, childToParentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = parentToChildReader.Close()
		_ = parentToChildWriter.Close()
		_ = childToParentReader.Close()
		_ = childToParentWriter.Close()
	}()
	parent := newWakeRepairParentHandoffForFiles(parentToChildWriter, childToParentReader)

	if err := parent.Release(prepared); err == nil ||
		!strings.Contains(err.Error(), "before exact acknowledgement") {
		t.Fatalf("release-before-ack error = %v", err)
	}
	if err := parentToChildReader.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var unexpected [1]byte
	if _, err := parentToChildReader.Read(unexpected[:]); err == nil {
		t.Fatalf("release-before-ack wrote protocol bytes: %q", unexpected[:])
	}
}

func TestWakeRepairChildReleaseFailuresFailClosed(t *testing.T) {
	source, prepared := wakeRepairProtocolPreparedForTest(t, "child-generation")
	admit, err := newWakeRepairHandoffAdmit(prepared)
	if err != nil {
		t.Fatal(err)
	}
	exactRelease, err := newWakeRepairHandoffRelease(admit)
	if err != nil {
		t.Fatal(err)
	}
	_, mismatchedPrepared := wakeRepairProtocolPreparedForTest(t, "other-generation")
	mismatchedAdmit, err := newWakeRepairHandoffAdmit(mismatchedPrepared)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedRelease, err := newWakeRepairHandoffRelease(mismatchedAdmit)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		action func(*os.File) bool
		wants  []string
	}{
		{
			name:  "missing release times out",
			wants: []string{"wait for wake repair release", "timeout"},
		},
		{
			name: "release EOF",
			action: func(writer *os.File) bool {
				if err := writer.Close(); err != nil {
					t.Fatalf("close release writer: %v", err)
				}
				return true
			},
			wants: []string{"wait for wake repair release", "EOF"},
		},
		{
			name: "partial release",
			action: func(writer *os.File) bool {
				if _, err := io.WriteString(writer, `{"schema":1,"kind":"release"`); err != nil {
					t.Fatalf("write partial release: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("close partial release writer: %v", err)
				}
				return true
			},
			wants: []string{"wait for wake repair release", "EOF"},
		},
		{
			name: "admit replay where release required",
			action: func(writer *os.File) bool {
				wrongKind := exactRelease.wire()
				wrongKind.Kind = wakeRepairHandoffKindAdmit
				if err := writeWakeRepairHandoffFrame(writer, wrongKind); err != nil {
					t.Fatalf("write replayed admit: %v", err)
				}
				return false
			},
			wants: []string{`want release`},
		},
		{
			name: "release for different admit",
			action: func(writer *os.File) bool {
				if err := writeWakeRepairHandoffRelease(writer, mismatchedRelease); err != nil {
					t.Fatalf("write mismatched release: %v", err)
				}
				return false
			},
			wants: []string{"release does not match exact admit"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parentToChildReader, parentToChildWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			childToParentReader, childToParentWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			writerClosed := false
			defer func() {
				_ = parentToChildReader.Close()
				if !writerClosed {
					_ = parentToChildWriter.Close()
				}
				_ = childToParentReader.Close()
				_ = childToParentWriter.Close()
			}()
			child := newWakeRepairChildHandoffForFiles(
				parentToChildReader,
				childToParentWriter,
			)

			oldTimeout := wakeRepairAdmitTimeout
			wakeRepairAdmitTimeout = 50 * time.Millisecond
			defer func() { wakeRepairAdmitTimeout = oldTimeout }()

			beforeAcknowledgeCalls := 0
			result := make(chan error, 1)
			go func() {
				result <- child.AwaitAdmitAcknowledgeAndRelease(
					prepared,
					func() error {
						beforeAcknowledgeCalls++
						return nil
					},
				)
			}()
			if err := writeWakeRepairHandoffAdmit(parentToChildWriter, admit); err != nil {
				t.Fatalf("write admit: %v", err)
			}
			ack, err := readWakeRepairHandoffAdmit(childToParentReader)
			if err != nil {
				t.Fatalf("read admitted acknowledgement: %v", err)
			}
			if ack != admit {
				t.Fatalf("acknowledgement = %#v, want %#v", ack, admit)
			}
			if test.action != nil {
				writerClosed = test.action(parentToChildWriter)
			}

			select {
			case err := <-result:
				if err == nil {
					t.Fatal("child passed admission without an exact release")
				}
				for _, want := range test.wants {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("release failure error = %v, want %q", err, want)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("child did not fail closed on invalid release")
			}
			if beforeAcknowledgeCalls != 1 {
				t.Fatalf("before-ack validation calls = %d, want 1", beforeAcknowledgeCalls)
			}
			if err := source.validate(); err != nil {
				t.Fatalf("source changed during protocol test: %v", err)
			}
		})
	}
}

func TestPrepareWakeRepairHandoffUsesPrivateInheritedDescriptors(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inboxDir.Close() }()

	cmd := exec.Command("true")
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               canonicalWakeRoot(root),
		rootIdentity:       "v1:linux:1:2",
		agent:              "codex",
		sourceGeneration:   "source-generation",
		sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
		bootID:             "boot-id",
	}
	if err := source.bindRetainedDirectories(agentDir, inboxDir); err != nil {
		t.Fatal(err)
	}
	handoff, err := prepareWakeRepairHandoff(cmd, source, agentDir, inboxDir)
	if err != nil {
		t.Fatalf("prepare handoff: %v", err)
	}
	defer func() { _ = handoff.Close() }()
	if len(cmd.ExtraFiles) != 4 {
		t.Fatalf("extra files = %d, want 4", len(cmd.ExtraFiles))
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, envWakeRepairHandoffReadFD+"=3") ||
		!strings.Contains(env, envWakeRepairHandoffWriteFD+"=4") ||
		!strings.Contains(env, envWakeRepairAgentDirFD+"=5") ||
		!strings.Contains(env, envWakeRepairInboxDirFD+"=6") {
		t.Fatalf("handoff env = %q", env)
	}
	for index, label := range []string{"agent directory", "inbox directory"} {
		fd := cmd.ExtraFiles[index+2].Fd()
		flags, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("inspect duplicated %s fd: %v", label, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("duplicated %s fd %d is not close-on-exec in parent", label, fd)
		}
	}
}

func TestWakeRepairChildHandoffDescriptorsDoNotLeakIntoInjector(t *testing.T) {
	fds := make([]int, 4)
	for index := range fds {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Dup(int(reader.Fd()))
		_ = reader.Close()
		if err != nil {
			_ = writer.Close()
			t.Fatalf("duplicate inherited handoff fd: %v", err)
		}
		t.Cleanup(func() { _ = writer.Close() })
		fds[index] = fd
	}
	t.Setenv(envWakeRepairHandoffReadFD, strconv.Itoa(fds[0]))
	t.Setenv(envWakeRepairHandoffWriteFD, strconv.Itoa(fds[1]))
	t.Setenv(envWakeRepairAgentDirFD, strconv.Itoa(fds[2]))
	t.Setenv(envWakeRepairInboxDirFD, strconv.Itoa(fds[3]))

	handoff, present, err := wakeRepairChildHandoffFromEnv()
	if err != nil {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		t.Fatalf("initialize inherited child handoff: %v", err)
	}
	if !present {
		t.Fatal("inherited child handoff was not detected")
	}
	defer func() { _ = handoff.Close() }()

	for _, fd := range fds {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("inspect initialized handoff fd %d: %v", fd, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("initialized handoff fd %d is not close-on-exec", fd)
		}
	}
	assertWakeRepairDescriptorsClosedInInjector(t, fds)
}

func assertWakeRepairDescriptorsClosedInInjector(t *testing.T, fds []int) {
	t.Helper()
	output := filepath.Join(t.TempDir(), "open-fds")
	inspector := filepath.Join(t.TempDir(), "fd-inspector")
	script := "#!/bin/sh\n" + `output=$1; shift; count=$1; shift; : > "$output"; while [ "$count" -gt 0 ]; do fd=$1; shift; if [ -e "/dev/fd/$fd" ]; then printf '%s\n' "$fd" >> "$output"; fi; count=$((count - 1)); done` + "\n"
	if err := os.WriteFile(inspector, []byte(script), 0o700); err != nil {
		t.Fatalf("write user-owned injector inspector: %v", err)
	}
	inspector, err := filepath.EvalSymlinks(inspector)
	if err != nil {
		t.Fatalf("resolve user-owned injector inspector: %v", err)
	}
	args := []string{output, strconv.Itoa(len(fds))}
	for _, fd := range fds {
		args = append(args, strconv.Itoa(fd))
	}
	if err := injectVia(&wakeConfig{
		injectVia:     inspector,
		injectArgs:    args,
		injectTimeout: 2 * time.Second,
	}, "ignored wake payload"); err != nil {
		t.Fatalf("run injector fd inspector: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read injector fd inspection: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("repair descriptors leaked into injector child: %s", data)
	}
}

func TestInheritedWakeRepairDirectoriesStayBoundAcrossAgentPathReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	sourceAgentPath := fsq.AgentBase(root, "codex")
	sourceInboxPath := fsq.AgentInboxNew(root, "codex")
	writeWakeRepairHandoffMessage(t, filepath.Join(sourceInboxPath, "source.md"), "source")

	parentAgentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parentAgentDir.Close() }()
	parentInboxDir, err := openWakeRepairInboxDir(parentAgentDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parentInboxDir.Close() }()
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               canonicalWakeRoot(root),
		rootIdentity:       "v1:darwin:1:2",
		agent:              "codex",
		sourceGeneration:   "source-generation",
		sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
		bootID:             "boot-id",
	}
	if err := source.bindRetainedDirectories(parentAgentDir, parentInboxDir); err != nil {
		t.Fatal(err)
	}
	childAgentFile, err := duplicateWakeRepairDirectoryFile(parentAgentDir.file, "test inherited wake agent directory")
	if err != nil {
		t.Fatal(err)
	}
	childInboxFile, err := duplicateWakeRepairDirectoryFile(parentInboxDir.file, "test inherited wake inbox directory")
	if err != nil {
		_ = childAgentFile.Close()
		t.Fatal(err)
	}
	childAgentDir, childInboxDir, err := openInheritedWakeRepairDirectories(
		childAgentFile,
		childInboxFile,
		source,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = childInboxDir.Close()
		_ = childAgentDir.Close()
	}()

	detachedAgentPath := filepath.Join(filepath.Dir(sourceAgentPath), "codex-detached")
	if err := os.Rename(sourceAgentPath, detachedAgentPath); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	replacementInboxPath := fsq.AgentInboxNew(root, "codex")
	writeWakeRepairHandoffMessage(t, filepath.Join(replacementInboxPath, "replacement.md"), "replacement")

	entries, err := childInboxDir.ReadDir()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "source.md" {
		t.Fatalf("retained inbox entries = %v, want only source.md", names)
	}
	header, err := childInboxDir.ReadHeader("source.md")
	if err != nil {
		t.Fatal(err)
	}
	if header.Subject != "source" {
		t.Fatalf("retained header subject = %q, want source", header.Subject)
	}
	if err := touchWakePresenceInDir(childAgentDir, "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sourceAgentPath, "presence.json")); !os.IsNotExist(err) {
		t.Fatalf("replacement agent received retained presence touch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(detachedAgentPath, "presence.json")); err != nil {
		t.Fatalf("retained agent did not receive presence touch: %v", err)
	}
	output, err := openWakeRepairOutputInDir(childAgentDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.WriteString("retained\n"); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sourceAgentPath, ".wake.repair.log")); !os.IsNotExist(err) {
		t.Fatalf("replacement agent received retained repair log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(detachedAgentPath, ".wake.repair.log")); err != nil {
		t.Fatalf("retained agent did not receive repair log: %v", err)
	}

	watcher, err := childInboxDir.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watcher.Close() }()
	writeWakeRepairHandoffMessage(t, filepath.Join(replacementInboxPath, "replacement-event.md"), "replacement event")
	select {
	case event := <-watcher.Events():
		t.Fatalf("replacement inbox reached retained watcher: %#v", event)
	case err := <-watcher.Errors():
		t.Fatalf("retained watcher error after replacement write: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	detachedInboxPath := filepath.Join(detachedAgentPath, "inbox", "new")
	writeWakeRepairHandoffMessage(t, filepath.Join(detachedInboxPath, "source-event.md"), "source event")
	select {
	case <-watcher.Events():
	case err := <-watcher.Errors():
		t.Fatalf("retained watcher error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("retained watcher did not observe source inode write")
	}
}

func writeWakeRepairHandoffMessage(t *testing.T, path, subject string) {
	t.Helper()
	data, err := (format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       subject,
			From:     "claude",
			To:       []string{"codex"},
			Thread:   "p2p/claude__codex",
			Subject:  subject,
			Created:  "2026-07-24T00:00:00Z",
			Priority: "normal",
		},
		Body: "body",
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
