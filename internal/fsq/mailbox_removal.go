package fsq

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// mailboxRemovalPrefix marks agents/<entry> directories that a mailbox
// removal moved aside. The leading dot keeps them outside the handle grammar,
// so readers of agents/ must skip them instead of treating them as handles.
const mailboxRemovalPrefix = ".removing-"

const mailboxRemovalNonceBytes = 12

// NewMailboxRemovalName returns a fresh agents/ entry name for moving the
// mailbox of handle aside before deletion.
func NewMailboxRemovalName(handle string) (string, error) {
	if err := ValidateHandle(handle); err != nil {
		return "", err
	}
	var nonce [mailboxRemovalNonceBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate mailbox removal name: %w", err)
	}
	return mailboxRemovalPrefix + handle + "-" + hex.EncodeToString(nonce[:]), nil
}

// IsMailboxRemovalName reports whether an agents/ entry is a mailbox moved
// aside by a removal, complete or not.
func IsMailboxRemovalName(name string) bool {
	return strings.HasPrefix(name, mailboxRemovalPrefix)
}

// ParseMailboxRemovalName returns the handle whose mailbox name was moved
// aside, or false when name is not an exact removal name.
func ParseMailboxRemovalName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, mailboxRemovalPrefix)
	if !ok {
		return "", false
	}
	cut := strings.LastIndexByte(rest, '-')
	if cut < 0 {
		return "", false
	}
	handle, nonce := rest[:cut], rest[cut+1:]
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
