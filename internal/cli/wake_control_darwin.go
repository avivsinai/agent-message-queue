//go:build darwin

package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	RequestID  string     `json:"request_id,omitempty"`
	Owner      *wakeOwner `json:"owner,omitempty"`
	Rollback   bool       `json:"rollback,omitempty"`
	Operation  string     `json:"operation,omitempty"`
}

const wakeControlRestartOperation = "restart"

func wakeControlSocketPath(root, me, generation string) string {
	root = canonicalWakeRoot(root)
	sum := sha256.Sum256([]byte(root + "\x00" + me + "\x00" + generation))
	return filepath.Join(fsq.AgentBase(root, me), ".w."+hex.EncodeToString(sum[:8]))
}

const wakeControlInjectViaACKMaxBytes = 256

func wakeControlResidueACK(err error) string {
	residue := wakeLockRemovalResiduesFromError(err)
	if len(residue) == 0 {
		return "ACK RESIDUE\n"
	}
	tokens := make([]string, 0, len(residue))
	for _, cause := range residue {
		switch cause {
		case wakeLockResidueDurability:
			tokens = append(tokens, "durability")
		case wakeLockResidueDetachedCleanup:
			tokens = append(tokens, "detached-cleanup")
		default:
			return "ACK RESIDUE\n"
		}
	}
	return "ACK RESIDUE " + strings.Join(tokens, ",") + "\n"
}

func parseWakeControlResidueACK(line string) ([]wakeLockRemovalResidue, error) {
	response := strings.TrimSpace(line)
	switch response {
	case "ACK":
		return nil, nil
	case "ACK RESIDUE":
		return []wakeLockRemovalResidue{wakeLockResidueCleanup}, nil
	}
	const prefix = "ACK RESIDUE "
	if !strings.HasPrefix(response, prefix) {
		return nil, fmt.Errorf("cooperative wake stop refused")
	}
	var residue []wakeLockRemovalResidue
	for _, token := range strings.Split(strings.TrimPrefix(response, prefix), ",") {
		var cause wakeLockRemovalResidue
		switch token {
		case "durability":
			cause = wakeLockResidueDurability
		case "detached-cleanup":
			cause = wakeLockResidueDetachedCleanup
		default:
			return nil, fmt.Errorf("cooperative wake stop refused")
		}
		next := appendWakeLockRemovalResidue(residue, cause)
		if len(next) == len(residue) {
			return nil, fmt.Errorf("cooperative wake stop refused")
		}
		residue = next
	}
	return residue, nil
}

func wakeControlResidueError(cause wakeLockRemovalResidue) error {
	var detail string
	switch cause {
	case wakeLockResidueDurability:
		detail = "listener removed the exact wake lock but could not confirm its durability"
	case wakeLockResidueDetachedCleanup:
		detail = "listener removed the exact wake lock from detached retained authority; preserving detached wake artifacts"
	default:
		detail = "listener removed the exact wake lock but reported cleanup residue"
	}
	return newWakeLockResidueError(cause, errors.New(detail))
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
	if !filepath.IsAbs(cleanPath) ||
		canonicalWakeRoot(filepath.Dir(cleanPath)) != canonicalWakeRoot(agentDir.path) ||
		!strings.HasPrefix(name, ".w.") || name == ".w." {
		return "", fmt.Errorf("wake control socket %s is outside authorized agent directory %s", path, agentDir.path)
	}
	return name, nil
}

func darwinControlSocketBasenameForCleanup(agentDir *wakeAgentDir, path string) (string, error) {
	return darwinControlSocketName(agentDir, path)
}

func removeDarwinControlSocketAt(dirfd int, name string) error {
	if err := assertNotWakeLockName(name); err != nil {
		return err
	}
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
	scope *wakeMutationScope,
	root string,
	me string,
	expected wakeLock,
	request wakeControlOwnerRequest,
	peerPID int,
	peerUID uint32,
) (authorized wakeLockInspection, authorizedTarget *wakeTarget, retErr error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return wakeLockInspection{}, nil, err
	}
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
	if err := validateBoundWakeMutationAt(scope, current); err != nil {
		return wakeLockInspection{}, nil, err
	}
	if classifyPersistedWakeClaim(current) != wakeClaimAuthoritative {
		return wakeLockInspection{}, nil, fmt.Errorf("wake control target is not an authoritative owner claim")
	}
	target, err := authoritativeWakeRecoveryTargetAt(scope, current)
	if err != nil {
		return wakeLockInspection{}, nil, err
	}
	observation, err := observeAuthoritativeWakeOwner(*current.Lock.Owner)
	defer func() {
		if closeErr := observation.Close(); closeErr != nil {
			authorized = wakeLockInspection{}
			authorizedTarget = nil
			retErr = errors.Join(retErr, closeErr)
		}
	}()
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
	testHooks *darwinWakeControlTestHooks,
) {
	authorized := false
	err := withExistingWakeMutationScopeNoWaitInDir(
		agentDir,
		func(scope *wakeMutationScope) error {
			_, _, err := scope.location()
			if err != nil {
				return err
			}
			_, _, err = authorizeDarwinOwnerControlAt(
				scope,
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
			if err := scope.queueStopRequest(stopRequest); err != nil {
				return err
			}
			authorized = true
			return nil
		},
	)
	if err != nil || !authorized {
		return
	}

	_ = conn.SetDeadline(time.Time{})
	<-loopStopped
	if testHooks != nil && testHooks.afterLoopStopped != nil {
		testHooks.afterLoopStopped()
	}

	removed := false
	err = withExistingWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		_, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		current, target, err := authorizeDarwinOwnerControlAt(
			scope,
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
		if err := removeAuthoritativeWakeClaimAt(scope, current, target); err != nil {
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

func authorizeDarwinWakeRestartControlAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	expected wakeLock,
	request wakeControlOwnerRequest,
	peerPID int,
	peerUID uint32,
) (retErr error) {
	canonicalRoot := canonicalWakeRoot(root)
	if request.Operation != wakeControlRestartOperation {
		return fmt.Errorf("wake restart control operation is unsupported")
	}
	if request.Rollback {
		return fmt.Errorf("wake restart control refuses rollback authorization")
	}
	if peerUID != uint32(os.Geteuid()) {
		return fmt.Errorf("wake restart control peer uid is not authorized")
	}
	if !validWakeReloadTransportGeneration(request.RequestID) {
		return fmt.Errorf("wake restart control request id is malformed")
	}
	if request.Owner == nil {
		return fmt.Errorf("wake restart control owner token is missing")
	}
	if err := validateAuthoritativeWakeOwner(*request.Owner); err != nil {
		return fmt.Errorf("wake restart control owner token is invalid: %w", err)
	}

	current := inspectWakeLockAt(dirfd, agentDir, root, me)
	if !current.Exists || current.Status != wakeLockValid || !current.IdentityConfirmed ||
		expected.Root != canonicalRoot || expected.Agent != me ||
		current.Lock.Root != canonicalRoot || current.Lock.Agent != me ||
		current.Lock.PID != expected.PID ||
		current.Lock.ProcessStart != expected.ProcessStart ||
		current.Lock.BootID != expected.BootID ||
		current.Lock.Generation != expected.Generation ||
		request.Generation != expected.Generation ||
		current.Lock.ControlSocket != expected.ControlSocket {
		return fmt.Errorf("authoritative wake generation changed")
	}
	if expected.ControlSocket != wakeControlSocketPath(root, me, expected.Generation) ||
		current.Lock.ControlSocket != wakeControlSocketPath(root, me, current.Lock.Generation) {
		return fmt.Errorf("wake restart control socket does not match the exact root, agent, and generation")
	}
	if expected.ResumeSchema != wakeResumeSchemaV2 || current.Lock.ResumeSchema != wakeResumeSchemaV2 ||
		expected.ResumeOwner == nil || current.Lock.ResumeOwner == nil {
		return fmt.Errorf("wake restart control target is not resumable")
	}
	if err := validateAuthoritativeWakeOwner(*current.Lock.ResumeOwner); err != nil {
		return fmt.Errorf("wake restart control advertised owner is invalid: %w", err)
	}
	if err := validateWakeRestartTransportPlatform(current.Lock, root, me); err != nil {
		return err
	}
	if !sameWakeOwner(request.Owner, current.Lock.ResumeOwner) ||
		!sameWakeOwner(expected.ResumeOwner, current.Lock.ResumeOwner) {
		return fmt.Errorf("wake restart control owner token does not match the claim")
	}

	observation, err := observeAuthoritativeWakeOwner(*current.Lock.ResumeOwner)
	defer func() {
		if closeErr := observation.Close(); closeErr != nil {
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	if err != nil {
		return err
	}
	if observation.State != wakeOwnerSame {
		return fmt.Errorf("wake restart control owner is %s: %s", observation.State, observation.Reason)
	}
	peerSession, err := getWakeProcessSID(peerPID)
	if err != nil {
		return fmt.Errorf("wake restart control peer session unavailable: %w", err)
	}
	if peerSession != current.Lock.ResumeOwner.SessionID {
		return fmt.Errorf(
			"wake restart control peer session %d does not match owner session %d",
			peerSession,
			current.Lock.ResumeOwner.SessionID,
		)
	}

	record, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
	if err != nil {
		return err
	}
	if !exists || record.Status != wakeRestartPending ||
		record.RequestID != request.RequestID ||
		record.Generation != expected.Generation ||
		record.Root != canonicalRoot || record.Agent != me ||
		!sameWakeOwner(&record.Owner, current.Lock.ResumeOwner) {
		return fmt.Errorf("wake restart control request does not match the exact pending record")
	}
	return nil
}

func handleDarwinWakeRestartControl(
	conn *net.UnixConn,
	agentDir *wakeAgentDir,
	root string,
	me string,
	lock wakeLock,
	request wakeControlOwnerRequest,
	peerPID int,
	peerUID uint32,
	restartSignals chan<- os.Signal,
) {
	if restartSignals == nil {
		return
	}
	err := withExistingWakeMutationScopeNoWaitInDir(
		agentDir,
		func(scope *wakeMutationScope) error {
			dirfd, _, err := scope.location()
			if err != nil {
				return err
			}
			if err := authorizeDarwinWakeRestartControlAt(
				dirfd,
				agentDir,
				root,
				me,
				lock,
				request,
				peerPID,
				peerUID,
			); err != nil {
				return err
			}
			return scope.queueRestartSignal(restartSignals, syscall.SIGUSR1)
		},
	)
	if err != nil {
		return
	}
	_, _ = conn.Write([]byte("ACK\n"))
}

func startWakeControlListener(
	root, me string,
	lock wakeLock,
) (func(), <-chan struct{}, func(), error) {
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
		nil,
	)
	if err != nil {
		_ = agentDir.Close()
	}
	return cleanup, stop, markStopped, err
}

func startWakeControlListenerInDirWithRestart(
	agentDir *wakeAgentDir,
	root, me string,
	lock wakeLock,
	restartSignals chan<- os.Signal,
) (func(), <-chan struct{}, func(), error) {
	return startWakeControlListenerInDirOwnedWithRestart(
		agentDir,
		root,
		me,
		lock,
		false,
		nil,
		restartSignals,
	)
}

type darwinWakeControlTestHooks struct {
	afterLoopStopped func()
}

func startWakeControlListenerInDirOwned(
	agentDir *wakeAgentDir,
	root, me string,
	lock wakeLock,
	closeAgentDir bool,
	testHooks *darwinWakeControlTestHooks,
) (func(), <-chan struct{}, func(), error) {
	return startWakeControlListenerInDirOwnedWithRestart(
		agentDir,
		root,
		me,
		lock,
		closeAgentDir,
		testHooks,
		nil,
	)
}

func startWakeControlListenerInDirOwnedWithRestart(
	agentDir *wakeAgentDir,
	root, me string,
	lock wakeLock,
	closeAgentDir bool,
	testHooks *darwinWakeControlTestHooks,
	restartSignals chan<- os.Signal,
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
		if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
			return err
		}
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
	acceptDone := make(chan struct{})
	var handlers sync.WaitGroup
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func(conn *net.UnixConn) {
				defer handlers.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				uid, err := darwinPeerEUID(conn)
				if err != nil || uid != uint32(os.Geteuid()) {
					return
				}
				line, err := bufio.NewReader(io.LimitReader(conn, 4097)).ReadString('\n')
				if err != nil || len(line) > 4096 {
					return
				}
				if lock.WakeMode == wakeOwnerWakeMode ||
					(lock.ResumeSchema == wakeResumeSchemaV2 && lock.ControlSocket != "") {
					var request wakeControlOwnerRequest
					if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &request); err != nil {
						return
					}
					peerPID, err := darwinPeerPID(conn)
					if err != nil {
						return
					}
					switch request.Operation {
					case "":
						if lock.WakeMode != wakeOwnerWakeMode {
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
							testHooks,
						)
					case wakeControlRestartOperation:
						handleDarwinWakeRestartControl(
							conn,
							agentDir,
							root,
							me,
							lock,
							request,
							peerPID,
							uid,
							restartSignals,
						)
					default:
						return
					}
					return
				}
				fields := strings.Fields(line)
				if len(fields) < 1 || len(fields) > 2 || fields[0] != lock.Generation {
					return
				}
				requestedTargetDigest := ""
				if len(fields) == 2 {
					requestedTargetDigest = fields[1]
				}
				accepted := false
				err = withExistingWakeMutationScopeNoWaitInDir(
					agentDir,
					func(scope *wakeMutationScope) error {
						dirfd, _, err := scope.location()
						if err != nil {
							return err
						}
						current := inspectWakeLockAt(dirfd, agentDir, root, me)
						if !current.Exists || current.Lock.Generation != lock.Generation || current.Lock.ControlSocket != path {
							return nil
						}
						if err := validateWakeLockOwnerlessMutationAtForTermination(dirfd, agentDir, current); err != nil {
							return err
						}
						if err := validateRequestedWakeTargetDigestAt(dirfd, agentDir, current, requestedTargetDigest); err != nil {
							return err
						}
						if err := scope.queueStopRequest(stopRequest); err != nil {
							return err
						}
						accepted = true
						return nil
					},
				)
				if err != nil || !accepted {
					return
				}
				// Authentication is bounded, but completion is bounded by the
				// configured inject-via execution timeout. Keep the generation
				// published until the loop has actually quiesced so a concurrent
				// acquire cannot start a second injector.
				_ = conn.SetDeadline(time.Time{})
				<-loopStopped
				if testHooks != nil && testHooks.afterLoopStopped != nil {
					testHooks.afterLoopStopped()
				}
				var removal wakeLockRemovalOutcome
				err = withExistingWakeMutationScopeNoWaitInDir(
					agentDir,
					func(scope *wakeMutationScope) error {
						dirfd, _, err := scope.location()
						if err != nil {
							return err
						}
						current := inspectWakeLockAt(dirfd, agentDir, root, me)
						if !current.Exists || current.Lock.Generation != lock.Generation ||
							current.Lock.ControlSocket != path {
							return nil
						}
						if err := validateWakeLockOwnerlessMutationAtForTermination(dirfd, agentDir, current); err != nil {
							return err
						}
						if err := validateRequestedWakeTargetDigestAt(dirfd, agentDir, current, requestedTargetDigest); err != nil {
							return err
						}
						removal = removeWakeLockIfUnchangedGuardedAtDurableOutcome(
							scope,
							current,
							scope.unlinkWakeLockForCleanup,
						)
						return nil
					},
				)
				if err != nil || !removal.Committed {
					return
				}
				if removal.Err != nil {
					_, _ = conn.Write([]byte(wakeControlResidueACK(removal.Err)))
					return
				}
				_, _ = conn.Write([]byte("ACK\n"))
			}(conn)
		}
	}()
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			markLoopStopped()
			_ = listener.Close()
			<-acceptDone
			handlers.Wait()
			_ = withExistingWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
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
	var exposedStop <-chan struct{} = stopRequest
	// AMQ <=0.49.10 published control sockets for raw and paste owner locks but
	// did not expose cooperative stop to their notification loops. Keep that
	// cross-version behavior until a lock schema can prove the owner supports
	// quiescing these modes before claim removal.
	if lock.WakeMode == wakeInjectModeRaw || lock.WakeMode == wakeInjectModePaste {
		exposedStop = nil
	}
	return cleanup, exposedStop, markLoopStopped, nil
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
	if err := withExistingWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, i.Root, i.Agent)
		if !sameWakeLockGeneration(i, current) {
			return fmt.Errorf("wake generation changed before cooperative stop")
		}
		return validateWakeStateAgentDirAt(dirfd, agentDir)
	}); err != nil {
		return false, fmt.Errorf("cooperative wake stop unavailable: %w", err)
	}
	return cooperativeStopInjectViaInDir(agentDir, i, nil)
}

func cooperativeStopInjectViaInDir(
	agentDir *wakeAgentDir,
	i wakeLockInspection,
	requestedTarget *wakeTarget,
) (bool, error) {
	if agentDir == nil {
		return false, fmt.Errorf("cooperative wake stop agent capability is missing")
	}
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
	request := i.Lock.Generation
	if requestedTarget != nil {
		var persistedDigest string
		if err := withExistingWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
			current := inspectWakeLockAt(dirfd, agentDir, i.Root, i.Agent)
			if !sameWakeLockGeneration(i, current) {
				return fmt.Errorf("wake generation changed before cooperative stop")
			}
			persisted, exists, err := readWakeTargetAt(
				dirfd, agentDir, i.Root, i.Agent,
			)
			if err != nil {
				return err
			}
			if !exists || !sameWakeInjectorIdentity(persisted, *requestedTarget) {
				return fmt.Errorf("requested wake target does not match wake lock")
			}
			persistedDigest, err = wakeTargetDigest(persisted)
			return err
		}); err != nil {
			return false, err
		}
		request += " " + persistedDigest
	}
	if _, err = fmt.Fprintf(conn, "%s\n", request); err != nil {
		return false, err
	}
	_ = conn.SetDeadline(time.Time{})
	line, err := bufio.NewReader(io.LimitReader(conn, wakeControlInjectViaACKMaxBytes+1)).ReadString('\n')
	if err != nil || len(line) > wakeControlInjectViaACKMaxBytes {
		return false, fmt.Errorf("cooperative wake stop refused")
	}
	residue, err := parseWakeControlResidueACK(line)
	if err != nil {
		return false, err
	}
	var postCommitErr error
	for _, cause := range residue {
		postCommitErr = errors.Join(postCommitErr, wakeControlResidueError(cause))
	}
	err = withExistingWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		cur := inspectWakeLockAt(dirfd, agentDir, i.Root, i.Agent)
		if cur.Exists {
			postCommitErr = errors.Join(postCommitErr, newWakeLockResidueError(
				wakeLockResidueReplacement,
				errors.New("wake lock appeared after committed cooperative retirement; preserving replacement artifacts"),
			))
		}
		return nil
	})
	return true, errors.Join(postCommitErr, err)
}

func validateRequestedWakeTargetDigestAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	requestedDigest string,
) error {
	if requestedDigest == "" {
		return nil
	}
	if requestedDigest != inspection.Lock.TargetDigest {
		return fmt.Errorf("requested wake target no longer matches wake lock")
	}
	target, exists, err := readWakeTargetAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
	)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("requested wake target is missing")
	}
	digest, err := wakeTargetDigest(target)
	if err != nil {
		return err
	}
	if digest != requestedDigest {
		return fmt.Errorf("requested wake target changed before cooperative stop")
	}
	return nil
}

func cooperativeStopAuthoritativeWakeInDir(
	agentDir *wakeAgentDir,
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
	if agentDir == nil {
		return false, fmt.Errorf("cooperative authoritative wake stop unavailable: wake agent directory capability is missing")
	}
	if err := withExistingWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
			return err
		}
		current := inspectWakeLockAt(dirfd, agentDir, i.Root, i.Agent)
		if !sameWakeLockGeneration(i, current) {
			return fmt.Errorf("authoritative wake generation changed before cooperative stop")
		}
		_, err = validateAuthoritativeWakeClaimPairAt(scope, current)
		return err
	}); err != nil {
		return false, fmt.Errorf("cooperative authoritative wake stop unavailable: %w", err)
	}
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
	err = withExistingWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, i.Root, i.Agent)
		gone = !current.Exists || current.Lock.Generation != i.Lock.Generation
		return nil
	})
	return gone, err
}
