//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	// coopWaitOwnerFile names the newest waiter of an agent. Every turn end
	// starts a waiter with a new timeout, so the newest one is the doorbell
	// and an older one exits when it reads another name here.
	coopWaitOwnerFile = ".rewake-hook.owner"
	// coopWaitLockFile is held by the one waiter of an agent that may ring.
	coopWaitLockFile = ".rewake-hook.lock"
	// coopWaitPoll is how often the waiter looks at the inbox, its owner
	// file and its binding when it has no file watcher, and while it waits
	// for the waiter lock, whose release makes no file event. It reads the
	// wake settings when mail is unread.
	coopWaitPoll = time.Second
	// coopWaitHandOver bounds the time from a waiter's owner file to its
	// hold of the waiter lock: the older waiter leaves within one poll of
	// the file, also inside its debounce, and the newer one tries the lock
	// again within one poll of that. The third poll is the margin.
	coopWaitHandOver = 3 * coopWaitPoll
	// coopWaitIdle is how often a waiter that holds the lock and watches its
	// files looks again with no file event: for the facts that make none, such
	// as its parent, its host session, its binding, a wake that died with
	// its lock in place, and due follow-ups.
	coopWaitIdle = 30 * time.Second
	// coopWaitFollowUpEvery bounds how often the waiter promotes due
	// follow-ups into the inbox it watches.
	coopWaitFollowUpEvery = 30 * time.Second
)

// runCoopWait is the waiter. It returns nil, and so exits 0, whenever it is
// not this session's doorbell: no participant, or a harness that would hold
// the event on it. A ring is errCoopWaitRing.
func runCoopWait(ctx context.Context) error {
	root, me, _, err := coopHookContext()
	if err != nil || root == "" || me == "" {
		return nil
	}
	claude, ok := coopWaitAdmits(readSeatDoorbell(root, me).optIn)
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
// no longer held. While a wake rings the agent it stays silent. Between
// looks it waits for a file event of the inbox or of a file it reads, or
// for its next tick; the debounce runs on its clock alone.
func waitCoopMail(ctx context.Context, root, me string, w coopWaiter) error {
	agent := fsq.AgentBase(root, me)
	statePath := filepath.Join(agent, coopWaitStateFile)
	ownerPath := filepath.Join(agent, coopWaitOwnerFile)
	owner := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := writeCoopHookState(ownerPath, owner); err != nil {
		return nil
	}
	var extra []string
	if path, err := machineWakeSettingsPath(); err == nil {
		extra = append(extra, path)
	}
	events := newCoopWaitEvents(agent, extra...)
	defer events.close()
	var (
		release  func()
		layers   wakeSettingsLayers
		debounce time.Time // when the debounce of the due mail ends; zero: none runs
		inbox    coopInbox
		promoted time.Time
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
		if events.live() && release != nil {
			wait = coopWaitIdle
		}
		quiet, debouncing := false, false
		var due []string
		// The lock-bound half of the seat read is taken at every look; the
		// settings files are read only when mail is unread.
		wake := observeSeatWake(root, me)
		if release != nil && wake.ringer(false) == doorbellHook {
			if now := time.Now(); now.Sub(promoted) >= coopWaitFollowUpEvery {
				promoted = now
				// The hook's stderr is the ring text, so a failure stays
				// silent here; the next drain reports it.
				_ = promoteFollowUpsAtPath(root, me, now)
			}
			wait = boundWait(wait, promoted.Add(coopWaitFollowUpEvery), time.Now())
			pending, err := inbox.pending(root, me)
			if err == nil && len(pending) > 0 {
				var settings wakeSettings
				settings, layers = coopWaitSettings(root, me, layers)
				seen := coopAnnounced(agent, w.session, false)
				now := time.Now()
				var next time.Time
				due, next = decideCoopWait(pending, seen.ids, settings.holdPolicy(), now)
				if wake.ringer(settings.attended) != doorbellHook {
					due, next = nil, time.Time{}
				}
				if len(due) > 0 && debounce.IsZero() {
					debounce = now.Add(settings.debounce)
					debouncing = true
				}
				switch {
				case len(due) > 0 && (debouncing || now.Before(debounce)):
					// The Stop guard of the same turn end may be about to
					// report this mail; let it, and batch a burst. A newer
					// waiter takes the doorbell within one poll, so the
					// debounce is waited in polls and ends at its own time.
					wait = boundWait(coopWaitPoll, debounce, now)
					quiet = true
				case len(due) > 0:
					// Ring only after the record is stored: a ring the next
					// waiter cannot see would repeat at every turn end.
					before, beforeErr := os.Lstat(statePath)
					if err := writeCoopHookState(statePath, nextCoopWaitState(seen.rang, pending, due)); err != nil {
						return nil
					}
					undo, err := w.ring()
					if undo {
						undoCoopWaitRing(statePath, seen.rang, before, beforeErr)
					}
					return err
				default:
					wait = boundWait(wait, next, now)
				}
			}
		}
		if len(due) == 0 {
			debounce = time.Time{}
		}
		if quiet {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
		} else if !events.wait(ctx, wait) {
			return nil
		}
	}
}

// undoCoopWaitRing puts back the rang record of a ring that did not reach
// the session. The record's file time is the wake's evidence of a recent
// ring, so the earlier time comes back with the earlier record, and a
// record that did not exist before the ring is removed.
func undoCoopWaitRing(path string, rang coopWaitState, before os.FileInfo, beforeErr error) {
	if errors.Is(beforeErr, fs.ErrNotExist) {
		_ = os.Remove(path)
		return
	}
	if writeCoopHookState(path, rang) != nil || before == nil {
		return
	}
	_ = os.Chtimes(path, time.Time{}, before.ModTime())
}

// boundWait returns wait cut so that it ends no later than at, and never
// below zero. A zero at leaves wait as it is.
func boundWait(wait time.Duration, at, now time.Time) time.Duration {
	if at.IsZero() {
		return wait
	}
	return max(min(wait, at.Sub(now)), 0)
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
func decideCoopWait(pending []coopStopMessage, announced map[string]time.Time, policy wakeHoldPolicy, now time.Time) (due []string, next time.Time) {
	taken := make(map[string]bool)
	for _, msg := range pending {
		if _, ok := announced[msg.ID]; msg.ID == "" || ok || taken[msg.ID] {
			continue
		}
		hold := newWakeHold(policy, msg.Priority, msg.File, now)
		if !hold.due.After(now) {
			due = append(due, msg.ID)
			taken[msg.ID] = true
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

// coopWaitHandOverLeft is how long the newest waiter of (root, me) still
// has at now to take the waiter lock: coopWaitHandOver from its owner file.
// It is zero when no waiter declared itself in that time. Between an older
// waiter that left and the newest one no process holds the lock. The owner
// file is read without following a link and is never created.
func coopWaitHandOverLeft(root, me string, now time.Time) time.Duration {
	info, err := os.Lstat(filepath.Join(fsq.AgentBase(root, me), coopWaitOwnerFile))
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	age, ok := hookRecordAge(now, info.ModTime())
	if !ok {
		return 0
	}
	return max(coopWaitHandOver-age, 0)
}

// coopWaitRunsInBackground reports whether the Claude Code session this
// hook runs in keeps an asyncRewake hook in the background, for a seat
// whose opt-in is optIn.
func coopWaitRunsInBackground(optIn bool) bool {
	_, ok := coopWaitAdmits(optIn)
	return ok
}

// coopWaitAdmits returns the host session of this hook when it may run the
// waiter: a host that needs an opt-in is admitted only when optIn, the
// opt-in the caller already read for the seat.
func coopWaitAdmits(optIn bool) (launch.HolderIdentity, bool) {
	host, support, ok := coopWaitHost()
	return host, ok && (!support.OptIn || optIn)
}

// coopWaitHost admits only a recorded ancestor session whose entrypoint
// and version keep an asyncRewake command in the background.
func coopWaitHost() (launch.HolderIdentity, harness.ClaudeRewakeSupport, bool) {
	session, configDir, ok := claudeSessionID()
	if !ok {
		return launch.HolderIdentity{}, harness.ClaudeRewakeSupport{}, false
	}
	record, host, ok := ancestorClaudeSession(configDir, session)
	support, known := harness.ClaudeRewakeEntrypoints[record.Entrypoint]
	if !ok || !known {
		return host, support, false
	}
	cmp, ok := update.CompareVersions(support.MinVersion, record.Version)
	return host, support, ok && cmp <= 0
}

// errCoopWaitRing is the waiter's ring. Its text is the doorbell, which
// amq prints on stderr as it does every error, and its code is the ring
// code.
var errCoopWaitRing = WithExitCode(harness.ClaudeRewakeRingExit, errors.New(harness.Doorbell))
