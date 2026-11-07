package format

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// idStampLayout is the UTC creation stamp that starts every message id,
// before its first underscore.
const idStampLayout = "2006-01-02T15-04-05.000Z"

func NewMessageID(now time.Time) (string, error) {
	stamp := now.UTC().Format(idStampLayout)
	pid := os.Getpid()
	suffix, err := randSuffix(4)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_pid%d_%s", stamp, pid, suffix), nil
}

func randSuffix(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(hex.EncodeToString(buf)), nil
}

// IDTime is the creation time stamped at the start of a message id, at
// millisecond precision. It works on a message file name too. ok is false
// when the id does not start with a stamp.
func IDTime(id string) (time.Time, bool) {
	stamp, _, found := strings.Cut(id, "_")
	if !found {
		return time.Time{}, false
	}
	at, err := time.Parse(idStampLayout, stamp)
	return at, err == nil
}
