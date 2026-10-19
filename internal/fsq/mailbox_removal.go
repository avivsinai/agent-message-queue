package fsq

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// MailboxRemovalDir is the root-relative directory where a mailbox removal
// stages agents/<handle> before deletion. It sits outside agents/ so no
// mailbox reader ever sees a staged tree.
var MailboxRemovalDir = filepath.Join("meta", "removing")

const (
	mailboxRemovalLockFile   = "removing.lock"
	mailboxRemovalNonceBytes = 12
)

// NewMailboxRemovalName returns a fresh name under MailboxRemovalDir for
// staging the mailbox of handle.
func NewMailboxRemovalName(handle string) (string, error) {
	if err := ValidateHandle(handle); err != nil {
		return "", err
	}
	var nonce [mailboxRemovalNonceBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate mailbox removal name: %w", err)
	}
	return handle + "-" + hex.EncodeToString(nonce[:]), nil
}

// ParseMailboxRemovalName returns the handle whose mailbox a staged name
// holds, or false when name is not an exact staging name.
func ParseMailboxRemovalName(name string) (string, bool) {
	cut := strings.LastIndexByte(name, '-')
	if cut < 0 {
		return "", false
	}
	handle, nonce := name[:cut], name[cut+1:]
	if len(nonce) != 2*mailboxRemovalNonceBytes || strings.ToLower(nonce) != nonce {
		return "", false
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		return "", false
	}
	if ValidateHandle(handle) != nil {
		return "", false
	}
	return handle, true
}

// ErrLockBusy reports that another holder has an exclusive file lock.
var ErrLockBusy = errors.New("lock is held by another process")

// WithMailboxRemovalLock runs fn while holding the root's exclusive mailbox
// removal lock, without waiting: it returns ErrLockBusy when another run
// holds it. With the lock held, every entry under MailboxRemovalDir is
// left by an interrupted run, never by a live one. The lock file is retained;
// the kernel releases the lock on close or process crash.
func (r *DeliveryRoot) WithMailboxRemovalLock(fn func() error) error {
	if fn == nil {
		return fmt.Errorf("mailbox removal lock callback is nil")
	}
	lockFile, err := r.OpenLockFile("meta", mailboxRemovalLockFile, 0o600)
	if err != nil {
		return fmt.Errorf("open mailbox removal lock: %w", err)
	}
	defer func() { _ = lockFile.Close() }()
	return tryExclusiveFileLock(lockFile, func() error {
		if err := r.VerifyBase(); err != nil {
			return err
		}
		return fn()
	})
}
