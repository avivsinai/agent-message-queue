package launch

import (
	"strings"
	"testing"
)

func TestValidateCallerContextBounds(t *testing.T) {
	valid := map[string]string{"": ""}
	if err := ValidateCallerContext(valid); err == nil || !strings.Contains(err.Error(), "key length") {
		t.Fatalf("empty key error = %v", err)
	}
	for _, test := range []struct {
		name    string
		context map[string]string
		want    string
	}{
		{name: "entries", context: callerContextEntries(33), want: "entries"},
		{name: "key", context: map[string]string{strings.Repeat("k", 65): "v"}, want: "key length"},
		{name: "value", context: map[string]string{"key": strings.Repeat("v", 1025)}, want: "value length"},
		{name: "total", context: callerContextTotalOverflow(), want: "UTF-8 bytes"},
		{name: "key NUL", context: map[string]string{"bad\x00key": "v"}, want: "NUL"},
		{name: "value NUL", context: map[string]string{"key": "bad\x00value"}, want: "NUL"},
		{name: "invalid key UTF-8", context: map[string]string{string([]byte{0xff}): "v"}, want: "invalid UTF-8"},
		{name: "invalid value UTF-8", context: map[string]string{"key": string([]byte{0xff})}, want: "invalid UTF-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateCallerContext(test.context); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateCallerContext error = %v, want %q", err, test.want)
			}
		})
	}
	if err := ValidateCallerContext(callerContextEntries(32)); err != nil {
		t.Fatalf("maximum valid context: %v", err)
	}
}

func callerContextEntries(count int) map[string]string {
	context := make(map[string]string, count)
	for i := 0; i < count; i++ {
		context[string(rune('a'+i))] = "v"
	}
	return context
}

func callerContextTotalOverflow() map[string]string {
	context := make(map[string]string, 16)
	for i := 0; i < 15; i++ {
		context[string(rune('a'+i))] = strings.Repeat("v", 1024)
	}
	context["p"] = strings.Repeat("v", 1009)
	return context
}
