//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/update"
)

// The co-op waiter is the doorbell of a Claude Code session that no wake
// rings, such as a session of the desktop app. amq coop install-hooks runs
// it as an asyncRewake hook: Claude Code keeps it in the background, and
// when it exits with code 2 Claude Code starts a turn with the hook's
// stderr. The waiter waits for unread mail that is due and that no hook has
// announced, records it, and rings with the fixed doorbell text. It never
// puts sender text in front of the model, and a ring is no proof that the
// agent read anything.

const (
	// coopWaitStateFile holds the ids the waiter rang for, and for which
	// Claude session. It is apart from the Stop guard's state: the guard
	// still blocks once for mail the waiter rang for, so a ring that Claude
	// Code dropped loses no mail. A record is an attempt, not a delivery.
	coopWaitStateFile = ".rewake-hook-state.json"
	// coopWaitOwnerFile names the newest waiter of an agent. Every turn end
	// starts a waiter with a new timeout, so the newest one is the doorbell
	// and an older one exits when it reads another name here.
	coopWaitOwnerFile = ".rewake-hook.owner"
	// coopWaitLockFile is held by the one waiter of an agent that may ring.
	coopWaitLockFile = ".rewake-hook.lock"
	// coopWaitPoll is how often the waiter looks at the inbox, its owner
	// file, its binding, and the wake settings.
	coopWaitPoll = time.Second
)

type coopWaitState struct {
	Schema        int      `json:"schema"`
	ClaudeSession string   `json:"claude_session"`
	RangIDs       []string `json:"rang_ids"`
}

// runCoopWait is the waiter. It returns nil, and so exits 0, whenever it is
// not this session's doorbell: no participant, or a harness that would hold
// the event on it. A ring is errCoopWaitRing.
func runCoopWait(ctx context.Context) error {
	root, me, _, err := coopHookContext()
	if err != nil || root == "" || me == "" {
		return nil
	}
	if !coopWaitRunsInBackground() {
		return nil
	}
	root = canonicalWakeRoot(root)
	if info, err := os.Stat(fsq.AgentBase(root, me)); err != nil || !info.IsDir() {
		return nil
	}
	return waitCoopMail(ctx, root, me, os.Getenv(envClaudeSession), os.Getppid())
}

// waitCoopMail waits until mail is due for claudeSession's agent, then
// records it and returns errCoopWaitRing. It returns nil when ctx ends,
// when the hook's shell is gone (Claude Code ended the hook or exited),
// when a newer waiter took over, or when the session's binding is no
// longer held. While a wake rings the agent it stays silent.
func waitCoopMail(ctx context.Context, root, me, claudeSession string, parent int) error {
	agent := fsq.AgentBase(root, me)
	statePath := filepath.Join(agent, coopWaitStateFile)
	ownerPath := filepath.Join(agent, coopWaitOwnerFile)
	owner := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := writeCoopHookState(ownerPath, owner); err != nil {
		return nil
	}
	var (
		release   func()
		layers    wakeSettingsLayers
		debounced bool
	)
	defer func() {
		if release != nil {
			release()
		}
	}()
	for {
		if os.Getppid() != parent || !coopWaitOwns(ownerPath, owner) || !claudeBindingStillHeld() {
			return nil
		}
		if release == nil {
			// The older waiter exits within one poll of reading the owner
			// file; until then it alone may ring.
			held, ok, err := lock.TryExclusive(filepath.Join(agent, coopWaitLockFile))
			if err != nil {
				return nil
			}
			if ok {
				release = held
			}
		}
		wait := coopWaitPoll
		var due []string
		if release != nil && !coopHookWakeRings(root, me) {
			pending, err := coopStopPending(root, me)
			if err == nil && len(pending) > 0 {
				var settings wakeSettings
				settings, layers = coopWaitSettings(root, me, layers)
				rang := readCoopWaitState(statePath, claudeSession)
				announced := append(readCoopStopState(filepath.Join(agent, coopStopStateFile)).BlockedIDs, rang.RangIDs...)
				now := time.Now()
				var next time.Time
				due, next = decideCoopWait(pending, announced, settings.holdPolicy(), now)
				switch {
				case len(due) > 0 && !debounced:
					// The Stop guard of the same turn end may be about to
					// report this mail; let it, and batch a burst.
					wait = settings.debounce
				case len(due) > 0:
					// Ring only after the record is stored: a ring the next
					// waiter cannot see would repeat at every turn end.
					if err := writeCoopHookState(statePath, nextCoopWaitState(rang, pending, due)); err != nil {
						return nil
					}
					return errCoopWaitRing
				case !next.IsZero() && next.Sub(now) < wait:
					wait = next.Sub(now)
				}
			}
		}
		debounced = len(due) > 0
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// coopWaitOwns reports whether the owner file still names this waiter.
func coopWaitOwns(path, owner string) bool {
	raw, err := os.ReadFile(path)
	var name string
	return err == nil && json.Unmarshal(raw, &name) == nil && name == owner
}

// decideCoopWait returns the ids of the unread mail that is due and not in
// announced, and the earliest time a held message is due (zero when none
// is held). A message is due when its hold has passed; the hold runs from
// the inbox file's time, as the wake's does.
func decideCoopWait(pending []coopStopMessage, announced []string, policy wakeHoldPolicy, now time.Time) (due []string, next time.Time) {
	for _, msg := range pending {
		if msg.ID == "" || slices.Contains(announced, msg.ID) || slices.Contains(due, msg.ID) {
			continue
		}
		hold := newWakeHold(policy, msg.Priority, msg.File, now)
		if !hold.due.After(now) {
			due = append(due, msg.ID)
			continue
		}
		if next.IsZero() || hold.due.Before(next) {
			next = hold.due
		}
	}
	slices.Sort(due)
	return due, next
}

// nextCoopWaitState keeps the rang ids that are still unread and adds due.
func nextCoopWaitState(old coopWaitState, pending []coopStopMessage, due []string) coopWaitState {
	next := coopWaitState{Schema: 1, ClaudeSession: old.ClaudeSession, RangIDs: slices.Clone(due)}
	for _, msg := range pending {
		if slices.Contains(old.RangIDs, msg.ID) && !slices.Contains(next.RangIDs, msg.ID) {
			next.RangIDs = append(next.RangIDs, msg.ID)
		}
	}
	slices.Sort(next.RangIDs)
	return next
}

// readCoopWaitState reads the ids rung for claudeSession. A record of
// another session says nothing about what this one heard, so it reads as
// none.
func readCoopWaitState(path, claudeSession string) coopWaitState {
	state := coopWaitState{Schema: 1, ClaudeSession: claudeSession}
	raw, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	var read coopWaitState
	if json.Unmarshal(raw, &read) != nil || read.ClaudeSession != claudeSession {
		return state
	}
	read.Schema = 1
	return read
}

// coopWaitSettings is the wake policy for (root, me): the defaults, the
// machine file, then the agent file. The waiter reads it at every poll, so
// a change applies while it waits. last is the previous result's layers: a
// file refused now keeps its last good value, and a file refused at the
// first read is left out, as at a wake start.
func coopWaitSettings(root, me string, last wakeSettingsLayers) (wakeSettings, wakeSettingsLayers) {
	agent := wakeSettingsLayerRead{doc: last.agent}
	machine := wakeSettingsLayerRead{doc: last.machine}
	if dir, err := openWakeDirectory(fsq.AgentBase(root, me), "wake agent directory"); err == nil {
		defer func() { _ = dir.Close() }()
		if observed, ok := readWakeSettingsSource(func() ([]byte, bool, error) { return readWakeSettingsFile(dir) }); ok {
			agent = decodeWakeSettingsObservation(observed)
		}
	}
	if observed, ok := readWakeSettingsSource(readMachineWakeSettings); ok {
		machine = decodeWakeSettingsObservation(observed)
	}
	return mergeWakeSettingsLayers(agent, machine, last)
}

// coopWaitRunsInBackground reports whether the Claude Code session this
// hook runs in keeps an asyncRewake hook in the background. A session that
// does not would hold its turn end until the waiter exits, so anything
// unproven is false: no session record among this process's ancestors for
// CLAUDE_CODE_SESSION_ID, an entrypoint AMQ has not seen background the
// hook, or a version below the one it was seen on.
func coopWaitRunsInBackground() bool {
	session := os.Getenv("CLAUDE_CODE_SESSION_ID")
	home, err := os.UserHomeDir()
	if session == "" || err != nil {
		return false
	}
	dir := harness.ClaudeConfigDir(home)
	if !filepath.IsAbs(dir) {
		return false
	}
	for _, ancestor := range launch.Ancestors() {
		record, err := readClaudeSessionRecord(dir, ancestor.PID)
		if err != nil || record.SessionID != session {
			continue
		}
		cmp, ok := update.CompareVersions(harness.ClaudeRewakeMinVersion, record.Version)
		return ok && cmp <= 0 && slices.Contains(harness.ClaudeRewakeEntrypoints, record.Entrypoint)
	}
	return false
}

// errCoopWaitRing is the waiter's ring. Its text is the doorbell, which
// amq prints on stderr as it does every error, and its code is the ring
// code.
var errCoopWaitRing = WithExitCode(harness.ClaudeRewakeRingExit, errors.New(harness.Doorbell))
