//go:build darwin

package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

var darwinSocketCWDMu sync.Mutex

// XUCRED_VERSION from Darwin's <sys/ucred.h>.
const darwinXUCredVersion uint32 = 0

type wakeControlOwnerRequest struct {
	Generation string     `json:"generation"`
	Owner      *wakeOwner `json:"owner,omitempty"`
	Rollback   bool       `json:"rollback,omitempty"`
}

func wakeControlSocketPath(root, me, generation string) string {
	sum := sha256.Sum256([]byte(canonicalWakeRoot(root) + "\x00" + me + "\x00" + generation))
	return filepath.Join(fsq.AgentBase(root, me), ".w."+hex.EncodeToString(sum[:8]))
}

func removeWakeLockIfUnchangedAt(dirfd int, agentDir *wakeAgentDir, inspection wakeLockInspection) error {
	path := filepath.Join(agentDir.path, ".wake.lock")
	return removeWakeLockIfUnchangedGuardedWithIO(
		inspection,
		func() ([]byte, os.FileInfo, error) { return readWakeLockFileAt(dirfd, path) },
		func() error { return unix.Unlinkat(dirfd, ".wake.lock", 0) },
	)
}

func withDarwinSocketDirFD(dirfd int, fn func() error) error {
	darwinSocketCWDMu.Lock()
	defer darwinSocketCWDMu.Unlock()
	oldfd, err := unix.Open(".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(oldfd) }()
	if err := unix.Fchdir(dirfd); err != nil {
		return err
	}
	callErr := fn()
	restoreErr := unix.Fchdir(oldfd)
	if callErr != nil {
		return callErr
	}
	return restoreErr
}

func listenDarwinUnixAt(agentDir *wakeAgentDir, name string) (*net.UnixListener, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	bound := false
	defer func() {
		if !bound {
			_ = unix.Close(fd)
		}
	}()
	err = agentDir.withFD(func(dirfd int) error {
		return withDarwinSocketDirFD(dirfd, func() error {
			if err := unix.Bind(fd, &unix.SockaddrUnix{Name: name}); err != nil {
				return err
			}
			return unix.Listen(fd, 16)
		})
	})
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "wake-control-listener")
	bound = true
	listenerAny, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	listener, ok := listenerAny.(*net.UnixListener)
	if !ok {
		_ = listenerAny.Close()
		return nil, fmt.Errorf("wake control listener is not unix")
	}
	return listener, nil
}

func dialDarwinUnixAt(agentDir *wakeAgentDir, name string, timeout time.Duration) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	connected := false
	defer func() {
		if !connected {
			_ = unix.Close(fd)
		}
	}()
	err = agentDir.withFD(func(dirfd int) error {
		return withDarwinSocketDirFD(dirfd, func() error {
			return unix.Connect(fd, &unix.SockaddrUnix{Name: name})
		})
	})
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "wake-control-client")
	connected = true
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	return conn, nil
}

func darwinControlSocketName(agentDir *wakeAgentDir, path string) (string, error) {
	cleanPath := filepath.Clean(path)
	name := filepath.Base(cleanPath)
	if filepath.Dir(cleanPath) != filepath.Clean(agentDir.path) || !strings.HasPrefix(name, ".w.") || name == ".w." {
		return "", fmt.Errorf("wake control socket %s is outside authorized agent directory %s", path, agentDir.path)
	}
	return name, nil
}

func darwinControlSocketBasenameForCleanup(agentDir *wakeAgentDir, path string) (string, error) {
	return darwinControlSocketName(agentDir, path)
}

func removeDarwinControlSocketAt(dirfd int, name string) error {
	err := unix.Unlinkat(dirfd, name, 0)
	if err == nil || err == unix.ENOENT {
		return nil
	}
	return err
}

func removeStaleDarwinControlSocketsAt(dirfd int) error {
	scanfd, err := unix.Openat(dirfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	scan := os.NewFile(uintptr(scanfd), "wake-agent-directory-scan")
	defer func() { _ = scan.Close() }()
	entries, err := scan.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".w.") {
			continue
		}
		if err := removeDarwinControlSocketAt(dirfd, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func secureDarwinControlSocketAt(dirfd int, name, path string) error {
	if err := unix.Fchmodat(dirfd, name, 0o600, 0); err != nil {
		return fmt.Errorf("chmod wake control socket %s: %w", path, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat wake control socket %s: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("wake control socket %s is not a socket", path)
	}
	if stat.Mode&0o777 != 0o600 {
		return fmt.Errorf("wake control socket %s mode is %o, want 0600", path, stat.Mode&0o777)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("wake control socket %s is owned by uid %d, want %d", path, stat.Uid, os.Geteuid())
	}
	return nil
}

func darwinPeerEUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred unix.Xucred
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		length := uint32(unsafe.Sizeof(cred))
		_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, uintptr(unix.SOL_LOCAL), uintptr(unix.LOCAL_PEERCRED), uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&length)), 0)
		if errno != 0 {
			sockErr = errno
		}
	})
	if err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, sockErr
	}
	if cred.Version != darwinXUCredVersion {
		return 0, fmt.Errorf("unsupported wake control peer credential version %d", cred.Version)
	}
	return cred.Uid, nil
}

func darwinPeerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int32
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		length := uint32(unsafe.Sizeof(pid))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(unix.SOL_LOCAL),
			uintptr(unix.LOCAL_PEERPID),
			uintptr(unsafe.Pointer(&pid)),
			uintptr(unsafe.Pointer(&length)),
			0,
		)
		if errno != 0 {
			sockErr = errno
		}
	})
	if err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, sockErr
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid wake control peer pid %d", pid)
	}
	return int(pid), nil
}

func captureDarwinControlPeerOwner(pid int) (wakeOwner, error) {
	first := inspectWakeProcess(pid)
	firstSessionID, firstSessionErr := getWakeProcessSID(pid)
	if !first.Running || first.StartToken == "" || first.BootID == "" || firstSessionErr != nil {
		return wakeOwner{}, fmt.Errorf("wake control peer identity is incomplete")
	}
	peer := wakeOwner{
		PID:          pid,
		ProcessStart: first.StartToken,
		BootID:       first.BootID,
		SessionID:    firstSessionID,
	}
	if err := validateAuthoritativeWakeOwner(peer); err != nil {
		return wakeOwner{}, err
	}
	second := inspectWakeProcess(pid)
	secondSessionID, secondSessionErr := getWakeProcessSID(pid)
	state, reason := classifyStableAuthoritativeWakeOwner(
		peer,
		first, firstSessionID, firstSessionErr,
		second, secondSessionID, secondSessionErr,
	)
	if state != wakeOwnerSame {
		return wakeOwner{}, fmt.Errorf("wake control peer identity is %s: %s", state, reason)
	}
	return peer, nil
}

func authorizeDarwinOwnerControlAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	expected wakeLock,
	request wakeControlOwnerRequest,
	peerPID int,
	peerUID uint32,
) (wakeLockInspection, *wakeTarget, error) {
	if peerUID != uint32(os.Geteuid()) {
		return wakeLockInspection{}, nil, fmt.Errorf("wake control peer uid is not authorized")
	}
	current := inspectWakeLockAt(dirfd, agentDir, root, me)
	if !current.Exists ||
		current.Lock.Generation != expected.Generation ||
		current.Lock.ControlSocket != expected.ControlSocket ||
		request.Generation != expected.Generation {
		return wakeLockInspection{}, nil, fmt.Errorf("authoritative wake generation changed")
	}
	if classifyPersistedWakeClaim(current) != wakeClaimAuthoritative {
		return wakeLockInspection{}, nil, fmt.Errorf("wake control target is not an authoritative owner claim")
	}
	target, err := authoritativeWakeRecoveryTargetAt(dirfd, agentDir, current)
	if err != nil {
		return wakeLockInspection{}, nil, err
	}
	observation, err := observeAuthoritativeWakeOwner(*current.Lock.Owner)
	defer func() { _ = observation.Close() }()
	if err != nil {
		return wakeLockInspection{}, nil, err
	}
	switch observation.State {
	case wakeOwnerDead:
		return current, target, nil
	case wakeOwnerSame:
		if request.Rollback {
			peer, err := captureDarwinControlPeerOwner(peerPID)
			if err == nil && sameWakeOwner(&peer, current.Lock.Owner) {
				return current, target, nil
			}
			return wakeLockInspection{}, nil, fmt.Errorf("wake control rollback peer is not the exact owner")
		}
		if request.Owner == nil {
			return wakeLockInspection{}, nil, fmt.Errorf("wake control owner token is missing")
		}
		if err := validateAuthoritativeWakeOwner(*request.Owner); err != nil {
			return wakeLockInspection{}, nil, fmt.Errorf("wake control owner token is invalid: %w", err)
		}
		if !sameWakeOwner(request.Owner, current.Lock.Owner) {
			return wakeLockInspection{}, nil, fmt.Errorf("wake control owner token does not match the claim")
		}
		peerSession, err := getWakeProcessSID(peerPID)
		if err != nil {
			return wakeLockInspection{}, nil, fmt.Errorf("wake control peer session unavailable: %w", err)
		}
		if peerSession != current.Lock.Owner.SessionID {
			return wakeLockInspection{}, nil, fmt.Errorf(
				"wake control peer session %d does not match owner session %d",
				peerSession,
				current.Lock.Owner.SessionID,
			)
		}
		return current, target, nil
	default:
		return wakeLockInspection{}, nil, fmt.Errorf("wake control owner is unknown: %s", observation.Reason)
	}
}

func handleDarwinOwnerControl(
	conn *net.UnixConn,
	agentDir *wakeAgentDir,
	root string,
	me string,
	lock wakeLock,
	request wakeControlOwnerRequest,
	peerPID int,
	peerUID uint32,
	stopRequest chan<- struct{},
	loopStopped <-chan struct{},
) {
	authorized := false
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		_, _, err := authorizeDarwinOwnerControlAt(
			dirfd,
			agentDir,
			root,
			me,
			lock,
			request,
			peerPID,
			peerUID,
		)
		if err != nil {
			return err
		}
		authorized = true
		return nil
	})
	if err != nil || !authorized {
		return
	}

	_ = conn.SetDeadline(time.Time{})
	select {
	case stopRequest <- struct{}{}:
	default:
	}
	<-loopStopped

	removed := false
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current, target, err := authorizeDarwinOwnerControlAt(
			dirfd,
			agentDir,
			root,
			me,
			lock,
			request,
			peerPID,
			peerUID,
		)
		if err != nil {
			return err
		}
		if err := removeAuthoritativeWakeClaimAt(dirfd, agentDir, current, target); err != nil {
			return err
		}
		removed = true
		return nil
	})
	if err != nil || !removed {
		return
	}
	_, _ = conn.Write([]byte("ACK\n"))
}

func startWakeControlListener(root, me string, lock wakeLock) (func(), <-chan struct{}, func(), error) {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup, stop, markStopped, err := startWakeControlListenerInDirOwned(
		agentDir,
		root,
		me,
		lock,
		true,
	)
	if err != nil {
		_ = agentDir.Close()
	}
	return cleanup, stop, markStopped, err
}

func startWakeControlListenerInDir(
	agentDir *wakeAgentDir,
	root, me string,
	lock wakeLock,
) (func(), <-chan struct{}, func(), error) {
	return startWakeControlListenerInDirOwned(agentDir, root, me, lock, false)
}

func startWakeControlListenerInDirOwned(
	agentDir *wakeAgentDir,
	root, me string,
	lock wakeLock,
	closeAgentDir bool,
) (func(), <-chan struct{}, func(), error) {
	path := lock.ControlSocket
	if path == "" {
		return func() {}, nil, func() {}, nil
	}
	if agentDir == nil {
		return nil, nil, nil, fmt.Errorf("wake agent directory capability is missing")
	}
	name, err := darwinControlSocketName(agentDir, path)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !current.Exists || current.Lock.Generation != lock.Generation || current.Lock.ControlSocket != path {
			return fmt.Errorf("wake control metadata changed before listener start")
		}
		return removeStaleDarwinControlSocketsAt(dirfd)
	}); err != nil {
		return nil, nil, nil, err
	}
	listener, err := listenDarwinUnixAt(agentDir, name)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := agentDir.withFD(func(dirfd int) error {
		return secureDarwinControlSocketAt(dirfd, name, path)
	}); err != nil {
		_ = listener.Close()
		_ = agentDir.withFD(func(dirfd int) error { return removeDarwinControlSocketAt(dirfd, name) })
		return nil, nil, nil, err
	}
	stopRequest := make(chan struct{}, 1)
	loopStopped := make(chan struct{})
	var loopStoppedOnce sync.Once
	markLoopStopped := func() { loopStoppedOnce.Do(func() { close(loopStopped) }) }
	go func() {
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			go func(conn *net.UnixConn) {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				uid, err := darwinPeerEUID(conn)
				if err != nil || uid != uint32(os.Geteuid()) {
					return
				}
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				if lock.WakeMode == wakeOwnerWakeMode {
					var request wakeControlOwnerRequest
					if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &request); err != nil {
						return
					}
					peerPID, err := darwinPeerPID(conn)
					if err != nil {
						return
					}
					handleDarwinOwnerControl(
						conn,
						agentDir,
						root,
						me,
						lock,
						request,
						peerPID,
						uid,
						stopRequest,
						loopStopped,
					)
					return
				}
				if strings.TrimSpace(line) != lock.Generation {
					return
				}
				accepted := false
				err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
					current := inspectWakeLockAt(dirfd, agentDir, root, me)
					if !current.Exists || current.Lock.Generation != lock.Generation || current.Lock.ControlSocket != path {
						return nil
					}
					if err := validateWakeLockOwnerlessMutation(current); err != nil {
						return err
					}
					accepted = true
					return nil
				})
				if err != nil || !accepted {
					return
				}
				// Authentication is bounded, but completion is bounded by the
				// configured inject-via execution timeout. Keep the generation
				// published until the loop has actually quiesced so a concurrent
				// acquire cannot start a second injector.
				_ = conn.SetDeadline(time.Time{})
				select {
				case stopRequest <- struct{}{}:
				default:
				}
				<-loopStopped
				removed := false
				err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
					current := inspectWakeLockAt(dirfd, agentDir, root, me)
					if !current.Exists || current.Lock.Generation != lock.Generation {
						removed = true
						return nil
					}
					if current.Lock.ControlSocket != path {
						return nil
					}
					if err := validateWakeLockOwnerlessMutation(current); err != nil {
						return err
					}
					if err := removeWakeLockIfUnchangedAt(dirfd, agentDir, current); err != nil {
						return err
					}
					removed = true
					return nil
				})
				if err != nil || !removed {
					return
				}
				_, _ = conn.Write([]byte("ACK\n"))
			}(conn)
		}
	}()
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			_ = listener.Close()
			_ = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
				if cur := inspectWakeLockAt(dirfd, agentDir, root, me); cur.Exists && cur.Lock.Generation != lock.Generation {
					return nil
				}
				return removeDarwinControlSocketAt(dirfd, name)
			})
			if closeAgentDir {
				_ = agentDir.Close()
			}
		})
	}
	return cleanup, stopRequest, markLoopStopped, nil
}

func cooperativeStopInjectVia(i wakeLockInspection) (bool, error) {
	if i.Lock.ControlSocket == "" || i.Lock.Generation == "" {
		return false, fmt.Errorf("live inject-via wake orphan has no cooperative control endpoint; stop the owning supervisor")
	}
	agentDir, err := openWakeAgentDir(i.Root, i.Agent)
	if err != nil {
		return false, fmt.Errorf("cooperative wake stop unavailable: %w", err)
	}
	defer func() { _ = agentDir.Close() }()
	name, err := darwinControlSocketName(agentDir, i.Lock.ControlSocket)
	if err != nil {
		return false, fmt.Errorf("cooperative wake stop unavailable: %w", err)
	}
	conn, err := dialDarwinUnixAt(agentDir, name, 2*time.Second)
	if err != nil {
		return false, fmt.Errorf("cooperative wake stop unavailable: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = fmt.Fprintf(conn, "%s\n", i.Lock.Generation); err != nil {
		return false, err
	}
	_ = conn.SetDeadline(time.Time{})
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ACK" {
		return false, fmt.Errorf("cooperative wake stop refused")
	}
	var gone bool
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		cur := inspectWakeLockAt(dirfd, agentDir, i.Root, i.Agent)
		gone = !cur.Exists || cur.Lock.Generation != i.Lock.Generation
		return nil
	})
	return gone, err
}

func cooperativeStopAuthoritativeWake(
	i wakeLockInspection,
	auth wakeOwnerReleaseAuthorization,
) (bool, error) {
	if i.Lock.WakeMode != wakeOwnerWakeMode ||
		i.Lock.ControlSocket == "" ||
		i.Lock.Generation == "" ||
		i.Lock.Owner == nil {
		return false, fmt.Errorf("authoritative wake has no cooperative control endpoint")
	}
	request := wakeControlOwnerRequest{
		Generation: i.Lock.Generation,
		Rollback:   auth.Rollback,
	}
	if auth.Token != nil {
		token := *auth.Token
		request.Owner = &token
	}
	data, err := json.Marshal(request)
	if err != nil {
		return false, fmt.Errorf("marshal authoritative wake stop request: %w", err)
	}
	agentDir, err := openWakeAgentDir(i.Root, i.Agent)
	if err != nil {
		return false, fmt.Errorf("cooperative authoritative wake stop unavailable: %w", err)
	}
	defer func() { _ = agentDir.Close() }()
	name, err := darwinControlSocketName(agentDir, i.Lock.ControlSocket)
	if err != nil {
		return false, fmt.Errorf("cooperative authoritative wake stop unavailable: %w", err)
	}
	conn, err := dialDarwinUnixAt(agentDir, name, 2*time.Second)
	if err != nil {
		return false, fmt.Errorf("cooperative authoritative wake stop unavailable: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return false, err
	}
	_ = conn.SetDeadline(time.Time{})
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ACK" {
		return false, fmt.Errorf("cooperative authoritative wake stop refused")
	}
	gone := false
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, i.Root, i.Agent)
		gone = !current.Exists || current.Lock.Generation != i.Lock.Generation
		return nil
	})
	return gone, err
}
