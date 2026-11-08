package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// A session bound to Buzz shows the segment in its status line; any other
// session shows nothing.
func TestStatuslineShowsTheBoundSession(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: dir, Handle: "one", Name: "one", Harness: "claude", HarnessSession: "S"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ session, want string }{{"S", "📡 buzz\n"}, {"unknown", ""}} {
		var out bytes.Buffer
		in := strings.NewReader(`{"session_id":"` + c.session + `","transcript_path":"/x/` + c.session + `.jsonl"}`)
		if code := run([]string{"statusline"}, in, &out, io.Discard); code != 0 || out.String() != c.want {
			t.Fatalf("session %s: exit %d, output %q; want %q", c.session, code, out.String(), c.want)
		}
	}
}
