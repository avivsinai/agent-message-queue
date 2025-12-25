package format

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

func NewMessageID(now time.Time) (string, error) {
	stamp := now.UTC().Format("2006-01-02T15-04-05.000Z")
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
