package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// unpinnedError is an owner message that names no active channel of a
// session. It chooses no session, so its answer lists the connected ones.
type unpinnedError struct{}

func (*unpinnedError) Error() string {
	return "this prompt came from no session's channel; write in the channel of the session you want"
}

// agentMarker is the machine agent's record under
// <state>/agents/_machine/<pubkey>.json. Each identity has its own file. It
// is rewritten on each tick, so its update time shows the agent runs.
type agentMarker struct {
	Name        string `json:"name"`
	PubKey      string `json:"pubkey"`
	DisplayName string `json:"display_name,omitempty"`
	PID         int    `json:"pid"`
	// Worker is the process identity of the worker that wrote the marker,
	// captured at its start. It is also the marker's generation: an older
	// worker never overwrites a newer one's marker.
	Worker launch.HolderIdentity `json:"worker,omitzero"`
	// RelayKey is the machine agent's relay key (machine.go), so a reader
	// finds its channel records.
	RelayKey string `json:"relay_key,omitempty"`
	// Host is "standalone": the agent runs inside amq-remote serve.
	Host    string `json:"host,omitempty"`
	Updated string `json:"updated"`
}

// agentMarkerFresh is how old a marker may be while its agent still runs.
// A running agent rewrites it at least every agentMarkerRefresh, but a tick
// waits behind a sweep whose posts can take minutes on a slow relay.
const agentMarkerFresh = 10 * time.Minute

// agentMarkerRefresh is how old an unchanged marker gets before a tick
// rewrites it; a changed marker is written on the tick that changes it.
const agentMarkerRefresh = 2 * time.Minute

// Agent states a marker reports. amq-remote reads the same markers.
const (
	// AgentRunning: the marker's worker runs.
	AgentRunning = "running"
	// AgentOffline: the marker's worker does not run.
	AgentOffline = "offline"
)

// AgentState reports what a marker says at now, from its update time and
// the worker identity it records. A worker proven alive is running; a
// fresh marker whose worker cannot be checked either way is running too,
// as is a marker with no identity that is fresh. A worker proven gone is
// offline at once, however fresh the marker is. A stale marker with no
// identity is offline.
func AgentState(updated string, worker launch.HolderIdentity, now time.Time) string {
	at, err := time.Parse(time.RFC3339, updated)
	fresh := err == nil && now.Sub(at) <= agentMarkerFresh
	if worker.PID <= 0 {
		if fresh {
			return AgentRunning
		}
		return AgentOffline
	}
	switch state, _ := launch.InspectHolder(worker); {
	case state == launch.LeaseValid, state == launch.LeaseUnverified && fresh:
		return AgentRunning
	}
	return AgentOffline
}

// workerIdentity is this worker process, set at start (NewStandalone) and
// recorded in every marker it writes.
var workerIdentity struct{ self launch.HolderIdentity }

// newerWorker reports whether worker a started after worker b. Unknown
// start times are never newer.
func newerWorker(a, b launch.HolderIdentity) bool {
	if a.PID <= 0 || b.PID <= 0 || a == b {
		return false
	}
	as, aerr := launch.ProcessStartUnix(a)
	bs, berr := launch.ProcessStartUnix(b)
	return aerr == nil && berr == nil && as > bs
}

// agentMarkerDir is <state>/agents/_machine: the machine agent's marker and
// its lead, dialog and mirror locks.
func (s *Server) agentMarkerDir() string {
	return filepath.Join(s.cfg.StateDir, "agents", machineName)
}

func readAgentMarker(path string) (agentMarker, bool) {
	var m agentMarker
	raw, err := readSmallRegular(path)
	if err != nil || json.Unmarshal(raw, &m) != nil {
		return agentMarker{}, false
	}
	return m, true
}

// lead reports whether this process leads the machine agent's identity: it
// holds _machine/<self>.lock. The first process to take the lock leads until
// it resigns or exits; then the kernel drops the lock and another process
// can take it. Without advisory locks every process leads. A lock error is
// no lead.
func (s *Server) lead() bool {
	if s.leader != nil || !lock.AdvisoryLockAvailable() {
		return true
	}
	dir := s.agentMarkerDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	release, ok, err := lock.TryExclusive(filepath.Join(dir, s.id.Agent+".lock"))
	if err != nil || !ok {
		return false
	}
	s.leader = release
	return true
}

// resign releases the lifecycle lead, if this worker holds it.
func (s *Server) resign() {
	if s.leader != nil {
		s.leader()
		s.leader = nil
	}
}

// writeAgentMarker stamps m as this process's and writes it under dir. Only
// the worker that leads the identity writes (lead), and it never overwrites
// the marker of a worker that started after it. A marker that already says
// the same, younger than agentMarkerRefresh, is left as it is.
func (s *Server) writeAgentMarker(dir string, m agentMarker, now time.Time) {
	path := filepath.Join(dir, s.id.Agent+".json")
	old, ok := readAgentMarker(path)
	if ok && newerWorker(old.Worker, workerIdentity.self) {
		// Holding the lead is the authority to write: a newer worker that
		// left is replaced, and only a newer one still running is kept.
		if state, _ := launch.InspectHolder(old.Worker); state == launch.LeaseValid {
			return
		}
	}
	m.PubKey, m.PID = s.id.Agent, os.Getpid()
	m.Worker = workerIdentity.self
	m.Updated = now.UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if at, err := time.Parse(time.RFC3339, old.Updated); ok && err == nil && !at.After(now) && now.Sub(at) < agentMarkerRefresh {
		old.Updated = m.Updated
		if prev, err := json.MarshalIndent(old, "", "  "); err == nil && string(prev) == string(raw) {
			return
		}
	}
	_ = s.writeState(path, append(raw, '\n'))
}

// sessionAgentName is the name of b's session in Buzz, which its channel
// carries: its label, else "AMQ: <name>" for a binding with no label.
func sessionAgentName(b binding.Binding) string {
	if b.Label != "" {
		return b.Label
	}
	return "AMQ: " + b.Name
}
