package fsq

import (
	"fmt"
	"path/filepath"
	"strings"
)

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
