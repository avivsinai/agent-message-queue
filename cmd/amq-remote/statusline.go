package main

import (
	"encoding/json"
	"flag"
	"io"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
)

// maxStatusInput bounds the status-line JSON read from stdin.
const maxStatusInput = 1 << 20

// statusline is `amq-remote statusline`: one status-line segment when the
// session is bound to Buzz, else nothing. A status line must never show an
// error or wait, so every failure prints nothing and exits 0. It reads only
// the binding files: no network, no lock, no write.
func statusline(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	session := fs.String("session", "", "the session id (default: session_id from the status-line JSON on stdin)")
	plain := fs.Bool("plain", false, "no emoji")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 0
	}
	id := strings.TrimSpace(*session)
	if id == "" {
		var in struct {
			SessionID      string `json:"session_id"`
			TranscriptPath string `json:"transcript_path"`
		}
		if json.NewDecoder(io.LimitReader(stdin, maxStatusInput)).Decode(&in) != nil {
			return 0
		}
		id = in.SessionID
		if id == "" && in.TranscriptPath != "" {
			id = strings.TrimSuffix(filepath.Base(in.TranscriptPath), filepath.Ext(in.TranscriptPath))
		}
	}
	if id == "" {
		return 0
	}
	all, err := binding.List()
	if err != nil {
		return 0
	}
	for _, b := range all {
		if b.RecordedSession() != id {
			continue
		}
		if state, _ := b.Liveness(); state == binding.Ended {
			continue
		}
		seg := "buzz"
		if !*plain {
			seg = "📡 " + seg
		}
		if dir, err := mirror.DirFor(b.Name); err == nil && mirror.Off(dir) {
			seg += " (mirror off)"
		}
		say(stdout, "%s", seg)
		return 0
	}
	return 0
}
