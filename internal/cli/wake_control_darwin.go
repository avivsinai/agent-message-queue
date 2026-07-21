//go:build darwin

package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
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

func wakeControlSocketPath(root, me, generation string) string {
	sum := sha256.Sum256([]byte(canonicalWakeRoot(root) + "\x00" + me + "\x00" + generation))
	return filepath.Join(fsq.AgentBase(root, me), ".w."+hex.EncodeToString(sum[:8]))
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

func readWakeLockFileAt(dirfd int, path string) ([]byte, os.FileInfo, error) {
	open := func() (*os.File, error) {
		fd, err := unix.Openat(dirfd, ".wake.lock", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat wake lock: %w", err)
	}
	if err := validateWakeLockFile(path, info); err != nil {
		return nil, nil, err
	}
	data, err := readWakeMetadata(file, "wake lock", path)
	if err != nil {
		return nil, nil, err
	}
	pathFile, err := open()
	if err != nil {
		return nil, nil, err
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return nil, nil, fmt.Errorf("re-stat wake lock: %w", statErr)
	}
	if err := validateWakeLockFile(path, pathInfo); err != nil {
		return nil, nil, err
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		return nil, nil, fmt.Errorf("wake lock %s changed while opening", path)
	}
	return data, info, nil
}

func inspectWakeLockAt(dirfd int, agentDir *wakeAgentDir, root, me string) wakeLockInspection {
	path := filepath.Join(agentDir.path, ".wake.lock")
	return inspectWakeLockWithReader(root, me, path, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileAt(dirfd, path)
	})
}

func removeWakeLockIfUnchangedAt(dirfd int, agentDir *wakeAgentDir, inspection wakeLockInspection) error {
	path := filepath.Join(agentDir.path, ".wake.lock")
	return removeWakeLockIfUnchangedGuardedWithIO(
		inspection,
		func() ([]byte, os.FileInfo, error) { return readWakeLockFileAt(dirfd, path) },
		func() error { return unix.Unlinkat(dirfd, ".wake.lock", 0) },
	)
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
	return cred.Uid, nil
}

func startWakeControlListener(root, me string, lock wakeLock) (func(), <-chan struct{}, func(), error) {
	path := lock.ControlSocket
	if path == "" {
		return func() {}, nil, func() {}, nil
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, nil, nil, err
	}
	keepAgentDir := false
	defer func() {
		if !keepAgentDir {
			_ = agentDir.Close()
		}
	}()
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
	keepAgentDir = true
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
				token, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil || strings.TrimSpace(token) != lock.Generation {
					return
				}
				accepted := false
				err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
					current := inspectWakeLockAt(dirfd, agentDir, root, me)
					if !current.Exists || current.Lock.Generation != lock.Generation || current.Lock.ControlSocket != path {
						return nil
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
			_ = agentDir.Close()
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
