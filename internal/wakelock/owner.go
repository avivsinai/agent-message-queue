package wakelock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxOwnerIdentityTokenBytes = 256

// Owner is the exact process identity of a wake owner.
type Owner struct {
	PID          int    `json:"pid"`
	ProcessStart string `json:"process_start,omitempty"`
	BootID       string `json:"boot_id,omitempty"`
	SessionID    int    `json:"session_id,omitempty"`
}

// ValidateOwner checks the fields every recorded owner must satisfy.
func ValidateOwner(owner Owner) error {
	if owner.PID <= 0 {
		return fmt.Errorf("wake owner pid must be > 0")
	}
	if strings.ContainsRune(owner.ProcessStart, 0) {
		return fmt.Errorf("wake owner process start contains NUL")
	}
	if strings.ContainsRune(owner.BootID, 0) {
		return fmt.Errorf("wake owner boot id contains NUL")
	}
	if owner.SessionID < 0 {
		return fmt.Errorf("wake owner session id must be >= 0")
	}
	return nil
}

// ValidateAuthoritativeOwner additionally requires the canonical process
// start, boot id, and session that bind an owner-bound wake.
func ValidateAuthoritativeOwner(owner Owner) error {
	if err := ValidateOwner(owner); err != nil {
		return err
	}
	if err := validateAuthoritativeOwnerToken("process start", owner.ProcessStart); err != nil {
		return err
	}
	if err := validateAuthoritativeOwnerToken("boot id", owner.BootID); err != nil {
		return err
	}
	if owner.SessionID <= 0 {
		return fmt.Errorf("wake owner session id must be > 0")
	}
	if !validOwnerProcessStart(owner.ProcessStart) {
		return fmt.Errorf("wake owner process start has malformed platform value")
	}
	if !validOwnerBootID(owner.BootID) {
		return fmt.Errorf("wake owner boot id has malformed platform value")
	}
	return nil
}

func validateAuthoritativeOwnerToken(label, value string) error {
	if value == "" {
		return fmt.Errorf("wake owner %s is required", label)
	}
	if len(value) > maxOwnerIdentityTokenBytes {
		return fmt.Errorf("wake owner %s is too long", label)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("wake owner %s is not valid UTF-8", label)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("wake owner %s contains a control character", label)
		}
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("wake owner %s is not canonical", label)
	}
	return nil
}

func validOwnerProcessStart(value string) bool {
	if validPositiveDecimal(value) {
		return true
	}
	return validOwnerTimestamp(value)
}

func validOwnerBootID(value string) bool {
	if validOwnerTimestamp(value) {
		return true
	}
	if len(value) != 36 {
		return false
	}
	for index, r := range value {
		switch index {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') &&
				(r < 'a' || r > 'f') &&
				(r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}

func validOwnerTimestamp(value string) bool {
	seconds, nanos, found := strings.Cut(value, ".")
	if !found || !validPositiveDecimal(seconds) || len(nanos) != 9 {
		return false
	}
	parsedNanos, err := strconv.ParseUint(nanos, 10, 32)
	return err == nil && parsedNanos < 1_000_000_000
}

func validPositiveDecimal(value string) bool {
	if value == "" {
		return false
	}
	if len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0
}

// ValidatePathOwnership refuses a file that group or others can write or that
// another user owns.
func ValidatePathOwnership(label, path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s %s is group/world-writable", label, path)
	}
	ownerUID, ownerOK := FileOwnerUID(info)
	currentUID, currentOK := CurrentUID()
	if ownerOK && currentOK && ownerUID != currentUID {
		return fmt.Errorf("%s %s is owned by uid %d, want current uid %d", label, path, ownerUID, currentUID)
	}
	return nil
}

// ValidStateDigest reports whether value is a lowercase sha256: digest.
func ValidStateDigest(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return false
	}
	encoded := value[len(prefix):]
	if encoded != strings.ToLower(encoded) {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size
}

// ValidStateGeneration reports whether value is a lowercase 16-byte hex nonce.
func ValidStateGeneration(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}
