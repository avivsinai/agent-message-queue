package fsq

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// IDStampLayout is the UTC creation stamp that starts every message id,
// before its first underscore, and so every message file name.
const IDStampLayout = "2006-01-02T15-04-05.000Z"

// IDTime is the creation time stamped at the start of a message id or
// message file name, at millisecond precision. ok is false when the name
// does not start with a stamp.
func IDTime(id string) (time.Time, bool) {
	stamp, _, found := strings.Cut(id, "_")
	if !found {
		return time.Time{}, false
	}
	at, err := time.Parse(IDStampLayout, stamp)
	return at, err == nil
}

// ValidateMessageFilename validates an inbox or DLQ message filename.
func ValidateMessageFilename(filename string) error {
	if filename == "" {
		return fmt.Errorf("empty filename")
	}
	if strings.HasPrefix(filename, ".") {
		return fmt.Errorf("dotfile names are not allowed")
	}
	if filepath.IsAbs(filename) {
		return fmt.Errorf("absolute path is not allowed")
	}
	if strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		return fmt.Errorf("path separators are not allowed")
	}
	if strings.Contains(filename, "\x00") {
		return fmt.Errorf("NUL byte is not allowed")
	}
	if filename == "." || filename == ".." {
		return fmt.Errorf("path traversal is not allowed")
	}
	if !strings.HasSuffix(filename, ".md") {
		return fmt.Errorf("filename must end with .md")
	}
	return nil
}
