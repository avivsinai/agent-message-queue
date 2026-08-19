package launch

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	LeaseVersion      = 1
	leaseFilename     = "lease.json"
	leaseLockFilename = "lease.lock"
	handleLockDir     = "handles"
)

type LeaseState string

const (
	LeaseMissing    LeaseState = "missing"
	LeaseValid      LeaseState = "valid"
	LeaseStale      LeaseState = "stale"
	LeaseUnverified LeaseState = "unverified"
)

// HolderIdentity is the wake-lock idiom: pid plus kernel start token and boot
// id. A lease is stale only when that holder is proven dead; unverified fails closed.
type HolderIdentity struct {
	PID          int    `json:"pid"`
	ProcessStart string `json:"process_start,omitempty"`
	BootID       string `json:"boot_id,omitempty"`
}

type leaseRecord struct {
	Version     int            `json:"version"`
	Holder      HolderIdentity `json:"holder"`
	LaunchNonce string         `json:"launch_nonce"`
}

// Lease is a live, process-local capability. Unexported fields keep a forged
// value from authorizing WriteBinding.
type Lease struct {
	root      *fsq.DeliveryRoot
	nonce     string
	holder    HolderIdentity
	secret    uint64
	handleIDs []string
	handles   []*os.File
	released  bool
}

func (l *Lease) LaunchNonce() string {
	if l == nil {
		return ""
	}
	return l.nonce
}

func (l *Lease) LockedHandles() []string {
	if l == nil {
		return nil
	}
	return slices.Clone(l.handleIDs)
}

func (l *Lease) holdsHandle(handle string) bool {
	return l != nil && slices.Contains(l.handleIDs, handle)
}

type LeaseHeldError struct {
	Nonce    string
	Evidence string
}

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("launch lease already held (nonce %s)", e.Nonce)
}

type LeaseUnverifiedError struct {
	Evidence string
}

func (e *LeaseUnverifiedError) Error() string {
	return fmt.Sprintf("launch lease holder is unverified: %s", e.Evidence)
}

type processInfo struct {
	PID          int
	Running      bool
	StartToken   string
	BootID       string
	InspectError error
}

var inspectProcess = inspectProcessPlatform

var liveLeaseSecrets sync.Map

// These seams keep failure handling testable without changing the filesystem
// capability contract. Production calls use the standard implementations.
var leaseRandomRead = rand.Read
var removeLeaseRecord = func(root *fsq.DeliveryRoot) error {
	return root.Remove(filepath.Join(bindingDirectory, leaseFilename))
}

type LeaseInspection struct {
	State    LeaseState
	Evidence string
	Holder   HolderIdentity
	Nonce    string
}

func LeasePath(sessionRoot string) string {
	return filepath.Join(sessionRoot, bindingDirectory, leaseFilename)
}

func AcquireLease(root *fsq.DeliveryRoot, nonce string) (*Lease, error) {
	if root == nil {
		return nil, fmt.Errorf("missing pinned session root")
	}
	holder, err := currentHolder()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(nonce) == "" {
		nonce, err = generateNonce()
		if err != nil {
			return nil, err
		}
	}
	var secret uint64
	if err := withLeaseInterlock(root, func() error {
		inspection, inspectErr := inspectLeaseLocked(root)
		if inspectErr != nil {
			return inspectErr
		}
		switch inspection.State {
		case LeaseValid:
			return &LeaseHeldError{Nonce: inspection.Nonce, Evidence: inspection.Evidence}
		case LeaseUnverified:
			return &LeaseUnverifiedError{Evidence: inspection.Evidence}
		case LeaseStale, LeaseMissing:
			secret, err = newSecret()
			if err != nil {
				return err
			}
			if _, loaded := liveLeaseSecrets.LoadOrStore(secret, struct{}{}); loaded {
				return fmt.Errorf("lease secret collision")
			}
			if err := writeLeaseRecord(root, leaseRecord{Version: LeaseVersion, Holder: holder, LaunchNonce: nonce}); err != nil {
				liveLeaseSecrets.Delete(secret)
				return err
			}
			return nil
		default:
			return fmt.Errorf("unknown lease state %q", inspection.State)
		}
	}); err != nil {
		return nil, err
	}
	lease := &Lease{root: root, nonce: nonce, holder: holder, secret: secret}
	return lease, nil
}

func InspectLease(root *fsq.DeliveryRoot) (LeaseInspection, error) {
	if root == nil {
		return LeaseInspection{}, fmt.Errorf("missing pinned session root")
	}
	var inspection LeaseInspection
	err := withLeaseInterlock(root, func() error {
		var inspectErr error
		inspection, inspectErr = inspectLeaseLocked(root)
		return inspectErr
	})
	return inspection, err
}

func (l *Lease) LockHandles(handles ...string) error {
	if err := l.authorizeLive(); err != nil {
		return err
	}
	if len(l.handles) != 0 {
		return fmt.Errorf("handle locks already held")
	}
	sorted := slices.Clone(handles)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	for _, handle := range sorted {
		if err := fsq.ValidateHandle(handle); err != nil {
			l.unlockHandles()
			return err
		}
		file, err := openAdvisoryLock(l.root, filepath.Join(bindingDirectory, handleLockDir, handle+".lock"))
		if err != nil {
			l.unlockHandles()
			return err
		}
		if err := lockExclusive(file); err != nil {
			_ = file.Close()
			l.unlockHandles()
			return err
		}
		l.handles = append(l.handles, file)
		l.handleIDs = append(l.handleIDs, handle)
	}
	return nil
}

func (l *Lease) Release() error {
	if l == nil {
		return fmt.Errorf("launch lease is not held")
	}
	l.unlockHandles()
	if l.released {
		return nil
	}
	err := withLeaseInterlock(l.root, func() error {
		inspection, inspectErr := inspectLeaseLocked(l.root)
		if inspectErr != nil {
			return inspectErr
		}
		if inspection.State != LeaseValid || inspection.Nonce != l.nonce {
			return fmt.Errorf("launch lease is no longer held by this process")
		}
		return removeLeaseRecord(l.root)
	})
	if err == nil {
		l.released = true
		liveLeaseSecrets.Delete(l.secret)
		l.secret = 0
	}
	return err
}

// abandonCapability drops only the process-local capability when its pinned
// root is no longer reachable through the authorized namespace. The caller
// owns any remaining filesystem diagnosis or cleanup.
func (l *Lease) abandonCapability() {
	if l == nil || l.released {
		return
	}
	l.unlockHandles()
	l.released = true
	liveLeaseSecrets.Delete(l.secret)
	l.secret = 0
}

func (l *Lease) authorizeWrite(root *fsq.DeliveryRoot) error {
	if err := l.authorizeLive(); err != nil {
		return err
	}
	if l.root != root {
		return fmt.Errorf("launch lease does not match the session root")
	}
	return withLeaseInterlock(root, func() error {
		inspection, err := inspectLeaseLocked(root)
		if err != nil {
			return err
		}
		if inspection.State != LeaseValid || inspection.Nonce != l.nonce {
			return fmt.Errorf("launch lease is not live")
		}
		return nil
	})
}

func (l *Lease) authorizeLive() error {
	if l == nil || l.released || l.secret == 0 || l.root == nil {
		return fmt.Errorf("launch lease is not held")
	}
	if _, ok := liveLeaseSecrets.Load(l.secret); !ok {
		return fmt.Errorf("launch lease is not held")
	}
	holder, err := currentHolder()
	if err != nil {
		return err
	}
	if holder.PID != l.holder.PID || holder.ProcessStart != l.holder.ProcessStart {
		return fmt.Errorf("launch lease holder identity no longer matches this process")
	}
	return nil
}

func (l *Lease) unlockHandles() {
	for i := len(l.handles) - 1; i >= 0; i-- {
		_ = unlockExclusive(l.handles[i])
		_ = l.handles[i].Close()
	}
	l.handles = nil
	l.handleIDs = nil
}

func inspectLeaseLocked(root *fsq.DeliveryRoot) (LeaseInspection, error) {
	file, info, err := root.OpenRegularNoFollow(filepath.Join(bindingDirectory, leaseFilename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LeaseInspection{State: LeaseMissing}, nil
		}
		return LeaseInspection{}, err
	}
	defer func() { _ = file.Close() }()
	if info.Mode().Perm() != 0o600 {
		return LeaseInspection{}, fmt.Errorf("lease permissions are %04o, want 0600", info.Mode().Perm())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return LeaseInspection{}, err
	}
	var record leaseRecord
	if err := decodeStrict(data, &record); err != nil {
		return LeaseInspection{State: LeaseUnverified, Evidence: "lease file is unreadable"}, nil
	}
	if record.Version != LeaseVersion || strings.TrimSpace(record.LaunchNonce) == "" || record.Holder.PID <= 0 {
		return LeaseInspection{State: LeaseUnverified, Evidence: "lease record is incomplete"}, nil
	}
	state, evidence := classifyHolder(record.Holder, inspectProcess(record.Holder.PID))
	return LeaseInspection{State: state, Evidence: evidence, Holder: record.Holder, Nonce: record.LaunchNonce}, nil
}

func classifyHolder(recorded HolderIdentity, live processInfo) (LeaseState, string) {
	if !live.Running {
		return LeaseStale, "holder pid is not running"
	}
	if recorded.ProcessStart != "" {
		if live.StartToken == "" {
			return LeaseUnverified, evidenceOr("process start time unavailable", live.InspectError)
		}
		if recorded.BootID != "" && live.BootID != "" && recorded.BootID != live.BootID {
			return LeaseStale, "boot id mismatch"
		}
		if recorded.BootID != "" && live.BootID == "" {
			return LeaseUnverified, evidenceOr("boot id unavailable", live.InspectError)
		}
		if recorded.ProcessStart != live.StartToken {
			if recorded.BootID == "" {
				return LeaseUnverified, "process start time mismatch"
			}
			return LeaseStale, "process start time mismatch"
		}
		return LeaseValid, "holder identity confirmed"
	}
	return LeaseUnverified, "lease lacks process start metadata"
}

func currentHolder() (HolderIdentity, error) {
	proc := inspectProcess(os.Getpid())
	if !proc.Running {
		return HolderIdentity{}, fmt.Errorf("current process is not running")
	}
	if proc.StartToken == "" {
		return HolderIdentity{}, fmt.Errorf("process start time unavailable")
	}
	return HolderIdentity{PID: proc.PID, ProcessStart: proc.StartToken, BootID: proc.BootID}, nil
}

func writeLeaseRecord(root *fsq.DeliveryRoot, record leaseRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = root.WriteFileAtomic(bindingDirectory, leaseFilename, data, 0o600)
	return err
}

func withLeaseInterlock(root *fsq.DeliveryRoot, fn func() error) error {
	file, err := openAdvisoryLock(root, filepath.Join(bindingDirectory, leaseLockFilename))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := lockExclusive(file); err != nil {
		return err
	}
	defer func() { _ = unlockExclusive(file) }()
	return fn()
}

func openAdvisoryLock(root *fsq.DeliveryRoot, rel string) (*os.File, error) {
	dir, name := filepath.Split(rel)
	file, err := root.OpenLockFile(strings.TrimSuffix(dir, string(filepath.Separator)), name, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Mode().Perm() != 0o600 {
		_ = file.Close()
		return nil, fmt.Errorf("lock permissions are %04o, want 0600", info.Mode().Perm())
	}
	return file, nil
}

func generateNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

func newSecret() (uint64, error) {
	var buf [8]byte
	n, err := leaseRandomRead(buf[:])
	if err != nil {
		return 0, fmt.Errorf("generate lease secret: %w", err)
	}
	if n != len(buf) {
		return 0, fmt.Errorf("generate lease secret: short random read: got %d bytes, want %d", n, len(buf))
	}
	secret := binary.BigEndian.Uint64(buf[:])
	if secret == 0 {
		return 0, fmt.Errorf("generate lease secret: zero value")
	}
	if _, loaded := liveLeaseSecrets.Load(secret); loaded {
		return 0, fmt.Errorf("generate lease secret: collision")
	}
	return secret, nil
}

func evidenceOr(base string, err error) string {
	if err == nil {
		return base
	}
	return fmt.Sprintf("%s: %v", base, err)
}
