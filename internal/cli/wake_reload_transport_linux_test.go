//go:build linux

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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

func requireLinuxWakeReloadSilentRefusal(t *testing.T, response string, err error) {
	t.Helper()
	if err == nil || response != "" {
		t.Fatalf("wake reload refusal response = %q, err=%v", response, err)
	}
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
