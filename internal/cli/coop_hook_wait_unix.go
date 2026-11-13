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
	// file and its binding. It reads the wake settings when mail is unread.
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
	claude, ok := coopWaitHost()
	if !ok {
		return nil
	}
	root = canonicalWakeRoot(root)
	if info, err := os.Stat(fsq.AgentBase(root, me)); err != nil || !info.IsDir() {
		return nil
	}
	return waitCoopMail(ctx, root, me, claudeWaiter(os.Getenv(envClaudeSession), os.Getppid(), claude))
}

// coopWaiter is what one harness gives the wait loop.
type coopWaiter struct {
	// session keys the rang record: what one session heard says nothing
	// about another that takes the handle.
	session string
	// parent is the process that started the waiter; when it changes, the
	// harness ended the hook.
	parent int
	// held reports whether the session's binding is still its binding.
	held func() bool
	// gone reports whether the host session ended while its hook's
	// process survived it.
	gone func() bool
	// ring rings for the due mail, after the rang record is stored. undo
	// puts the record back: the ring did not reach the session, so the next
	// waiter rings again. err is what the waiter returns.
	ring func() (undo bool, err error)
}

// claudeWaiter is the waiter of a Claude Code session whose Claude Code
// process is claude. Its ring is errCoopWaitRing.
func claudeWaiter(claudeSession string, parent int, claude launch.HolderIdentity) coopWaiter {
	return coopWaiter{
		session: claudeSession,
		parent:  parent,
		held:    claudeBindingStillHeld,
		gone: func() bool {
			state, _ := launch.InspectHolder(claude)
			return state == launch.LeaseStale
		},
		ring: func() (bool, error) { return false, errCoopWaitRing },
	}
}

// waitCoopMail waits until mail is due for the waiter's agent, then
// records it and rings. It returns nil when ctx ends, when the hook's
// process parent changed (the harness ended the hook), when the host
// session is gone (a process that was killed leaves the hook's shell
// behind), when a newer waiter took over, or when the session's binding is
// no longer held. While a wake rings the agent it stays silent.
func waitCoopMail(ctx context.Context, root, me string, w coopWaiter) error {
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
		inbox     coopInbox
	)
	defer func() {
		if release != nil {
			release()
		}
	}()
	for {
		if os.Getppid() != w.parent || !coopWaitOwns(ownerPath, owner) || !w.held() || w.gone() {
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
			pending, err := inbox.pending(root, me)
			if err == nil && len(pending) > 0 {
				var settings wakeSettings
				settings, layers = coopWaitSettings(root, me, layers)
				rang := readCoopWaitState(statePath, w.session)
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
					undo, err := w.ring()
					if undo {
						_ = writeCoopHookState(statePath, rang)
					}
					return err
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
	skip := coopIDSet(announced)
	for _, msg := range pending {
		if msg.ID == "" || skip[msg.ID] {
			continue
		}
		hold := newWakeHold(policy, msg.Priority, msg.File, now)
		if !hold.due.After(now) {
			due = append(due, msg.ID)
			skip[msg.ID] = true
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
	rang, kept := coopIDSet(old.RangIDs), coopIDSet(due)
	for _, msg := range pending {
		if rang[msg.ID] && !kept[msg.ID] {
			next.RangIDs = append(next.RangIDs, msg.ID)
			kept[msg.ID] = true
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
// machine file, then the agent file. The waiter reads it at every poll that
// finds unread mail, so a change applies while it waits. last is the previous result's layers: a
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

// coopWaitRuns reports whether a wait hook holds the doorbell of (root, me).
// The one waiter that may ring keeps the agent's waiter lock for as long as
// its process lives, so a lock that cannot be taken is a waiter that runs.
// Looking changes nothing in a root that may not be the caller's own: the
// lock file is opened only if it exists as a regular file, read-only and
// without following a link, and it is never created.
func coopWaitRuns(root, me string) bool {
	f, _, err := fsq.OpenRegularNoFollow(filepath.Join(fsq.AgentBase(root, me), coopWaitLockFile))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	ok, err := lock.TryLock(f)
	return err == nil && !ok
}

// coopWaitRunsInBackground reports whether the Claude Code session this
// hook runs in keeps an asyncRewake hook in the background.
func coopWaitRunsInBackground() bool {
	_, ok := coopWaitHost()
	return ok
}

// coopWaitHost returns the Claude Code process of the session this hook
// runs in, when that session keeps an asyncRewake hook in the background.
// A session that does not would hold its turn end until the waiter exits,
// so anything unproven is false: no session record among this process's
// ancestors for CLAUDE_CODE_SESSION_ID, an entrypoint AMQ has not seen
// background the hook, or a version below the one it was seen on.
func coopWaitHost() (claude launch.HolderIdentity, ok bool) {
	session, configDir, ok := claudeSessionID()
	if !ok {
		return launch.HolderIdentity{}, false
	}
	record, claude, ok := ancestorClaudeSession(configDir, session)
	if !ok {
		return launch.HolderIdentity{}, false
	}
	cmp, ok := update.CompareVersions(harness.ClaudeRewakeMinVersion, record.Version)
	return claude, ok && cmp <= 0 && slices.Contains(harness.ClaudeRewakeEntrypoints, record.Entrypoint)
}

// errCoopWaitRing is the waiter's ring. Its text is the doorbell, which
// amq prints on stderr as it does every error, and its code is the ring
// code.
var errCoopWaitRing = WithExitCode(harness.ClaudeRewakeRingExit, errors.New(harness.Doorbell))
