package main

import (
	"strings"
	"testing"
)

// R8: link's help is a document, not a parse error. `link --help` and
// `link -h` exit 0 and print the commands.
func TestLinkHelpPrintsUsage(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		var out strings.Builder
		result, code, err := link(args, strings.NewReader(""), outWriter{&out}, nil)
		if code != 0 || err != nil || result != nil {
			t.Fatalf("link %v = (%v, %d, %v); want exit 0", args, result, code, err)
		}
		text := out.String()
		for _, want := range []string{"Usage of the link commands:", "amq-remote link add", "amq-remote link status", "amq-remote link remove"} {
			if !strings.Contains(text, want) {
				t.Fatalf("link %v output does not name %q:\n%s", args, want, text)
			}
		}
	}
}

// outWriter collects writer output for link().
type outWriter struct{ b *strings.Builder }

func (w outWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
