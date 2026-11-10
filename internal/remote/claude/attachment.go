// Package claude is the Claude Code adapter factory. It registers the
// kind:"claude" factory so a manifest entry
// `{"kind":"claude","target":"<name>","config":{...}}` builds a
// claudeAttachment without editing serve.
//
// Inspect is file-only (session registry + transcript tail): NO child
// process is ever spawned from Inspect. The capability projection is
// HONEST: submit goes over the pinned socket wire
// (docs/research/r0-03-cc-socket-wire-capture.md). No keystrokes, no print
// child, no `claude -p` fallback, no process ownership (ADR invariant 1).
// The Stop-hook installer ships with its receiver subcommand: an installed
// hook whose command does not exist would exit 2, Claude Code's
// blocking-error code — the opposite of failing open.
//
// Evidence vocabulary ("delivered" does not exist in the schema): a
// socket ack alone is TENTATIVE (bound, ownership not proven); the
// transcript user line is SUBMITTED; the assistant turn starting
// (Stop-hook post or transcript assistant line) is ADMITTED.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// SentinelUnpinned is the epoch Inspect reports, matching the other
// seam-backed adapters: this adapter never pins an epoch of its own.
const SentinelUnpinned = "unpinned"

// config is the adapter-specific config block for a claude manifest entry.
type config struct {
	// Pid is the target Claude Code process id. The adapter resolves the
	// session registry file (<config dir>/sessions/<pid>.json) from it; the
	// pid also keys the transcript under <config dir>/projects via sessionId.
	Pid int `json:"pid"`
	// ConfigDir is the absolute Claude config dir the session runs with
	// (its CLAUDE_CONFIG_DIR), recorded by the attaching process inside the
	// session. Empty = home/.claude. The endpoint never reads the variable:
	// its own env is not the session's.
	ConfigDir string `json:"config_dir,omitempty"`
	// Target overrides the published target id (the manifest's declared
	// identity, the same rule as codex/pi). Empty = the registry name,
	// validated, else "claude:<pid>".
	Target string `json:"target,omitempty"`
	// Home overrides the Claude home for tests; empty = os.UserHomeDir().
	Home string `json:"home,omitempty"`
	// Approve is accepted from older manifests and changes nothing: tool
	// dialogs reach Buzz through the PermissionRequest hook and the dialogs
	// directory, not through the attachment.
	Approve bool `json:"approve,omitempty"`
}

// dirs resolves the home that roots AMQ's markers and the config dir that
// roots the session's Claude data: the recorded config_dir, else
// home/.claude. It never reads CLAUDE_CONFIG_DIR.
func (c config) dirs() (home, configDir string, err error) {
	home = c.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", "", fmt.Errorf("resolve home: %w", err)
		}
	}
	configDir = c.ConfigDir
	if configDir == "" {
		configDir = defaultConfigDir(home)
	}
	return home, configDir, nil
}

// TargetSettingsPath is the settings.json of the session a claude manifest
// entry attaches: <config_dir>/settings.json, else
// home/.claude/settings.json. Like the endpoint, it never reads
// CLAUDE_CONFIG_DIR.
func TargetSettingsPath(raw json.RawMessage) (string, error) {
	var c config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return "", fmt.Errorf("parse claude config: %w", err)
		}
	}
	_, dir, err := c.dirs()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// ReplacesEntry reports whether claude config next may replace the manifest
// entry prev of the same target. Both must name the same pid with the same
// settings apart from config_dir and a gained approve, and the two config
// dirs must differ. prev's config dir must hold no registry entry for the
// pid while next's holds an interactive one: that proves prev does not
// describe the session now running as that pid. Any doubt keeps prev.
func ReplacesEntry(prev, next json.RawMessage) bool {
	var p, n map[string]any
	if json.Unmarshal(prev, &p) != nil || json.Unmarshal(next, &n) != nil || p == nil || n == nil {
		return false
	}
	if p["approve"] == true && n["approve"] != true {
		return false
	}
	for _, m := range []map[string]any{p, n} {
		delete(m, "config_dir")
		delete(m, "approve")
	}
	if !reflect.DeepEqual(p, n) {
		return false
	}
	var pc, nc config
	if json.Unmarshal(prev, &pc) != nil || json.Unmarshal(next, &nc) != nil || pc.Pid <= 0 {
		return false
	}
	for _, d := range []string{pc.ConfigDir, nc.ConfigDir} {
		if d != "" && !filepath.IsAbs(d) {
			return false
		}
	}
	_, pdir, perr := pc.dirs()
	_, ndir, nerr := nc.dirs()
	if perr != nil || nerr != nil || filepath.Clean(pdir) == filepath.Clean(ndir) {
		return false
	}
	if old, err := readSessionRegistry(pdir, pc.Pid); err != nil || old != nil {
		return false
	}
	cur, err := readSessionRegistry(ndir, nc.Pid)
	return err == nil && cur != nil && cur.SessionID != "" && (cur.Kind == "" || cur.Kind == "interactive")
}

// sessionRegistry is the subset of ~/.claude/sessions/<pid>.json the
// adapter reads. Unknown fields are ignored (forward-compatible).
type sessionRegistry struct {
	Pid                 int      `json:"pid"`
	SessionID           string   `json:"sessionId"`
	Cwd                 string   `json:"cwd"`
	Version             string   `json:"version"`
	PeerProtocol        int      `json:"peerProtocol"`
	PeerFeatures        []string `json:"peerFeatures"`
	Kind                string   `json:"kind"`
	MessagingSocketPath string   `json:"messagingSocketPath"`
	Name                string   `json:"name"`
	Status              string   `json:"status"`
	UpdatedAt           int64    `json:"updatedAt"` // unix millis
}

// maxRegistryBytes bounds the session-registry read: a real entry is well
// under 1 KiB; anything larger on a local-process-writable path is refused
// before it can stall a read under the endpoint mutex.
const maxRegistryBytes = 64 << 10

// Factory builds a claude Attachment from a registry.FactoryConfig.
func Factory(_ context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	var c config
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &c); err != nil {
			return nil, fmt.Errorf("parse claude config: %w", err)
		}
	}
	if c.Pid <= 0 {
		return nil, fmt.Errorf("claude config: pid is required (the target Claude Code process id)")
	}
	if c.ConfigDir != "" && !filepath.IsAbs(c.ConfigDir) {
		return nil, fmt.Errorf("claude config: config_dir %q is not an absolute path; refusing", c.ConfigDir)
	}
	// The manifest target is the identity the endpoint addresses this
	// adapter under (codex factory comment, ".13"); cfg.Target wins over
	// any registry-derived name, which crosses a trust boundary (the
	// registry file is local-process-writable).
	if cfg.Target != "" {
		c.Target = cfg.Target
	}
	att, err := Attach(c)
	if err != nil {
		return nil, err
	}
	return att, nil
}

// Attach builds the attachment, resolving the session registry once. A
// missing registry entry is a startup refusal: the adapter never guesses a
// session identity (the same identity posture as the pi adapter).
func Attach(cfg config) (*Attachment, error) {
	home, configDir, err := cfg.dirs()
	if err != nil {
		return nil, fmt.Errorf("claude adapter: %w", err)
	}
	reg, err := readSessionRegistry(configDir, cfg.Pid)
	if err != nil {
		return nil, fmt.Errorf("claude adapter: %w", err)
	}
	if reg == nil {
		// No registry entry for the pid: a claude session we cannot identify
		// is never attached (a missing registry file is "unknown session",
		// not "no registry kind"): readSessionRegistry returns nil,nil on
		// absence and Attach refuses it.
		return nil, fmt.Errorf("claude adapter: pid %d has no session registry entry; refusing", cfg.Pid)
	}
	if reg.Kind != "" && reg.Kind != "interactive" {
		return nil, fmt.Errorf("claude adapter: pid %d is kind %q; only interactive sessions attach", cfg.Pid, reg.Kind)
	}
	target := cfg.Target
	if target == "" {
		target = deriveTarget(cfg.Pid, reg)
	}
	if !protocol.ValidTargetID(target) {
		return nil, fmt.Errorf("claude adapter: derived target id %q is not protocol-addressable (opaque grammar); set an explicit target in the manifest", target)
	}
	att := &Attachment{
		target:       target,
		cfg:          cfg,
		home:         home,
		configDir:    configDir,
		boundSession: reg.SessionID,
		runs:         map[requests.Key]*runRecord{},
		cancelIntent: map[requests.Key]bool{},
		recoverFrom:  map[requests.Key]recoverScan{},
		released:     map[requests.Key]struct{}{},
		ctx:          context.Background(),
	}
	token, err := bindStopSession(home, reg.SessionID)
	if err != nil && !errors.Is(err, errUnsupportedPlatform) {
		return nil, fmt.Errorf("claude adapter: %w", err)
	}
	att.boundToken = token
	return att, nil
}

// deriveTarget prefers the registry name when it is protocol-valid; the
// pid form is the fallback. The caller validates the result.
func deriveTarget(pid int, reg *sessionRegistry) string {
	if reg != nil && reg.Name != "" && protocol.ValidTargetID(reg.Name) {
		return reg.Name
	}
	return "claude:" + strconv.Itoa(pid)
}

// markerDir holds AMQ's own markers (amq-stop, amq-bound). It
// is always home/.claude/sessions, whatever config dir a session uses: the
// hook processes and the endpoint resolve it from home alone.
func markerDir(home string) string {
	return filepath.Join(home, ".claude", "sessions")
}

// defaultConfigDir is the Claude config dir of a session that does not set
// CLAUDE_CONFIG_DIR.
func defaultConfigDir(home string) string {
	return filepath.Join(home, ".claude")
}

// sessionConfigDir is the Claude config dir of the session this process
// runs in: CLAUDE_CONFIG_DIR when set, else home/.claude. Only code that
// runs inside the session calls it, and SessionTranscript, which uses what
// it names only when that file exists. A relative value is refused, never
// resolved against a guessed directory.
func sessionConfigDir(home string) (string, error) {
	dir := harness.ClaudeConfigDir(home)
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("CLAUDE_CONFIG_DIR %q is not an absolute path; refusing", dir)
	}
	return filepath.Clean(dir), nil
}

// registryDir is Claude's session registry under config dir dir: the
// <pid>.json entries and the peer key files Claude mints beside them.
func registryDir(dir string) string {
	return filepath.Join(dir, "sessions")
}

// readSessionRegistry reads <config dir>/sessions/<pid>.json.
//
// Trust boundary: this is a Claude-internal registry file on a
// local-process-writable path, not an AMQ state leaf — so it does NOT get
// the state-leaf lstat refusal. Instead the read goes through
// readRegularBounded (safeopen.go): lstat gate, size bound before open,
// no-follow non-blocking open, fstat recheck, and a LimitReader, so neither
// a FIFO nor a growing file can stall or bloat a read under the endpoint
// mutex.
func readSessionRegistry(dir string, pid int) (*sessionRegistry, error) {
	path := filepath.Join(registryDir(dir), strconv.Itoa(pid)+".json")
	reg, err := registryCache.Load(path, func() (*sessionRegistry, error) { return decodeSessionRegistry(path) })
	if reg == nil || err != nil {
		return nil, err
	}
	c := *reg
	c.PeerFeatures = slices.Clone(c.PeerFeatures)
	return &c, nil
}

// registryCache keeps each decoded registry file while it is unchanged:
// serve and the Buzz agent read every session's entry on each sweep.
var registryCache = fsq.NewVersionCache[*sessionRegistry](256)

func decodeSessionRegistry(path string) (*sessionRegistry, error) {
	raw, err := readRegularBounded(path, maxRegistryBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no registry entry yet: not an error
		}
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	var reg sessionRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	if reg.SessionID == "" {
		return nil, fmt.Errorf("session registry %s: sessionId is empty", path)
	}
	return &reg, nil
}

// Attachment implements core.Attachment over the Claude Code surfaces.
// It never injects keystrokes, never spawns a print child, and owns no
// process (ADR invariant 1). Inspect performs NO child-process spawn: it
// reads the session registry file only (a spawn under the endpoint
// mutex would freeze every target for the life of a hung child).
type Attachment struct {
	target string
	cfg    config
	// home roots AMQ's markers; configDir roots Claude's own data (the
	// registry, transcripts, peer key, and settings).
	home      string
	configDir string

	// Evidence path (submit.go): retained runs, cancel intent, the
	// native-event sink, and the confirmation poller's cancel. ctx is the
	// attachment lifetime; the poller is never on the request path.
	mu            sync.Mutex
	runs          map[requests.Key]*runRecord
	cancelIntent  map[requests.Key]bool
	eventSink     func(core.NativeEvent)
	confirmCancel func()
	// stopConsumed is the byte offset in the session's Stop-marker file up
	// to which marker lines are settled (matched to a run or dropped as
	// orphans). Lines past it are preserved for the next poll.
	stopConsumed int64
	// boundSession is the session id published for the Stop-hook receiver.
	// Empty after unsubscribe. User off does not clear it. The receiver
	// writes for this id when no binding file is present, or when the
	// binding names this session.
	boundSession string
	// boundToken is this attachment's sentinel file under amq-bound.
	boundToken string
	// cur is the poller's transcript position; owner is the run that owns
	// the turn the cursor is inside, nil for a foreign or unknown turn.
	cur   transcriptCursor
	owner *runRecord
	// activitySink is the parsed-transcript observer. It is not the core
	// event sink, and its lifetime does not replace Subscribe.
	activitySink func(ActivityNote)
	activityTurn string
	// activityTurnTS is the transcript time of the user boundary that
	// opened activityTurn. An older Stop must not clear a later turn.
	activityTurnTS int64
	// activityPath and activityNext are how far activity has delivered.
	// Confirmation recovery may rewind cur; it must not replay these notes.
	// A truncated file resets activityNext. A recovery rewind does not.
	activityPath string
	activityNext int64
	// curGen increments whenever the cursor is reset outside the poller (a
	// recovered run needs a replay from its delivery offset); a poll that
	// started under an older generation does not write its cursor back.
	curGen uint64
	// recoverFrom[key] is how far a restart-recovery scan has read for a
	// key it has not found yet, so an uncertain record costs only the new
	// transcript bytes on each reconcile tick.
	recoverFrom map[requests.Key]recoverScan
	// released holds keys whose result the endpoint acknowledged: their
	// delivery is still in the transcript, and recovery must never bring
	// them back as a phantom running run. Bounded by maxReleased.
	released map[requests.Key]struct{}
	// closed: the endpoint unsubscribed. The poller is stopped and never
	// restarted; the attachment is being replaced or shut down.
	closed bool
	ctx    context.Context
	// clock replaces the wall clock in tests.
	clock func() time.Time
}

// Inspect implements core.Attachment: the honest projection. Status is
// the registry file's own status field, normalized to the projection
// vocabulary {idle,busy,unknown,offline} (mirroring codex
// threadStatus); the pid is liveness-checked (a stale registry left by a
// crashed session must not read "live" forever). Submit is advertised
// TRUE — the pinned socket wire is wired (submit.go); the interrupt-family
// seams remain false (no keystroke seam,
// docs/remote-compat.md §3). The projection's submit evidence is
// submitted; per-run evidence is carried by Lookup.
func (a *Attachment) Inspect() protocol.Session {
	status, _ := a.observe()
	att := "live"
	if status == "offline" {
		att = "offline"
	}
	return protocol.Session{
		Schema:      protocol.SchemaSession,
		TargetID:    a.target,
		Epoch:       SentinelUnpinned,
		Harness:     "claude_code",
		DisplayName: "claude " + a.target,
		Attachment:  att,
		Status:      status,
		// Submit is true over the pinned socket wire; the rest stay
		// false — no interrupt seam without keystrokes. Tool dialogs reach
		// Buzz through the PermissionRequest hook, not the endpoint.
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: true, CancelRequest: false,
			ApproveTool: false, AnswerQuestion: false, Steer: false,
			Terminal: "unavailable",
		},
		// Submit proves socket delivery; the transcript delivery line proves
		// submitted and the assistant line the run, per request, through
		// Lookup. So the projection is submitted: a relay share for Claude
		// sets min_evidence submitted, and an admitted floor is refused.
		Evidence:   &protocol.Evidence{Submit: protocol.EvidenceSubmitted, Completion: "run_terminal"},
		ObservedAt: protocol.FormatTime(a.now()),
	}
}

// normalizeStatus maps the registry's status vocabulary onto the
// projection's {idle,busy,unknown,offline}. Unknown values — including a
// hostile or future-vocabulary value from the local-process-writable
// registry — map to "unknown", never published raw.
func normalizeStatus(s string) string {
	switch s {
	case "idle":
		return "idle"
	case "busy", "shell", "tool":
		return "busy"
	}
	return "unknown"
}

// NativeSessionID is the live registry entry's sessionId, the identity
// relay sharing pins (core.NativeIdentifier); empty when the session is not
// live.
func (a *Attachment) NativeSessionID() string {
	_, sessionID := a.observe()
	return sessionID
}

// observe returns the projection status and, for a live registry entry, its
// sessionId: the native identity relay sharing pins. It is a pure
// filesystem read: the registry file's status field plus a pid liveness
// check. No child process, no bound that can silently exceed its timeout.
func (a *Attachment) observe() (string, string) {
	reg, err := readSessionRegistry(a.configDir, a.cfg.Pid)
	if err != nil {
		return "offline", ""
	}
	if reg == nil {
		// No registry entry (missing-file path) — offline.
		return "offline", ""
	}
	if alive, err := pidAlive(a.cfg.Pid); err != nil || !alive {
		return "offline", ""
	}
	// A registry status the process does not update any more is still
	// "unknown" in the projection, never a guessed idle.
	if reg.Status == "" {
		return "unknown", reg.SessionID
	}
	return normalizeStatus(reg.Status), reg.SessionID
}

func (a *Attachment) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

// trackBoundSession publishes sessionID for the Stop-hook receiver when the
// live registry session changes. File IO stays under a.mu: the files are
// tiny and the unsubscribe path clears the same field under that lock.
func (a *Attachment) trackBoundSession(sessionID string) {
	a.mu.Lock()
	if a.closed || a.boundSession == sessionID {
		a.mu.Unlock()
		return
	}
	prev, prevToken := a.boundSession, a.boundToken
	a.boundSession = sessionID
	token, _ := bindStopSession(a.home, sessionID)
	a.boundToken = token
	if prev != "" {
		unbindStopSession(a.home, prev, prevToken)
	}
	a.mu.Unlock()
}

// Submit is implemented in submit.go over the pinned socket wire.

// Lookup is implemented in submit.go over the retained run map.

// CancelExact implements core.Attachment: typed refusal, never a zero
// value (an empty Disposition would persist as a fake outcome).
// Claude Code has no user-level interrupt without keystrokes (pinned
// evidence, docs/remote-compat.md §3), so cancellation is unsupported.
func (a *Attachment) CancelExact(key requests.Key, _ string) (core.CancelEvidence, error) {
	return core.CancelEvidence{
		Disposition: protocol.CancelUnsupported,
		Message:     "claude_code has no interrupt seam without keystrokes (docs/remote-compat.md §3); cancellation is unsupported",
	}, nil
}

// Respond implements core.Attachment. Tool dialogs are answered through the
// PermissionRequest hook, so the endpoint has no interaction to answer.
func (a *Attachment) Respond(requests.Key, string, string, string) (protocol.Code, error) {
	return protocol.CodeUnsupported, nil
}

// AcknowledgeResult releases the retained terminal evidence for the key
// (mirrors codex): once the endpoint has persisted the outcome the
// adapter drops its record, so a restarted attachment's Unknown is honest.
func (a *Attachment) AcknowledgeResult(key requests.Key, _, _ string) {
	a.mu.Lock()
	delete(a.runs, key)
	delete(a.recoverFrom, key)
	if len(a.released) >= maxReleased {
		for k := range a.released { // drop an arbitrary old entry
			delete(a.released, k)
			break
		}
	}
	a.released[key] = struct{}{}
	a.mu.Unlock()
}

// maxReleased bounds the released-key set; the endpoint acknowledges each
// key once and never looks a released key up again in normal operation.
const maxReleased = 4096

// Subscribe registers the native-event sink (the confirmation poller
// emits status events; the Stop-hook receiver posts terminal events
// through the same sink). Returns the unsubscribe function.
//
// Unsubscribe is the attachment's end of life: the endpoint calls it when
// the target is replaced, unregistered or shut down, so it also stops the
// confirmation poller for good.
func (a *Attachment) Subscribe(cb func(core.NativeEvent)) func() {
	a.mu.Lock()
	a.eventSink = cb
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.eventSink = nil
		a.closed = true
		sid, token := a.boundSession, a.boundToken
		a.boundSession = ""
		a.boundToken = ""
		stop := a.confirmCancel
		a.confirmCancel = nil
		a.mu.Unlock()
		if sid != "" {
			unbindStopSession(a.home, sid, token)
		}
		if stop != nil {
			stop()
		}
	}
}

// ObserveActivity registers a parsed-transcript observer. It uses the
// confirmation poller already running for this attachment; it does not
// replace Subscribe or start a second reader. The poller keeps running
// while the observer is registered, including when no remote run is open.
// Unsubscribe releases that hold. The callback is not invoked with a.mu held.
func (a *Attachment) ObserveActivity(cb func(ActivityNote)) func() {
	a.mu.Lock()
	a.activitySink = cb
	a.mu.Unlock()
	if cb != nil {
		a.kickConfirmations()
	}
	return func() {
		a.mu.Lock()
		a.activitySink = nil
		a.mu.Unlock()
	}
}

func init() {
	registry.Register("claude", Factory)
}

// SessionIDForPID reads the sessionId of the interactive session with this
// pid from the session registry, the same registry the adapter attaches
// through. amq-remote attach uses it to verify the session it pins is the
// one it runs in.
func SessionIDForPID(pid int) (string, error) {
	s, err := SessionForPID(pid)
	return s.ID, err
}

// Session is what the registry entry of a Claude process says about its
// session: the session id, its working directory, and its name.
type Session struct {
	ID, Cwd, Name string
	// Transcript is the session's transcript when it is a regular file;
	// "" otherwise.
	Transcript string
}

// SessionForPID reads the registry entry of the Claude process pid from
// the session registry of this process's config dir; an error when pid has
// none. It runs inside the session, so the config dir comes from its env.
func SessionForPID(pid int) (Session, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Session{}, err
	}
	dir, err := sessionConfigDir(home)
	if err != nil {
		return Session{}, err
	}
	reg, err := readSessionRegistry(dir, pid)
	if err != nil {
		return Session{}, err
	}
	if reg == nil {
		return Session{}, fmt.Errorf("no Claude session registry entry for pid %d", pid)
	}
	return sessionOf(dir, reg), nil
}

// SessionOfProcess reads the registry entry of the Claude process pid under
// the Claude config dir configDir, for a process that started at started.
// An entry last written (its updatedAt, else its mtime) before that start
// is a reused pid's leftover, never this process's, and is refused; so is a
// missing entry.
func SessionOfProcess(configDir string, pid int, started time.Time) (Session, error) {
	reg, err := readSessionRegistry(configDir, pid)
	if err != nil {
		return Session{}, err
	}
	if reg == nil {
		return Session{}, fmt.Errorf("no Claude session registry entry for pid %d", pid)
	}
	written := time.UnixMilli(reg.UpdatedAt)
	if reg.UpdatedAt <= 0 {
		fi, err := os.Lstat(filepath.Join(registryDir(configDir), strconv.Itoa(pid)+".json"))
		if err != nil {
			return Session{}, err
		}
		written = fi.ModTime()
	}
	if written.Before(started) {
		return Session{}, fmt.Errorf("the Claude session registry entry for pid %d predates its process", pid)
	}
	return sessionOf(configDir, reg), nil
}

// sessionOf is the session reg names under the Claude config dir dir.
func sessionOf(dir string, reg *sessionRegistry) Session {
	s := Session{ID: reg.SessionID, Cwd: reg.Cwd, Name: reg.Name}
	if path := transcriptPath(dir, reg.Cwd, reg.SessionID); regularFile(path) {
		s.Transcript = path
	}
	return s
}

func regularFile(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}
