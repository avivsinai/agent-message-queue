//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/avivsinai/agent-message-queue/internal/harness"
)

const (
	agyStatusEnv         = "AGY_CLI_OSC7501"
	agyStatusMessagesEnv = "AGY_CLI_OSC7501_MESSAGES"
	// agyRelayOutputDrain bounds how long the relay copies output after agy
	// exits: a background process agy left may hold the terminal open.
	agyRelayOutputDrain = 200 * time.Millisecond
)

// agyRelayPlan decides how coop exec starts agy. agy runs no hooks with
// Google Cloud sign-in, so the only signal that its permission or sign-in
// prompt is open is the OSC 7501 status it writes to its terminal. coop exec
// then stays as agy's terminal: agy runs on a pty whose output the relay
// reads. relay is false when AGY_CLI_OSC7501 is set to anything but "on",
// or when stdin is no terminal; agy then runs as before and notice, if set,
// says so. strip is true when AMQ turned the status on, so the sequences it
// consumes never reach the outer terminal.
func agyRelayPlan(binaryPath string, env []string, stdinIsTerminal bool) (relayEnv []string, relay, strip bool, notice string) {
	if facts, _ := harness.ForExecutable(binaryPath); facts.Name != harness.Agy || !stdinIsTerminal {
		return env, false, false, ""
	}
	value, set := lookupEnv(env, agyStatusEnv)
	if set && value != "on" {
		return env, false, false, "agy status is off (" + agyStatusEnv + "=" + value + "); a doorbell can answer agy's permission prompt"
	}
	if !set {
		env = setEnvVar(env, agyStatusEnv, "on")
	}
	if _, ok := lookupEnv(env, agyStatusMessagesEnv); !ok {
		env = setEnvVar(env, agyStatusMessagesEnv, "brief")
	}
	return env, true, !set, ""
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
// blocked:kind=permission and blocked:kind=auth write it, idle, working and
// done remove it. Another blocked kind changes nothing and is logged once,
// so a new kind is seen, never guessed. The sink removes only the exact
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

// agyDialogEvent marks a dialog marker an agy relay wrote.
const agyDialogEvent = "OSC7501"

func (s *agyDialogSink) onState(state, kind string) {
	switch state {
	case "blocked":
		if kind != "permission" && kind != "auth" {
			if !s.logged[kind] && s.log != nil {
				s.logged[kind] = true
				_, _ = fmt.Fprintf(s.log, "amq coop exec: agy reports blocked kind %q; the doorbell is not held for it\n", kind)
			}
			return
		}
		if s.open == kind {
			return
		}
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
	case "idle", "working", "done":
		s.clear()
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
func runAgyRelay(binaryPath string, argv, env []string, sink *agyDialogSink, strip bool) error {
	cmd := exec.Command(binaryPath, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Env = env
	stdin := int(os.Stdin.Fd())
	size, sizeErr := pty.GetsizeFull(os.Stdin)
	if sizeErr != nil {
		size = nil
	}
	master, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return fmt.Errorf("start agy on a pty: %w", err)
	}
	saved, err := term.MakeRaw(stdin)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("put the terminal in raw mode for agy: %w", err)
	}
	restore := func() { _ = term.Restore(stdin, saved) }
	defer restore()

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	go func() {
		for sig := range signals {
			if sig == syscall.SIGWINCH {
				_ = pty.InheritSize(os.Stdin, master)
				continue
			}
			_ = cmd.Process.Signal(sig)
		}
	}()

	go func() { _, _ = io.Copy(master, os.Stdin) }()

	var mu sync.Mutex
	parser := &osc7501Parser{strip: strip, onState: func(state, kind string) {
		mu.Lock()
		defer mu.Unlock()
		sink.onState(state, kind)
	}}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if _, werr := os.Stdout.Write(parser.feed(buf[:n])); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	_ = cmd.Wait()
	select {
	case <-drained:
	case <-time.After(agyRelayOutputDrain):
	}
	mu.Lock()
	_ = sink.Close()
	mu.Unlock()
	restore()
	os.Exit(agyExitCode(cmd.ProcessState))
	return nil
}

// agyExitCode is the status a shell reports for agy.
func agyExitCode(state *os.ProcessState) int {
	if state == nil {
		return ExitError
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	return ExitError
}
