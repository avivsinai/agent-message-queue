package launch

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxCallerContextEntries    = 32
	MaxCallerContextKeyBytes   = 64
	MaxCallerContextValueBytes = 1024
	MaxCallerContextTotalBytes = 16 * 1024
)

type CallerContextValidationError struct{ Reason string }

func (err *CallerContextValidationError) Error() string { return "caller_context: " + err.Reason }

func callerContextErrorf(format string, args ...any) error {
	return &CallerContextValidationError{Reason: fmt.Sprintf(format, args...)}
}

// ValidateCallerContext validates opaque caller-owned correlation metadata.
func ValidateCallerContext(context map[string]string) error {
	if len(context) > MaxCallerContextEntries {
		return callerContextErrorf("has %d entries, maximum is %d", len(context), MaxCallerContextEntries)
	}
	total := 0
	for key, value := range context {
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return callerContextErrorf("contains invalid UTF-8")
		}
		if strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return callerContextErrorf("must not contain NUL")
		}
		if len(key) == 0 || len(key) > MaxCallerContextKeyBytes {
			return callerContextErrorf("key length must be 1 through %d UTF-8 bytes", MaxCallerContextKeyBytes)
		}
		if len(value) > MaxCallerContextValueBytes {
			return callerContextErrorf("value length must not exceed %d UTF-8 bytes", MaxCallerContextValueBytes)
		}
		total += len(key) + len(value)
	}
	if total > MaxCallerContextTotalBytes {
		return callerContextErrorf("contains %d UTF-8 bytes, maximum is %d", total, MaxCallerContextTotalBytes)
	}
	return nil
}

func cloneCallerContext(context map[string]string) map[string]string {
	if len(context) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(context))
	for key, value := range context {
		cloned[key] = value
	}
	return cloned
}
