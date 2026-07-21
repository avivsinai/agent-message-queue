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

func withDarwinSocketDir(dir string, fn func() error) error {
	dirfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dirfd) }()
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

func listenDarwinUnix(path string) (*net.UnixListener, error) {
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
	err = withDarwinSocketDir(filepath.Dir(path), func() error {
		if err := unix.Bind(fd, &unix.SockaddrUnix{Name: filepath.Base(path)}); err != nil {
			return err
		}
		return unix.Listen(fd, 16)
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

func dialDarwinUnix(path string, timeout time.Duration) (net.Conn, error) {
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
	err = withDarwinSocketDir(filepath.Dir(path), func() error {
		return unix.Connect(fd, &unix.SockaddrUnix{Name: filepath.Base(path)})
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
	if err := withWakeLifecycleGuard(root, me, func() error {
		current := inspectWakeLock(root, me)
		if !current.Exists || current.Lock.Generation != lock.Generation || current.Lock.ControlSocket != path {
			return fmt.Errorf("wake control metadata changed before listener start")
		}
		stale, err := filepath.Glob(filepath.Join(fsq.AgentBase(root, me), ".w.*"))
		if err != nil {
			return err
		}
		for _, candidate := range stale {
			if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}
	listener, err := listenDarwinUnix(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
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
				token, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil || strings.TrimSpace(token) != lock.Generation {
					return
				}
				removed := false
				err = withWakeLifecycleGuard(root, me, func() error {
					current := inspectWakeLock(root, me)
					if !current.Exists || current.Lock.Generation != lock.Generation || current.Lock.ControlSocket != path {
						return nil
					}
					if err := removeWakeLockIfUnchangedGuarded(current); err != nil {
						return err
					}
					removed = true
					return nil
				})
				if err != nil || !removed {
					return
				}
				select {
				case stopRequest <- struct{}{}:
				default:
				}
				<-loopStopped
				_, _ = conn.Write([]byte("ACK\n"))
			}(conn)
		}
	}()
	cleanup := func() {
		_ = listener.Close()
		_ = withWakeLifecycleGuard(root, me, func() error {
			if cur := inspectWakeLock(root, me); cur.Exists && cur.Lock.Generation != lock.Generation {
				return nil
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		})
	}
	return cleanup, stopRequest, markLoopStopped, nil
}

func cooperativeStopInjectVia(i wakeLockInspection) (bool, error) {
	if i.Lock.ControlSocket == "" || i.Lock.Generation == "" {
		return false, fmt.Errorf("live inject-via wake orphan has no cooperative control endpoint; stop the owning supervisor")
	}
	conn, err := dialDarwinUnix(i.Lock.ControlSocket, 2*time.Second)
	if err != nil {
		return false, fmt.Errorf("cooperative wake stop unavailable: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = fmt.Fprintf(conn, "%s\n", i.Lock.Generation); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ACK" {
		return false, fmt.Errorf("cooperative wake stop refused")
	}
	var gone bool
	err = withWakeLifecycleGuard(i.Root, i.Agent, func() error {
		cur := inspectWakeLock(i.Root, i.Agent)
		gone = !cur.Exists || cur.Lock.Generation != i.Lock.Generation
		return nil
	})
	return gone, err
}
