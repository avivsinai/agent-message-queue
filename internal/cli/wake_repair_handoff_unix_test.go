//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestWakeRepairHandoffMessagesBindExactSourcePreparedAndAdmit(t *testing.T) {
	source := wakeRepairHandoffSource{
		schema:               wakeRepairHandoffSchema,
		root:                 "/private/tmp/amq",
		rootIdentity:         "v1:darwin:1:2",
		agent:                "codex",
		sourceGeneration:     "source-generation",
		sourceTargetDigest:   "sha256:" + strings.Repeat("1", 64),
		sourceFloorDigest:    "sha256:" + strings.Repeat("2", 64),
		bootID:               "boot-id",
		agentDirDevice:       1,
		agentDirInode:        2,
		inboxParentDirDevice: 1,
		inboxParentDirInode:  4,
		inboxDirDevice:       1,
		inboxDirInode:        3,
	}
	if err := source.validate(); err != nil {
		t.Fatalf("validate source: %v", err)
	}
	sourceDigest, err := source.digest()
	if err != nil {
		t.Fatalf("digest source: %v", err)
	}
	replacedParent := source
	replacedParent.inboxParentDirInode++
	replacedParentDigest, err := replacedParent.digest()
	if err != nil {
		t.Fatalf("digest source with replaced inbox parent identity: %v", err)
	}
	if replacedParentDigest == sourceDigest {
		t.Fatal("source digest does not bind the original inbox parent identity")
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
	dir := secureTempDirForTest(t)
	output := filepath.Join(dir, "open-fds")
	t.Setenv(injectViaHelperEnv, "1")
	args := []string{
		"-test.run=^TestWakeRepairFDInspectorHelperProcess$",
		"--",
		output,
		strconv.Itoa(len(fds)),
	}
	for _, fd := range fds {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			t.Fatalf("stat repair descriptor %d before injector exec: %v", fd, err)
		}
		args = append(
			args,
			strconv.Itoa(fd)+":"+
				strconv.FormatUint(uint64(stat.Dev), 10)+":"+
				strconv.FormatUint(uint64(stat.Ino), 10),
		)
	}
	if err := injectVia(&wakeConfig{
		injectVia:     copyTestBinaryForInjectVia(t),
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
