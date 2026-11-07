package format

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func NewMessageID(now time.Time) (string, error) {
	stamp := now.UTC().Format(fsq.IDStampLayout)
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

// IDTime is the creation time stamped at the start of a message id; see
// fsq.IDTime.
func IDTime(id string) (time.Time, bool) { return fsq.IDTime(id) }
