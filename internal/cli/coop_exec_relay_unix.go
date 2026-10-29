//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/terminalrelay"
)

const (
	agyStatusEnv         = "AGY_CLI_OSC7501"
	agyStatusMessagesEnv = "AGY_CLI_OSC7501_MESSAGES"
)

// agyRelayPlan decides whether coop exec relays agy's terminal. agy runs no
// hooks with Google Cloud sign-in, so the only signal that its permission or
// sign-in prompt is open is the OSC 7501 status it writes to its terminal
// when the person set AGY_CLI_OSC7501=on. Only then, and only when stdin is
// a terminal, coop exec stays as agy's terminal: agy runs on a pty whose
// output the relay reads, and the wake holds the doorbell while a prompt is
// open. Otherwise agy runs directly and gets the plain doorbell.
func agyRelayPlan(binaryPath string, env []string, stdinIsTerminal bool) bool {
	facts, _ := harness.ForExecutable(binaryPath)
	value, _ := lookupEnv(env, agyStatusEnv)
	return facts.Name == harness.Agy && stdinIsTerminal && value == "on"
}

// agyRelayEnv is env for a relayed agy: brief status messages unless the
// person chose otherwise.
func agyRelayEnv(env []string) []string {
	if _, ok := lookupEnv(env, agyStatusMessagesEnv); !ok {
		env = setEnvVar(env, agyStatusMessagesEnv, "brief")
	}
	return env
}

func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return value, true
		}
	}
	return "", false
}

// agyDialogSink keeps the wake dialog marker in step with agy's status:
// blocked:kind=permission and blocked:kind=auth write it, and so does
// working, with kind "working": agy queues a doorbell typed during a turn
// and ignores the Enter-only reminders, so the doorbell waits for the end
// of the turn, as it does for Cursor. done and idle remove it. agy reports
// working once when a turn starts and again after a prompt is answered.
// Another blocked kind changes nothing and is logged once, so a new kind is
// seen, never guessed. The sink removes only the exact
// marker it wrote: a marker another writer put in its place stays. coop
// exec creates a sink only when this launch's wake owns the agent's wake.
type agyDialogSink struct {
	agentDir *wakeAgentDir
	log      io.Writer
	open     string
	written  []byte
	logged   map[string]bool
}

func newAgyDialogSink(root, handle string, log io.Writer) (*agyDialogSink, error) {
	agentDir, err := openExistingWakeAgentDir(root, handle)
	if err != nil {
		return nil, err
	}
	sink := &agyDialogSink{agentDir: agentDir, log: log, logged: map[string]bool{}}
	// An agy marker left by an earlier relay of this seat (a crash) would
	// hold this session; this launch owns the agent's wake, so no other
	// relay of it runs. Any other marker stays.
	_ = agentDir.withFD(func(dirfd int) error {
		return removeWakeDialogMarkerIf(dirfd, agentDir, func(current []byte) bool {
			var marker wakeDialogMarker
			return json.Unmarshal(current, &marker) == nil && marker.Event == agyDialogEvent && marker.Harness == "agy"
		})
	})
	return sink, nil
}

const (
	// agyDialogEvent marks a dialog marker an agy relay wrote.
	agyDialogEvent = "OSC7501"
	// agyWorkingKind is the marker kind of a running agy turn.
	agyWorkingKind = "working"
)

// agyTurnMarker reports a marker of a running agy turn.
func agyTurnMarker(marker wakeDialogMarker) bool {
	return marker.Event == agyDialogEvent && marker.Harness == "agy" && marker.Kind == agyWorkingKind
}

func (s *agyDialogSink) onState(state, kind string) {
	switch state {
	case "blocked":
		if kind != "permission" && kind != "auth" {
			if !s.logged[kind] && s.log != nil {
				s.logged[kind] = true
				_, _ = fmt.Fprintf(s.log, "amq coop exec: agy reports blocked kind %q; the doorbell hold does not change for it\n", kind)
			}
			return
		}
		if s.open == kind {
			return
		}
		s.write(kind)
	case "working":
		// Written each time, so the marker's age is the time since agy last
		// reported the turn.
		s.write(agyWorkingKind)
	case "idle", "done":
		s.clear()
	}
}

// write replaces the marker with one of kind.
func (s *agyDialogSink) write(kind string) {
	raw, err := json.Marshal(wakeDialogMarker{
		Schema: 1, Event: agyDialogEvent, Harness: "agy", Kind: kind,
		OpenedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	if s.agentDir.withFD(func(dirfd int) error {
		return replaceWakeDialogMarkerAt(dirfd, s.agentDir, raw)
	}) == nil {
		s.open, s.written = kind, raw
	}
}

// clear removes the marker only while it is still the one this sink wrote.
func (s *agyDialogSink) clear() {
	if s.written == nil {
		return
	}
	written := s.written
	_ = s.agentDir.withFD(func(dirfd int) error {
		return removeWakeDialogMarkerIf(dirfd, s.agentDir, func(current []byte) bool {
			return bytes.Equal(current, written)
		})
	})
	s.open, s.written = "", nil
}

func (s *agyDialogSink) Close() error {
	s.clear()
	return s.agentDir.Close()
}

// runAgyRelay runs agy on a pty and relays the terminal both ways until agy
// exits, then exits with agy's status (128+signal for a signal). coop exec
// keeps its pid, so the wake's owner binding and controlling terminal are
// unchanged: the wake types into the outer terminal, and the relay forwards
// every input byte to agy. It returns only when agy cannot start.
func runAgyRelay(binaryPath string, argv, env []string, sink *agyDialogSink) error {
	var mu sync.Mutex
	parser := &osc7501Parser{onState: func(state, kind string) {
		mu.Lock()
		defer mu.Unlock()
		sink.onState(state, kind)
	}}
	code, err := terminalrelay.Run(binaryPath, argv, env, agyRelayWriter{parser: parser})
	mu.Lock()
	_ = sink.Close()
	mu.Unlock()
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

type agyRelayWriter struct{ parser *osc7501Parser }

func (w agyRelayWriter) Write(data []byte) (int, error) {
	_, err := os.Stdout.Write(w.parser.feed(data))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func runTerminalRelayHelper(args []string) error {
	os.Exit(terminalrelay.RunHelper(args))
	return nil
}
