//go:build linux

package cli

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const linuxWakeReloadExternalHelperEnv = "AMQ_TEST_WAKE_RELOAD_EXTERNAL_HELPER"

const linuxWakeReloadExternalAllowEarlyRefusalEnv = "AMQ_TEST_WAKE_RELOAD_ALLOW_EARLY_REFUSAL"

const linuxWakeReloadExternalResponsePrefix = "AMQ_TEST_WAKE_RELOAD_RESPONSE_BASE64="

type linuxWakeReloadTransportFixture struct {
	root     string
	agent    string
	owner    wakeOwner
	expected wakeLockInspection
	agentDir *wakeAgentDir
}

func (transport *linuxWakeReloadTransport) Path() string {
	if transport == nil {
		return ""
	}
	return transport.path
}

func (transport *linuxWakeReloadTransport) ActiveHandlers() int {
	if transport == nil {
		return 0
	}
	return len(transport.handlerSlots)
}

func writeWakeReloadTransportRequest(
	conn *net.UnixConn,
	request wakeReloadTransportRequest,
	fds []int,
) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if len(payload) > wakeReloadTransportMaxRequestBytes {
		return fmt.Errorf("wake reload request exceeds size bound")
	}
	var rights []byte
	if len(fds) > 0 {
		rights = unix.UnixRights(fds...)
	}
	n, _, err := conn.WriteMsgUnix(payload, rights, nil)
	if err != nil {
		return err
	}
	if n < len(payload) {
		if _, err := conn.Write(payload[n:]); err != nil {
			return err
		}
	}
	return conn.CloseWrite()
}

func dialLinuxWakeReloadTransport(
	agentDir *wakeAgentDir,
	name string,
	timeout time.Duration,
) (*net.UnixConn, error) {
	if agentDir == nil || timeout <= 0 || !strings.HasPrefix(name, ".wr.") ||
		filepath.Base(name) != name {
		return nil, fmt.Errorf("wake reload dial target is invalid")
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ownedFD := true
	defer func() {
		if ownedFD {
			_ = unix.Close(fd)
		}
	}()
	err = agentDir.withFD(func(dirfd int) error {
		return unix.Connect(fd, &unix.SockaddrUnix{Name: linuxWakeReloadProcFDPath(dirfd, name)})
	})
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "wake-reload-client")
	connAny, err := net.FileConn(file)
	_ = file.Close()
	ownedFD = false
	if err != nil {
		return nil, err
	}
	conn, ok := connAny.(*net.UnixConn)
	if !ok {
		_ = connAny.Close()
		return nil, fmt.Errorf("wake reload connection is not unix")
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	return conn, nil
}

func snapshotLinuxWakeReloadTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += "\x00" + string(data)
		}
		snapshot[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func requireLinuxWakeReloadTreeUnchanged(
	t *testing.T,
	before map[string]string,
	after map[string]string,
) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("wake reload changed tree entry count: before=%d after=%d", len(before), len(after))
	}
	for path, want := range before {
		if got, ok := after[path]; !ok || got != want {
			t.Fatalf("wake reload changed %q: before=%q after=%q exists=%t", path, want, got, ok)
		}
	}
}

func newLinuxWakeReloadTransportFixture(t *testing.T) linuxWakeReloadTransportFixture {
	t.Helper()
	root := secureTempDirForTest(t)
	const agent = "codex"
	ownerProcess := exec.Command("sleep", "30")
	if err := ownerProcess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownerProcess.Process.Kill()
		_ = ownerProcess.Wait()
	})
	sessionID, err := getWakeProcessSID(ownerProcess.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	owner := wakeOwner{
		PID:          ownerProcess.Process.Pid,
		ProcessStart: "67890",
		BootID:       "22222222-2222-2222-2222-222222222222",
		SessionID:    sessionID,
	}
	wakeProcess := wakeProcessInfo{
		PID:        os.Getpid(),
		Running:    true,
		StartToken: "12345",
		BootID:     "11111111-1111-1111-1111-111111111111",
		Executable: "/usr/local/bin/amq",
		Args:       []string{"amq", "wake", "--root", root, "--me", agent},
	}
	ownerInfo := wakeProcessInfo{
		PID:        owner.PID,
		Running:    true,
		StartToken: owner.ProcessStart,
		BootID:     owner.BootID,
		Executable: "/usr/local/bin/amq",
		Args:       []string{"amq", "coop", "exec"},
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case os.Getpid():
			return wakeProcess
		case owner.PID:
			return ownerInfo
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	lock := wakeLock{
		PID:          os.Getpid(),
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-08-01T00:00:00Z",
		ProcessStart: wakeProcess.StartToken,
		BootID:       wakeProcess.BootID,
		Executable:   wakeProcess.Executable,
		Args:         append([]string(nil), wakeProcess.Args...),
		WakeMode:     wakeInjectModeNone,
		Generation:   "0123456789abcdef0123456789abcdef",
	}
	writeWakeLockExactForTest(t, root, agent, lock)
	expected := inspectWakeLock(root, agent)
	if expected.Status != wakeLockValid || !expected.IdentityConfirmed {
		t.Fatalf("fixture wake lock = %#v", expected)
	}
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	return linuxWakeReloadTransportFixture{
		root: root, agent: agent, owner: owner, expected: expected, agentDir: agentDir,
	}
}

func newRealLinuxWakeReloadTransportFixture(t *testing.T) linuxWakeReloadTransportFixture {
	t.Helper()
	root := secureTempDirForTest(t)
	const agent = "codex"
	ownerProcess := exec.Command("sleep", "30")
	if err := ownerProcess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ownerProcess.Process.Kill()
		_ = ownerProcess.Wait()
	})
	ownerInfo := inspectWakeProcessPlatform(ownerProcess.Process.Pid)
	ownerSID, err := getWakeProcessSID(ownerProcess.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	owner := wakeOwner{
		PID: ownerProcess.Process.Pid, ProcessStart: ownerInfo.StartToken,
		BootID: ownerInfo.BootID, SessionID: ownerSID,
	}
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		t.Fatalf("real owner identity: %v", err)
	}
	wakeProcess := inspectWakeProcessPlatform(os.Getpid())
	wakeProcess.Executable = "/usr/local/bin/amq"
	wakeProcess.Args = []string{"amq", "wake", "--root", root, "--me", agent}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == os.Getpid() {
			return wakeProcess
		}
		return inspectWakeProcessPlatform(pid)
	})
	originalPeerProcess := linuxWakeReloadPeerProcess
	linuxWakeReloadPeerProcess = inspectWakeProcessPlatform
	t.Cleanup(func() { linuxWakeReloadPeerProcess = originalPeerProcess })
	lock := wakeLock{
		PID: os.Getpid(), TTY: "unknown", Root: canonicalWakeRoot(root), Agent: agent,
		Started: time.Now().UTC().Format(time.RFC3339Nano), ProcessStart: wakeProcess.StartToken,
		BootID: wakeProcess.BootID, Executable: wakeProcess.Executable,
		Args: append([]string(nil), wakeProcess.Args...), WakeMode: wakeInjectModeNone,
		Generation: "0123456789abcdef0123456789abcdef",
	}
	writeWakeLockExactForTest(t, root, agent, lock)
	expected := inspectWakeLock(root, agent)
	if expected.Status != wakeLockValid || !expected.IdentityConfirmed {
		t.Fatalf("real fixture wake lock = %#v", expected)
	}
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	return linuxWakeReloadTransportFixture{
		root: root, agent: agent, owner: owner, expected: expected, agentDir: agentDir,
	}
}

func TestLinuxWakeReloadExternalHelperProcess(t *testing.T) {
	if os.Getenv(linuxWakeReloadExternalHelperEnv) != "1" {
		return
	}
	payload, err := base64.StdEncoding.DecodeString(os.Getenv("AMQ_TEST_WAKE_RELOAD_PAYLOAD"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("unix", os.Getenv("AMQ_TEST_WAKE_RELOAD_PATH"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatal("external helper connection is not unix")
	}
	defer func() { _ = unixConn.Close() }()
	allowEarlyRefusal := os.Getenv(linuxWakeReloadExternalAllowEarlyRefusalEnv) == "1"
	written, writeErr := unixConn.Write(payload)
	writeTeardown := linuxWakeReloadSilentTeardownError(writeErr)
	if writeErr != nil && !writeTeardown {
		t.Fatal(writeErr)
	}
	if written != len(payload) {
		if !allowEarlyRefusal || !writeTeardown {
			t.Fatalf("external helper wrote %d/%d request bytes", written, len(payload))
		}
	}
	closeWriteErr := unixConn.CloseWrite()
	if closeWriteErr != nil && !linuxWakeReloadSilentTeardownError(closeWriteErr) {
		t.Fatal(closeWriteErr)
	}
	response, readErr := io.ReadAll(unixConn)
	if readErr != nil {
		if len(response) != 0 || !linuxWakeReloadSilentTeardownError(readErr) {
			t.Fatal(readErr)
		}
	}
	if len(response) != 0 && (writeErr != nil || closeWriteErr != nil) {
		t.Fatalf("external helper received %d response bytes after connection teardown", len(response))
	}
	_, _ = fmt.Fprintln(
		os.Stdout,
		linuxWakeReloadExternalResponsePrefix+base64.StdEncoding.EncodeToString(response),
	)
}

func linuxWakeReloadSilentTeardownError(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

func runLinuxWakeReloadExternalHelper(
	t *testing.T,
	endpoint *linuxWakeReloadTransport,
	request wakeReloadTransportRequest,
	setsid bool,
) ([]byte, error) {
	t.Helper()
	payload := encodeWakeReloadTransportRequestForTest(t, request)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxWakeReloadExternalHelperProcess$")
	cmd.Env = append(os.Environ(),
		linuxWakeReloadExternalHelperEnv+"=1",
		"AMQ_TEST_WAKE_RELOAD_PATH="+endpoint.Path(),
		"AMQ_TEST_WAKE_RELOAD_PAYLOAD="+base64.StdEncoding.EncodeToString(payload),
	)
	allowEarlyRefusal := "0"
	if setsid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		allowEarlyRefusal = "1"
	}
	cmd.Env = append(cmd.Env, linuxWakeReloadExternalAllowEarlyRefusalEnv+"="+allowEarlyRefusal)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf(
				"external helper failed: %w (stdout=%q stderr=%q)",
				err,
				output,
				exitErr.Stderr,
			)
		}
		return nil, fmt.Errorf("start external helper: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, linuxWakeReloadExternalResponsePrefix) {
			continue
		}
		return base64.StdEncoding.DecodeString(strings.TrimPrefix(line, linuxWakeReloadExternalResponsePrefix))
	}
	return nil, fmt.Errorf("external helper response marker missing from %q", output)
}

func (fixture linuxWakeReloadTransportFixture) request() wakeReloadTransportRequest {
	request := validWakeReloadTransportRequestForTest()
	request.Root = canonicalWakeRoot(fixture.root)
	request.Agent = fixture.agent
	request.Generation = fixture.expected.Lock.Generation
	request.Owner = fixture.owner
	return request
}

func sendLinuxWakeReloadTransportRequest(
	t *testing.T,
	fixture linuxWakeReloadTransportFixture,
	endpoint *linuxWakeReloadTransport,
	request wakeReloadTransportRequest,
	fds ...int,
) (string, error) {
	t.Helper()
	conn, err := dialLinuxWakeReloadTransport(
		fixture.agentDir,
		endpoint.socketName,
		time.Second,
	)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if err := writeWakeReloadTransportRequest(conn, request, fds); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line), err
}

func sendRawLinuxWakeReloadTransportRequest(
	t *testing.T,
	fixture linuxWakeReloadTransportFixture,
	endpoint *linuxWakeReloadTransport,
	payload []byte,
) (string, error) {
	t.Helper()
	conn, err := dialLinuxWakeReloadTransport(
		fixture.agentDir,
		endpoint.socketName,
		time.Second,
	)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(payload); err != nil {
		return "", err
	}
	if err := conn.CloseWrite(); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line), err
}

func requireLinuxWakeReloadSilentRefusal(t *testing.T, response string, err error) {
	t.Helper()
	if err == nil || response != "" {
		t.Fatalf("wake reload refusal response = %q, err=%v", response, err)
	}
}

func linuxOpenFDSet(t *testing.T) map[int]struct{} {
	t.Helper()
	directory, err := os.Open("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	snapshotFD := int(directory.Fd())
	names, err := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	open := make(map[int]struct{}, len(names))
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			t.Fatalf("parse /proc/self/fd entry %q: %v", name, err)
		}
		if fd != snapshotFD {
			open[fd] = struct{}{}
		}
	}
	return open
}

func TestLinuxWakeReloadTransportAuthenticatesAndOnlyRefusesUnavailable(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	for path, data := range map[string]string{
		filepath.Join(fixture.agentDir.path, "inbox", "new", "message.md"): "queued-message",
		filepath.Join(fixture.agentDir.path, "inbox", "cur", "claimed.md"): "claimed-message",
		filepath.Join(fixture.agentDir.path, ".wake.prepared"):             "prepared-sentinel",
		filepath.Join(fixture.agentDir.path, ".wake.ready"):                "ready-sentinel",
		filepath.Join(fixture.agentDir.path, ".wake.notifier"):             "notifier-sentinel",
		filepath.Join(fixture.agentDir.path, ".wake.status"):               "status-sentinel",
		filepath.Join(fixture.agentDir.path, ".wake.marker"):               "marker-sentinel",
		filepath.Join(fixture.agentDir.path, "terminal-output.sentinel"):   "terminal-unchanged",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lockPath := filepath.Join(fixture.agentDir.path, ".wake.lock")
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()
	treeBefore := snapshotLinuxWakeReloadTree(t, fixture.root)

	info, err := os.Lstat(endpoint.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("reload endpoint mode = %v", info.Mode())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		t.Fatalf("reload endpoint owner = %#v", info.Sys())
	}
	if filepath.Dir(endpoint.Path()) != filepath.Clean(fixture.agentDir.path) ||
		!strings.HasPrefix(filepath.Base(endpoint.Path()), ".wr.") {
		t.Fatalf("reload endpoint path = %q", endpoint.Path())
	}

	response, err := sendLinuxWakeReloadTransportRequest(
		t,
		fixture,
		endpoint,
		fixture.request(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if response != `{"status":"unavailable","reason_code":"reload_command_unavailable"}` {
		t.Fatalf("response = %q", response)
	}
	if strings.Contains(response, "ACK") || strings.Contains(response, "QUEUED") {
		t.Fatalf("reload-only refusal exposed a mutation acknowledgement: %q", response)
	}
	handlerDeadline := time.Now().Add(time.Second)
	for endpoint.ActiveHandlers() != 0 && time.Now().Before(handlerDeadline) {
		time.Sleep(time.Millisecond)
	}
	if endpoint.ActiveHandlers() != 0 {
		t.Fatalf("active handlers = %d", endpoint.ActiveHandlers())
	}
	lockAfter, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockBefore, lockAfter) {
		t.Fatal("reload transport mutated the wake lock")
	}
	requireLinuxWakeReloadTreeUnchanged(
		t,
		treeBefore,
		snapshotLinuxWakeReloadTree(t, fixture.root),
	)
	current := inspectWakeLock(fixture.root, fixture.agent)
	if current.Lock.ResumeSchema != 0 || current.Lock.ControlSocket != "" ||
		wakeControlSocketPath(fixture.root, fixture.agent, current.Lock.Generation) != "" {
		t.Fatalf("reload transport enabled Linux advertisement: %#v", current.Lock)
	}
}

func TestLinuxWakeReloadTransportClosesEveryReceivedFD(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()

	var closed atomic.Int32
	originalClose := closeWakeReloadReceivedFD
	closeWakeReloadReceivedFD = func(fd int) error {
		closed.Add(1)
		return unix.Close(fd)
	}
	t.Cleanup(func() { closeWakeReloadReceivedFD = originalClose })

	first := make([]int, 2)
	second := make([]int, 2)
	if err := unix.Pipe2(first, unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	if err := unix.Pipe2(second, unix.O_CLOEXEC); err != nil {
		_ = unix.Close(first[0])
		_ = unix.Close(first[1])
		t.Fatal(err)
	}
	defer func() {
		_ = unix.Close(first[0])
		_ = unix.Close(first[1])
		_ = unix.Close(second[0])
		_ = unix.Close(second[1])
	}()

	response, err := sendLinuxWakeReloadTransportRequest(
		t,
		fixture,
		endpoint,
		fixture.request(),
		first[0],
		second[0],
	)
	requireLinuxWakeReloadSilentRefusal(t, response, err)
	if got := closed.Load(); got != 2 {
		t.Fatalf("received descriptor closes = %d, want 2", got)
	}
}

func TestLinuxWakeReloadTransportClosesParsedFDsOnControlTruncationWithoutLeaks(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	var closed atomic.Int32
	var receivedMu sync.Mutex
	receivedFDs := make(map[int]struct{})
	originalClose := closeWakeReloadReceivedFD
	closeWakeReloadReceivedFD = func(fd int) error {
		receivedMu.Lock()
		receivedFDs[fd] = struct{}{}
		receivedMu.Unlock()
		closed.Add(1)
		return unix.Close(fd)
	}
	t.Cleanup(func() { closeWakeReloadReceivedFD = originalClose })

	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()

	fdCount := linuxWakeReloadMaxReceivedFDs + 4
	pipes := make([][2]int, fdCount)
	rights := make([]int, 0, fdCount)
	for index := range pipes {
		if err := unix.Pipe2(pipes[index][:], unix.O_CLOEXEC); err != nil {
			t.Fatal(err)
		}
		rights = append(rights, pipes[index][0])
	}
	defer func() {
		for _, pipe := range pipes {
			_ = unix.Close(pipe[0])
			_ = unix.Close(pipe[1])
		}
	}()
	scratch := make([][2]int, 0, 32)
	closeScratchPipes := func() {
		for _, pipe := range scratch {
			_ = unix.Close(pipe[0])
			_ = unix.Close(pipe[1])
		}
	}
	for range cap(scratch) {
		var pipe [2]int
		if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC); err != nil {
			closeScratchPipes()
			t.Fatal(err)
		}
		scratch = append(scratch, pipe)
	}
	closeScratch := make(chan struct{})
	scratchClosed := make(chan struct{})
	var closeScratchOnce sync.Once
	closeScratchAndWait := func() {
		closeScratchOnce.Do(func() { close(closeScratch) })
		<-scratchClosed
	}
	t.Cleanup(closeScratchAndWait)
	go func() {
		<-closeScratch
		closeScratchPipes()
		close(scratchClosed)
	}()
	before := linuxOpenFDSet(t)
	response, err := sendLinuxWakeReloadTransportRequest(
		t,
		fixture,
		endpoint,
		fixture.request(),
		rights...,
	)
	requireLinuxWakeReloadSilentRefusal(t, response, err)
	deadline := time.Now().Add(time.Second)
	for endpoint.ActiveHandlers() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := endpoint.ActiveHandlers(); got != 0 {
		t.Fatalf("truncated ancillary handler count = %d", got)
	}
	// Model an unrelated test tearing down descriptors between snapshots.
	closeScratchAndWait()
	if got := closed.Load(); got == 0 || got > int32(linuxWakeReloadMaxReceivedFDs) {
		t.Fatalf("parsed truncated descriptor closes = %d", got)
	}
	after := linuxOpenFDSet(t)
	receivedMu.Lock()
	for fd := range receivedFDs {
		if _, open := after[fd]; open {
			receivedMu.Unlock()
			t.Fatalf("received descriptor %d remained open after handler quiescence", fd)
		}
	}
	receivedMu.Unlock()
	var newFDs []int
	for fd := range after {
		if _, existed := before[fd]; !existed {
			newFDs = append(newFDs, fd)
		}
	}
	if len(newFDs) != 0 {
		sort.Ints(newFDs)
		t.Fatalf("new descriptors remained after truncated control: %v", newFDs)
	}
}

func TestLinuxWakeReloadTransportRejectsWrongCredentialProcessAndRequestIdentity(t *testing.T) {
	t.Run("uid", func(t *testing.T) {
		fixture := newLinuxWakeReloadTransportFixture(t)
		originalCred := linuxWakeReloadGetPeerCred
		linuxWakeReloadGetPeerCred = func(conn *net.UnixConn) (*unix.Ucred, error) {
			cred, err := originalCred(conn)
			if cred != nil {
				copy := *cred
				copy.Uid++
				cred = &copy
			}
			return cred, err
		}
		t.Cleanup(func() { linuxWakeReloadGetPeerCred = originalCred })
		endpoint, err := startLinuxWakeReloadTransport(
			fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
			500*time.Millisecond,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = endpoint.Close() }()
		response, err := sendLinuxWakeReloadTransportRequest(t, fixture, endpoint, fixture.request())
		requireLinuxWakeReloadSilentRefusal(t, response, err)
	})

	t.Run("process", func(t *testing.T) {
		fixture := newLinuxWakeReloadTransportFixture(t)
		originalProcess := linuxWakeReloadPeerProcess
		var snapshots atomic.Int32
		linuxWakeReloadPeerProcess = func(pid int) wakeProcessInfo {
			process := originalProcess(pid)
			if snapshots.Add(1)%2 == 0 {
				process.StartToken = "54321"
			}
			return process
		}
		t.Cleanup(func() { linuxWakeReloadPeerProcess = originalProcess })
		endpoint, err := startLinuxWakeReloadTransport(
			fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
			500*time.Millisecond,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = endpoint.Close() }()
		response, err := sendLinuxWakeReloadTransportRequest(t, fixture, endpoint, fixture.request())
		requireLinuxWakeReloadSilentRefusal(t, response, err)
	})

	for _, mutate := range []struct {
		name string
		fn   func(*wakeReloadTransportRequest)
	}{
		{name: "zero schema", fn: func(request *wakeReloadTransportRequest) { request.Schema = 0 }},
		{name: "future schema", fn: func(request *wakeReloadTransportRequest) { request.Schema = 2 }},
		{name: "owner", fn: func(request *wakeReloadTransportRequest) { request.Owner.ProcessStart = "54321" }},
		{name: "generation", fn: func(request *wakeReloadTransportRequest) { request.Generation = "1123456789abcdef0123456789abcdef" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			fixture := newLinuxWakeReloadTransportFixture(t)
			endpoint, err := startLinuxWakeReloadTransport(
				fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
				500*time.Millisecond,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = endpoint.Close() }()
			request := fixture.request()
			mutate.fn(&request)
			response, err := sendLinuxWakeReloadTransportRequest(t, fixture, endpoint, request)
			requireLinuxWakeReloadSilentRefusal(t, response, err)
		})
	}
}

func TestLinuxWakeReloadTransportRejectsNonCanonicalLiveFraming(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()

	canonical := encodeWakeReloadTransportRequestForTest(t, fixture.request())
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "trailing object", payload: append(append([]byte(nil), canonical...), []byte("{}\n")...)},
		{name: "unterminated", payload: append([]byte(nil), canonical[:len(canonical)-1]...)},
		{name: "leading whitespace", payload: append([]byte(" "), canonical...)},
		{name: "legacy plaintext generation", payload: []byte(fixture.expected.Lock.Generation + "\n")},
		{name: "crlf", payload: append(append([]byte(nil), canonical[:len(canonical)-1]...), '\r', '\n')},
		{name: "oversized", payload: bytes.Repeat([]byte{'x'}, wakeReloadTransportMaxRequestBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, err := sendRawLinuxWakeReloadTransportRequest(t, fixture, endpoint, test.payload)
			requireLinuxWakeReloadSilentRefusal(t, response, err)
		})
	}
}

func TestLinuxWakeReloadTransportRejectsChangedLockAndWrongSession(t *testing.T) {
	t.Run("changed lock", func(t *testing.T) {
		fixture := newLinuxWakeReloadTransportFixture(t)
		endpoint, err := startLinuxWakeReloadTransport(
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			fixture.expected,
			fixture.owner,
			500*time.Millisecond,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = endpoint.Close() }()

		lockPath := filepath.Join(fixture.agentDir.path, ".wake.lock")
		parked := lockPath + ".parked"
		if err := os.Rename(lockPath, parked); err != nil {
			t.Fatal(err)
		}
		writeWakeLockExactForTest(t, fixture.root, fixture.agent, fixture.expected.Lock)
		response, err := sendLinuxWakeReloadTransportRequest(
			t,
			fixture,
			endpoint,
			fixture.request(),
		)
		if !errors.Is(err, os.ErrClosed) && err == nil {
			t.Fatalf("changed-lock request response = %q, want silent refusal", response)
		}
		if response != "" {
			t.Fatalf("changed-lock request response = %q, want empty", response)
		}
	})

	t.Run("wrong peer session", func(t *testing.T) {
		fixture := newLinuxWakeReloadTransportFixture(t)
		originalSID := linuxWakeReloadGetPeerSID
		linuxWakeReloadGetPeerSID = func(int) (int, error) {
			return fixture.owner.SessionID + 1, nil
		}
		t.Cleanup(func() { linuxWakeReloadGetPeerSID = originalSID })
		endpoint, err := startLinuxWakeReloadTransport(
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			fixture.expected,
			fixture.owner,
			500*time.Millisecond,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = endpoint.Close() }()

		response, err := sendLinuxWakeReloadTransportRequest(
			t,
			fixture,
			endpoint,
			fixture.request(),
		)
		if err == nil || response != "" {
			t.Fatalf("wrong-session response = %q, err=%v", response, err)
		}
	})
}

func TestLinuxWakeReloadTransportDeadlineDrainsSilentHandler(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()
	conn, err := dialLinuxWakeReloadTransport(
		fixture.agentDir,
		endpoint.socketName,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := bufio.NewReader(conn).ReadByte(); err == nil {
		t.Fatal("silent handler produced a response")
	}
	deadline := time.Now().Add(time.Second)
	for endpoint.ActiveHandlers() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if endpoint.ActiveHandlers() != 0 {
		t.Fatalf("timed-out active handlers = %d", endpoint.ActiveHandlers())
	}
}

func TestLinuxWakeReloadTransportBoundsHandlersAndAccountsEveryExit(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()

	clients := make([]*net.UnixConn, 0, linuxWakeReloadMaxHandlers)
	defer func() {
		for _, conn := range clients {
			_ = conn.Close()
		}
	}()
	for range linuxWakeReloadMaxHandlers {
		conn, err := dialLinuxWakeReloadTransport(
			fixture.agentDir,
			endpoint.socketName,
			time.Second,
		)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
	}
	deadline := time.Now().Add(time.Second)
	for endpoint.ActiveHandlers() != linuxWakeReloadMaxHandlers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := endpoint.ActiveHandlers(); got != linuxWakeReloadMaxHandlers {
		t.Fatalf("active handlers = %d, want %d", got, linuxWakeReloadMaxHandlers)
	}

	overflow, err := dialLinuxWakeReloadTransport(
		fixture.agentDir,
		endpoint.socketName,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = overflow.Close() }()
	_ = overflow.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := bufio.NewReader(overflow).ReadByte(); err == nil {
		t.Fatal("overflow handler was not refused")
	}
	if got := endpoint.ActiveHandlers(); got > linuxWakeReloadMaxHandlers {
		t.Fatalf("active handlers exceeded bound: %d", got)
	}

	for _, conn := range clients {
		_ = conn.Close()
	}
	deadline = time.Now().Add(time.Second)
	for endpoint.ActiveHandlers() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := endpoint.ActiveHandlers(); got != 0 {
		t.Fatalf("active handlers after client close = %d", got)
	}
}

func TestLinuxWakeReloadTransportRejectsReboundSocketIdentity(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	path := endpoint.Path()
	conn, err := dialLinuxWakeReloadTransport(
		fixture.agentDir,
		endpoint.socketName,
		time.Second,
	)
	if err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := os.Remove(path); err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	sibling, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	sibling.SetUnlinkOnClose(false)
	defer func() {
		_ = sibling.Close()
		_ = os.Remove(path)
	}()
	if err := os.Chmod(path, 0o600); err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	writeErr := writeWakeReloadTransportRequest(conn, fixture.request(), nil)
	response, readErr := bufio.NewReader(conn).ReadString('\n')
	if writeErr == nil {
		requireLinuxWakeReloadSilentRefusal(t, strings.TrimSpace(response), readErr)
	} else if response != "" || readErr == nil {
		t.Fatalf("rebound endpoint write err=%v, response=%q, read err=%v", writeErr, response, readErr)
	}
	if err := endpoint.Close(); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("rebound cleanup error = %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("rebound sibling was removed: %v", err)
	}
}

func TestLinuxWakeReloadTransportCleanupPreservesReboundSiblingIdentity(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	path := endpoint.Path()
	if err := os.Remove(path); err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	sibling, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	sibling.SetUnlinkOnClose(false)
	defer func() {
		_ = sibling.Close()
		_ = os.Remove(path)
	}()
	if err := os.Chmod(path, 0o600); err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	if err := endpoint.Close(); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("rebound cleanup error = %v", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("rebound sibling was removed: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("cleanup replaced or removed rebound sibling identity")
	}
}

func TestLinuxWakeReloadTransportCloseDoesNotWaitForeverForLifecycleGuard(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := os.OpenFile(
		wakeLifecycleGuardPath(fixture.root, fixture.agent),
		os.O_RDWR|syscall.O_CLOEXEC,
		0,
	)
	if err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	defer func() { _ = guard.Close() }()
	if err := unix.Flock(int(guard.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_ = unix.Flock(int(guard.Fd()), unix.LOCK_UN)
		}
	}()
	reachedGuard := make(chan struct{})
	originalHook := linuxWakeReloadBeforeGuard
	var reached atomic.Bool
	linuxWakeReloadBeforeGuard = func() {
		if reached.CompareAndSwap(false, true) {
			close(reachedGuard)
		}
	}
	t.Cleanup(func() { linuxWakeReloadBeforeGuard = originalHook })

	conn, err := dialLinuxWakeReloadTransport(fixture.agentDir, endpoint.socketName, time.Second)
	if err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := writeWakeReloadTransportRequest(conn, fixture.request(), nil); err != nil {
		_ = endpoint.Close()
		t.Fatal(err)
	}
	select {
	case <-reachedGuard:
	case <-time.After(time.Second):
		_ = endpoint.Close()
		t.Fatal("request handler did not reach lifecycle authorization")
	}

	closed := make(chan error, 1)
	go func() { closed <- endpoint.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		_ = unix.Flock(int(guard.Fd()), unix.LOCK_UN)
		locked = false
		<-closed
		t.Fatal("transport.Close blocked on the wake lifecycle guard")
	}
}

func TestLinuxWakeReloadTransportUsesRealPeerCredentialsAndSessions(t *testing.T) {
	fixture := newRealLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()

	t.Run("same session authenticated unavailable", func(t *testing.T) {
		response, err := runLinuxWakeReloadExternalHelper(t, endpoint, fixture.request(), false)
		if err != nil {
			t.Fatalf("external helper: %v", err)
		}
		if got, want := strings.TrimSpace(string(response)),
			`{"status":"unavailable","reason_code":"reload_command_unavailable"}`; got != want {
			t.Fatalf("same-session response = %q, want %q", got, want)
		}
	})

	t.Run("new session silently refused", func(t *testing.T) {
		response, err := runLinuxWakeReloadExternalHelper(t, endpoint, fixture.request(), true)
		if err != nil {
			t.Fatalf("setsid helper: %v", err)
		}
		if len(response) != 0 {
			t.Fatalf("wrong-session response = %q, want empty", response)
		}
	})
}

func TestLinuxWakeReloadTransportRejectedMutationDoesNotSignalOwner(t *testing.T) {
	fixture := newRealLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()
	request := fixture.request()
	request.Operation = "recover"
	response, err := runLinuxWakeReloadExternalHelper(t, endpoint, request, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(response) != 0 {
		t.Fatalf("mutation response = %q, want empty", response)
	}
	if err := syscall.Kill(fixture.owner.PID, 0); err != nil {
		t.Fatalf("cooperative owner was signaled or exited: %v", err)
	}
}

func TestLinuxWakeReloadTransportPublishRefusesConcurrentPublicEntry(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	path := linuxWakeReloadTransportPath(
		fixture.root, fixture.agent, fixture.expected.Lock.Generation,
	)
	originalHook := linuxWakeReloadBeforePublish
	linuxWakeReloadBeforePublish = func(dirfd int, stagingName, publicName string) error {
		return unix.Symlinkat("attacker-owned", dirfd, publicName)
	}
	t.Cleanup(func() { linuxWakeReloadBeforePublish = originalHook })
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
		500*time.Millisecond,
	)
	if err == nil {
		_ = endpoint.Close()
		t.Fatal("concurrent public entry was overwritten")
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if target != "attacker-owned" {
		t.Fatalf("concurrent public entry = %q", target)
	}
}

func TestLinuxWakeReloadTransportCleanupRetiresBeforeExactUnlink(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	path := endpoint.Path()
	originalHook := linuxWakeReloadAfterRetire
	linuxWakeReloadAfterRetire = func(dirfd int, retiredName, publicName string) error {
		return unix.Symlinkat("replacement", dirfd, publicName)
	}
	t.Cleanup(func() { linuxWakeReloadAfterRetire = originalHook })
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if target != "replacement" {
		t.Fatalf("replacement public entry = %q", target)
	}
}

func TestLinuxWakeReloadTransportDoesNotAdvertisePathnameEvidence(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir, fixture.root, fixture.agent, fixture.expected, fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()
	inspection := fixture.expected
	evidence := validWakeReloadTransportRequestForTest().Candidate
	evidence.Platform = "linux"
	evidence.Method = wakeImageMethodPathnameObserved
	inspection.Lock.RunningImageEvidence = &evidence
	inspection.Lock.ImagePath = evidence.ExecutionPath
	inspection.Lock.ImageVersion = evidence.EmbeddedVersion
	decision := classifyWakeCheckReload(fixture.root, fixture.agent, inspection)
	if decision.Status != wakeReloadUnavailable || decision.ReasonCode != wakeReloadReasonNotAdvertised {
		t.Fatalf("listening observational endpoint classification = %#v", decision)
	}
}
