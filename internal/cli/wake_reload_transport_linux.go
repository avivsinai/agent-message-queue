//go:build linux

package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

const (
	linuxWakeReloadDefaultHandlerTimeout = 2 * time.Second
	linuxWakeReloadMaxHandlerTimeout     = 5 * time.Second
	linuxWakeReloadMaxHandlers           = 4
	linuxWakeReloadListenBacklog         = 8
	linuxWakeReloadMaxReceivedFDs        = 16
)

var (
	closeWakeReloadReceivedFD  = unix.Close
	linuxWakeReloadGetPeerSID  = getWakeProcessSID
	linuxWakeReloadPeerProcess = func(pid int) wakeProcessInfo {
		return inspectWakeProcess(pid)
	}
	linuxWakeReloadGetPeerCred   = getLinuxWakeReloadPeerCred
	linuxWakeReloadBeforePublish func(dirfd int, stagingName, publicName string) error
	linuxWakeReloadAfterRetire   func(dirfd int, retiredName, publicName string) error
	linuxWakeReloadBeforeGuard   func()
)

type linuxWakeReloadSocketIdentity struct {
	device    uint64
	inode     uint64
	ctimeSec  int64
	ctimeNsec int64
	mode      uint32
	uid       uint32
}

type linuxWakeReloadTransport struct {
	agentDir       *wakeAgentDir
	root           string
	agent          string
	expected       wakeLockInspection
	owner          wakeOwner
	path           string
	socketName     string
	socketIdentity linuxWakeReloadSocketIdentity
	handlerTimeout time.Duration
	listener       *net.UnixListener
	handlerSlots   chan struct{}
	handlers       sync.WaitGroup
	acceptDone     chan struct{}
	closeOnce      sync.Once
	closeErr       error
}

// linuxWakeReloadTransportPath is deliberately not wakeControlSocketPath.
// Sharing that advertised endpoint would publish resume schema 2 on Linux
// using pathname-reopened evidence mislabeled as fd_exec before execution-bound
// image authority exists. This listener remains an unadvertised refusal-only
// seam until that authority is available.
func linuxWakeReloadTransportPath(root, agent, generation string) string {
	sum := sha256.Sum256([]byte(canonicalWakeRoot(root) + "\x00" + agent + "\x00" + generation))
	return filepath.Join(fsq.AgentBase(root, agent), ".wr."+hex.EncodeToString(sum[:8]))
}

func startWakeReloadTransportInDir(
	agentDir *wakeAgentDir,
	root string,
	agent string,
	expected wakeLockInspection,
	owner wakeOwner,
) (func(), error) {
	transport, err := startLinuxWakeReloadTransport(
		agentDir,
		root,
		agent,
		expected,
		owner,
		linuxWakeReloadDefaultHandlerTimeout,
	)
	if err != nil {
		return nil, err
	}
	return func() { _ = transport.Close() }, nil
}

func startLinuxWakeReloadTransport(
	agentDir *wakeAgentDir,
	root string,
	agent string,
	expected wakeLockInspection,
	owner wakeOwner,
	handlerTimeout time.Duration,
) (*linuxWakeReloadTransport, error) {
	root = canonicalWakeRoot(root)
	if err := validateLinuxWakeReloadTransportStart(
		agentDir,
		root,
		agent,
		expected,
		owner,
		handlerTimeout,
	); err != nil {
		return nil, err
	}

	var current wakeLockInspection
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current = inspectWakeLockAt(dirfd, agentDir, root, agent)
		return validateLinuxWakeReloadCurrentLock(expected, current, root, agent, owner)
	}); err != nil {
		return nil, err
	}
	if err := validateLinuxWakeReloadOwnerLive(owner); err != nil {
		return nil, err
	}

	path := linuxWakeReloadTransportPath(root, agent, expected.Lock.Generation)
	socketName := filepath.Base(path)
	if filepath.Dir(path) != filepath.Clean(agentDir.path) ||
		!strings.HasPrefix(socketName, ".wr.") || strings.ContainsRune(socketName, '/') {
		return nil, fmt.Errorf("wake reload endpoint escaped retained agent directory")
	}
	listener, identity, err := listenLinuxWakeReloadTransportAt(agentDir, socketName, path)
	if err != nil {
		return nil, &wakeReloadTransportUnavailableError{err: err}
	}
	transport := &linuxWakeReloadTransport{
		agentDir:       agentDir,
		root:           root,
		agent:          agent,
		expected:       current,
		owner:          owner,
		path:           path,
		socketName:     socketName,
		socketIdentity: identity,
		handlerTimeout: handlerTimeout,
		listener:       listener,
		handlerSlots:   make(chan struct{}, linuxWakeReloadMaxHandlers),
		acceptDone:     make(chan struct{}),
	}
	go transport.acceptLoop()
	return transport, nil
}

func validateLinuxWakeReloadTransportStart(
	agentDir *wakeAgentDir,
	root string,
	agent string,
	expected wakeLockInspection,
	owner wakeOwner,
	handlerTimeout time.Duration,
) error {
	if agentDir == nil {
		return fmt.Errorf("wake reload agent directory capability is missing")
	}
	if root == "" || !filepath.IsAbs(root) || canonicalWakeRoot(root) != root {
		return fmt.Errorf("wake reload root is not canonical")
	}
	if err := fsq.ValidateHandle(agent); err != nil {
		return fmt.Errorf("wake reload agent is invalid: %w", err)
	}
	if handlerTimeout <= 0 || handlerTimeout > linuxWakeReloadMaxHandlerTimeout {
		return fmt.Errorf("wake reload handler timeout is outside the bounded range")
	}
	return validateLinuxWakeReloadIdentity(root, agent, expected, owner)
}

func validateLinuxWakeReloadIdentity(
	root string,
	agent string,
	expected wakeLockInspection,
	owner wakeOwner,
) error {
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		return fmt.Errorf("wake reload owner is invalid: %w", err)
	}
	if !expected.Exists || expected.Status != wakeLockValid || !expected.IdentityConfirmed {
		return fmt.Errorf("wake reload lock identity is not confirmed")
	}
	if expected.Root != root || expected.Agent != agent ||
		expected.Lock.Root != root || expected.Lock.Agent != agent {
		return fmt.Errorf("wake reload lock scope does not match")
	}
	if !validWakeReloadTransportGeneration(expected.Lock.Generation) {
		return fmt.Errorf("wake reload generation is invalid")
	}
	if expected.PID != os.Getpid() || expected.Lock.PID != os.Getpid() {
		return fmt.Errorf("wake reload lock is not owned by this process")
	}
	if expected.Lock.ProcessStart != expected.Process.StartToken ||
		compareWakeBootID(expected.Lock.BootID, expected.Process) != bootIDMatch {
		return fmt.Errorf("wake reload lock process identity does not match the running wake")
	}
	if expected.Lock.Owner != nil && !sameWakeOwner(expected.Lock.Owner, &owner) {
		return fmt.Errorf("wake reload persisted owner does not match")
	}
	if expected.Lock.ResumeOwner != nil && !sameWakeOwner(expected.Lock.ResumeOwner, &owner) {
		return fmt.Errorf("wake reload persisted resume owner does not match")
	}
	return nil
}

func validateLinuxWakeReloadCurrentLock(
	expected wakeLockInspection,
	current wakeLockInspection,
	root string,
	agent string,
	owner wakeOwner,
) error {
	if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed ||
		current.Status != wakeLockValid {
		return fmt.Errorf("wake reload lock changed")
	}
	return validateLinuxWakeReloadIdentity(root, agent, current, owner)
}

func validateLinuxWakeReloadOwnerLive(owner wakeOwner) (retErr error) {
	observation, err := observeAuthoritativeWakeOwner(owner)
	defer func() {
		retErr = errors.Join(retErr, observation.Close())
	}()
	if err != nil {
		return err
	}
	if observation.State != wakeOwnerSame {
		return fmt.Errorf("wake reload owner identity is %s: %s", observation.State, observation.Reason)
	}
	select {
	case <-observation.Done():
		return fmt.Errorf("wake reload owner exited during authentication")
	default:
		return nil
	}
}

func listenLinuxWakeReloadTransportAt(
	agentDir *wakeAgentDir,
	name string,
	path string,
) (*net.UnixListener, linuxWakeReloadSocketIdentity, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, linuxWakeReloadSocketIdentity{}, err
	}
	ownedFD := true
	defer func() {
		if ownedFD {
			_ = unix.Close(fd)
		}
	}()

	var identity linuxWakeReloadSocketIdentity
	err = agentDir.withFD(func(dirfd int) error {
		stagingName, err := randomLinuxWakeReloadPrivateName("stage")
		if err != nil {
			return err
		}
		procPath := linuxWakeReloadProcFDPath(dirfd, stagingName)
		if err := unix.Bind(fd, &unix.SockaddrUnix{Name: procPath}); err != nil {
			return fmt.Errorf("bind wake reload endpoint %s: %w", path, err)
		}
		if err := unix.Fchmodat(dirfd, stagingName, 0o600, 0); err != nil {
			return fmt.Errorf("chmod wake reload endpoint %s: %w", path, err)
		}
		stagedIdentity, err := inspectLinuxWakeReloadSocketAt(dirfd, stagingName, path)
		if err != nil {
			return err
		}
		cleanupName := stagingName
		complete := false
		defer func() {
			if !complete {
				_ = removeLinuxWakeReloadSocketIfSameAt(dirfd, agentDir.path, cleanupName, stagedIdentity)
			}
		}()
		if err := unix.Listen(fd, linuxWakeReloadListenBacklog); err != nil {
			return fmt.Errorf("listen on wake reload endpoint %s: %w", path, err)
		}
		if linuxWakeReloadBeforePublish != nil {
			if err := linuxWakeReloadBeforePublish(dirfd, stagingName, name); err != nil {
				return err
			}
		}
		if err := unix.Renameat2(dirfd, stagingName, dirfd, name, unix.RENAME_NOREPLACE); err != nil {
			return fmt.Errorf("publish wake reload endpoint %s: %w", path, err)
		}
		cleanupName = name
		captured, err := inspectLinuxWakeReloadSocketAt(dirfd, name, path)
		if err != nil {
			return err
		}
		if !sameLinuxWakeReloadStableSocketIdentity(stagedIdentity, captured) {
			return fmt.Errorf("wake reload endpoint identity changed during publication")
		}
		identity = captured
		complete = true
		return nil
	})
	if err != nil {
		if identity != (linuxWakeReloadSocketIdentity{}) {
			_ = removeLinuxWakeReloadSocketIfSame(agentDir, name, identity)
		}
		return nil, linuxWakeReloadSocketIdentity{}, err
	}

	file := os.NewFile(uintptr(fd), "wake-reload-listener")
	listenerAny, err := net.FileListener(file)
	_ = file.Close()
	ownedFD = false
	if err != nil {
		_ = removeLinuxWakeReloadSocketIfSame(agentDir, name, identity)
		return nil, linuxWakeReloadSocketIdentity{}, err
	}
	listener, ok := listenerAny.(*net.UnixListener)
	if !ok {
		_ = listenerAny.Close()
		_ = removeLinuxWakeReloadSocketIfSame(agentDir, name, identity)
		return nil, linuxWakeReloadSocketIdentity{}, fmt.Errorf("wake reload listener is not unix")
	}
	listener.SetUnlinkOnClose(false)
	return listener, identity, nil
}

func randomLinuxWakeReloadPrivateName(purpose string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate wake reload %s name: %w", purpose, err)
	}
	return ".wr." + purpose + "." + hex.EncodeToString(nonce[:]), nil
}

func sameLinuxWakeReloadStableSocketIdentity(
	first linuxWakeReloadSocketIdentity,
	second linuxWakeReloadSocketIdentity,
) bool {
	return first.device == second.device && first.inode == second.inode &&
		first.mode == second.mode && first.uid == second.uid
}

func linuxWakeReloadProcFDPath(dirfd int, name string) string {
	return fmt.Sprintf("/proc/self/fd/%d/%s", dirfd, name)
}

func inspectLinuxWakeReloadSocketAt(
	dirfd int,
	name string,
	path string,
) (linuxWakeReloadSocketIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return linuxWakeReloadSocketIdentity{}, fmt.Errorf("stat wake reload endpoint %s: %w", path, err)
	}
	identity := linuxWakeReloadSocketIdentity{
		device:    uint64(stat.Dev),
		inode:     stat.Ino,
		ctimeSec:  stat.Ctim.Sec,
		ctimeNsec: stat.Ctim.Nsec,
		mode:      stat.Mode,
		uid:       stat.Uid,
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return linuxWakeReloadSocketIdentity{}, fmt.Errorf("wake reload endpoint %s is not a socket", path)
	}
	if stat.Mode&0o777 != 0o600 {
		return linuxWakeReloadSocketIdentity{}, fmt.Errorf(
			"wake reload endpoint %s mode is %o, want 0600",
			path,
			stat.Mode&0o777,
		)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return linuxWakeReloadSocketIdentity{}, fmt.Errorf(
			"wake reload endpoint %s is owned by uid %d, want %d",
			path,
			stat.Uid,
			os.Geteuid(),
		)
	}
	return identity, nil
}

func removeLinuxWakeReloadSocketIfSame(
	agentDir *wakeAgentDir,
	name string,
	expected linuxWakeReloadSocketIdentity,
) error {
	return agentDir.withFD(func(dirfd int) error {
		return removeLinuxWakeReloadSocketIfSameAt(dirfd, agentDir.path, name, expected)
	})
}

func removeLinuxWakeReloadSocketIfSameAt(
	dirfd int,
	dirPath string,
	name string,
	expected linuxWakeReloadSocketIdentity,
) error {
	retiredName, err := randomLinuxWakeReloadPrivateName("retired")
	if err != nil {
		return err
	}
	if err := unix.Renameat2(dirfd, name, dirfd, retiredName, unix.RENAME_NOREPLACE); errors.Is(err, syscall.ENOENT) {
		return nil
	} else if err != nil {
		return err
	}
	if linuxWakeReloadAfterRetire != nil {
		if err := linuxWakeReloadAfterRetire(dirfd, retiredName, name); err != nil {
			return err
		}
	}
	retired, inspectErr := inspectLinuxWakeReloadSocketAt(
		dirfd,
		retiredName,
		filepath.Join(dirPath, retiredName),
	)
	if inspectErr == nil && sameLinuxWakeReloadStableSocketIdentity(retired, expected) {
		if err := unix.Unlinkat(dirfd, retiredName, 0); err != nil && !errors.Is(err, syscall.ENOENT) {
			return err
		}
		return nil
	}
	// A non-matching retired object is never unlinked. Restore it only if
	// no successor claimed the public name; otherwise preserve both names.
	restoreErr := unix.Renameat2(dirfd, retiredName, dirfd, name, unix.RENAME_NOREPLACE)
	identityErr := fmt.Errorf("wake reload endpoint identity changed; refused cleanup")
	if restoreErr == nil {
		return errors.Join(inspectErr, identityErr)
	}
	if errors.Is(restoreErr, syscall.EEXIST) {
		return errors.Join(
			inspectErr,
			identityErr,
			fmt.Errorf("preserved retired endpoint %s because public name %s is occupied", retiredName, name),
		)
	}
	return errors.Join(inspectErr, identityErr, restoreErr)
}

func (transport *linuxWakeReloadTransport) Close() error {
	if transport == nil {
		return nil
	}
	transport.closeOnce.Do(func() {
		listenerErr := transport.listener.Close()
		if errors.Is(listenerErr, net.ErrClosed) {
			listenerErr = nil
		}
		<-transport.acceptDone
		transport.handlers.Wait()
		removeErr := removeLinuxWakeReloadSocketIfSame(
			transport.agentDir,
			transport.socketName,
			transport.socketIdentity,
		)
		transport.closeErr = errors.Join(listenerErr, removeErr)
	})
	return transport.closeErr
}

func (transport *linuxWakeReloadTransport) acceptLoop() {
	defer close(transport.acceptDone)
	for {
		conn, err := transport.listener.AcceptUnix()
		if err != nil {
			return
		}
		select {
		case transport.handlerSlots <- struct{}{}:
			transport.handlers.Add(1)
			go transport.handle(conn)
		default:
			_ = conn.Close()
		}
	}
}

func (transport *linuxWakeReloadTransport) handle(conn *net.UnixConn) {
	defer transport.handlers.Done()
	defer func() { <-transport.handlerSlots }()
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(transport.handlerTimeout))

	peer, err := captureLinuxWakeReloadPeer(conn)
	if err != nil {
		return
	}
	defer func() { _ = peer.Close() }()
	if peer.uid != uint32(os.Geteuid()) || peer.owner.SessionID != transport.owner.SessionID {
		return
	}
	if err := transport.validateSocketIdentity(); err != nil {
		return
	}
	payload, ancillary, err := readLinuxWakeReloadTransportRequest(conn)
	if err != nil || ancillary {
		return
	}
	request, err := decodeWakeReloadTransportRequest(payload)
	if err != nil {
		return
	}
	if err := peer.Revalidate(); err != nil {
		return
	}
	if err := transport.authorize(request); err != nil {
		return
	}
	if err := peer.Revalidate(); err != nil {
		return
	}
	response, err := encodeWakeReloadTransportResponse(wakeReloadTransportUnavailableResponse())
	if err != nil {
		return
	}
	_, _ = conn.Write(response)
}

func (transport *linuxWakeReloadTransport) validateSocketIdentity() error {
	return transport.agentDir.withFD(func(dirfd int) error {
		current, err := inspectLinuxWakeReloadSocketAt(
			dirfd,
			transport.socketName,
			transport.path,
		)
		if err != nil {
			return err
		}
		if current != transport.socketIdentity {
			return fmt.Errorf("wake reload endpoint identity changed")
		}
		return nil
	})
}

func (transport *linuxWakeReloadTransport) authorize(request wakeReloadTransportRequest) error {
	if request.Root != transport.root || request.Agent != transport.agent ||
		request.Generation != transport.expected.Lock.Generation || request.Owner != transport.owner {
		return fmt.Errorf("wake reload request identity does not match")
	}
	if err := transport.validateSocketIdentity(); err != nil {
		return err
	}
	observation, observeErr := observeAuthoritativeWakeOwner(transport.owner)
	if observeErr != nil {
		return errors.Join(observeErr, observation.Close())
	}
	if observation.State != wakeOwnerSame {
		return errors.Join(
			fmt.Errorf("wake reload owner identity is %s: %s", observation.State, observation.Reason),
			observation.Close(),
		)
	}
	if linuxWakeReloadBeforeGuard != nil {
		linuxWakeReloadBeforeGuard()
	}
	authErr := withWakeLifecycleGuardNoWaitInDir(transport.agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, transport.agentDir, transport.root, transport.agent)
		return validateLinuxWakeReloadCurrentLock(
			transport.expected,
			current,
			transport.root,
			transport.agent,
			transport.owner,
		)
	})
	if authErr == nil {
		select {
		case <-observation.Done():
			authErr = fmt.Errorf("wake reload owner exited during authentication")
		default:
		}
	}
	return errors.Join(authErr, observation.Close())
}

type linuxWakeReloadPeer struct {
	pidfd int
	uid   uint32
	owner wakeOwner
}

func captureLinuxWakeReloadPeer(conn *net.UnixConn) (*linuxWakeReloadPeer, error) {
	cred, err := linuxWakeReloadGetPeerCred(conn)
	if err != nil {
		return nil, err
	}
	if cred.Pid <= 0 || cred.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("wake reload peer credentials are not authorized")
	}
	pid := int(cred.Pid)
	pidfd, err := linuxPidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("open wake reload peer pidfd: %w", err)
	}
	peer := &linuxWakeReloadPeer{pidfd: pidfd, uid: cred.Uid}
	if err := peer.captureStableOwner(pid); err != nil {
		_ = peer.Close()
		return nil, err
	}
	return peer, nil
}

func (peer *linuxWakeReloadPeer) captureStableOwner(pid int) error {
	exited, err := linuxPidfdPoll(peer.pidfd, 0)
	if err != nil || exited {
		return fmt.Errorf("wake reload peer is not live: %w", err)
	}
	first := linuxWakeReloadPeerProcess(pid)
	firstSID, firstSIDErr := linuxWakeReloadGetPeerSID(pid)
	owner := wakeOwner{
		PID:          pid,
		ProcessStart: first.StartToken,
		BootID:       first.BootID,
		SessionID:    firstSID,
	}
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		return fmt.Errorf("wake reload peer identity is incomplete: %w", err)
	}
	second := linuxWakeReloadPeerProcess(pid)
	secondSID, secondSIDErr := linuxWakeReloadGetPeerSID(pid)
	exited, err = linuxPidfdPoll(peer.pidfd, 0)
	if err != nil || exited {
		return fmt.Errorf("wake reload peer changed during inspection: %w", err)
	}
	state, reason := classifyStableAuthoritativeWakeOwner(
		owner,
		first,
		firstSID,
		firstSIDErr,
		second,
		secondSID,
		secondSIDErr,
	)
	if state != wakeOwnerSame {
		return fmt.Errorf("wake reload peer identity is %s: %s", state, reason)
	}
	peer.owner = owner
	return nil
}

func (peer *linuxWakeReloadPeer) Revalidate() error {
	if peer == nil || peer.pidfd < 0 {
		return fmt.Errorf("wake reload peer capability is missing")
	}
	exited, err := linuxPidfdPoll(peer.pidfd, 0)
	if err != nil || exited {
		return fmt.Errorf("wake reload peer is not live: %w", err)
	}
	first := linuxWakeReloadPeerProcess(peer.owner.PID)
	firstSID, firstSIDErr := linuxWakeReloadGetPeerSID(peer.owner.PID)
	second := linuxWakeReloadPeerProcess(peer.owner.PID)
	secondSID, secondSIDErr := linuxWakeReloadGetPeerSID(peer.owner.PID)
	state, reason := classifyStableAuthoritativeWakeOwner(
		peer.owner,
		first,
		firstSID,
		firstSIDErr,
		second,
		secondSID,
		secondSIDErr,
	)
	if state != wakeOwnerSame {
		return fmt.Errorf("wake reload peer changed: %s", reason)
	}
	exited, err = linuxPidfdPoll(peer.pidfd, 0)
	if err != nil || exited {
		return fmt.Errorf("wake reload peer changed after inspection: %w", err)
	}
	return nil
}

func (peer *linuxWakeReloadPeer) Close() error {
	if peer == nil || peer.pidfd < 0 {
		return nil
	}
	fd := peer.pidfd
	peer.pidfd = -1
	return linuxPidfdClose(fd)
}

func getLinuxWakeReloadPeerCred(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	if sockErr != nil {
		return nil, sockErr
	}
	return cred, nil
}

func readLinuxWakeReloadTransportRequest(conn *net.UnixConn) ([]byte, bool, error) {
	var payload bytes.Buffer
	ancillary := false
	buffer := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(4*linuxWakeReloadMaxReceivedFDs))
	for {
		n, oobn, flags, _, err := conn.ReadMsgUnix(buffer, oob)
		if oobn > 0 {
			hadAncillary, closeErr := closeLinuxWakeReloadAncillary(oob[:oobn])
			ancillary = ancillary || hadAncillary
			if closeErr != nil {
				return nil, true, closeErr
			}
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			return nil, true, fmt.Errorf("wake reload request was truncated")
		}
		if n > 0 {
			if payload.Len()+n > wakeReloadTransportMaxRequestBytes {
				return nil, ancillary, fmt.Errorf("wake reload request exceeds size bound")
			}
			_, _ = payload.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ancillary, err
		}
		if n == 0 && oobn == 0 {
			return nil, ancillary, io.ErrNoProgress
		}
	}
	return payload.Bytes(), ancillary, nil
}

func closeLinuxWakeReloadAncillary(oob []byte) (bool, error) {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return true, err
	}
	var closeErr error
	for _, message := range messages {
		if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SCM_RIGHTS {
			closeErr = errors.Join(closeErr, fmt.Errorf("wake reload ancillary data is unsupported"))
			continue
		}
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			closeErr = errors.Join(closeErr, err)
			continue
		}
		for _, fd := range fds {
			closeErr = errors.Join(closeErr, closeWakeReloadReceivedFD(fd))
		}
	}
	return len(messages) > 0, closeErr
}
