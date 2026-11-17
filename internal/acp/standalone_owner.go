package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// routeSessionPrefix names the session of one route on a standalone
// server: "route/<channel>".
const routeSessionPrefix = "route/"

// Owner takes one owner message of a standalone server. The message is
// decided first (decideOwner): a mirror switch,
// a slash command or a dialog answer is carried out at once, never behind
// a turn, and its acknowledgement is posted once per event (postOnce). Any
// other message is a turn: it waits in its route's queue, and each route
// runs one turn at a time (runRemote). A message in a channel that is not
// an active route of this agent does nothing.
//
// The event's claim, remote-events/<agent>/<event>.json, is the eventClaim
// that turnBinding reads, created exclusively after the decision and
// before the act. Decided names the decision's kind; a turn's claim has
// none and holds the route's binding. When the claim already exists:
//   - it is this agent's and Decided is set: nothing is done;
//   - it is this agent's and Decided is empty: the turn runs again, and
//     runRemote resumes it (one request, one final post per event);
//   - it is another agent's, or unreadable: nothing is done.
//
// A route whose session is no longer bound writes no claim; its turn gets
// the refusal of runRemote, posted once per event.
//
// The returned channel closes once the handling has returned: after the
// acknowledgement of a decision, or after the turn. Messages are handled
// only while the machine workers run (StartMachine), and their stop waits
// for every decision in flight, so nothing is posted after it. A message
// that arrives outside them, a turn cut by their stop, or one still queued
// at the stop is not handled: its channel stays open, and a later delivery
// handles it (a turn's claim lets it run again).
func (s *Server) Owner(in buzzop.Inbound) <-chan struct{} {
	done := make(chan struct{})
	r, routed := s.channelRoute(in.Channel)
	if !routed {
		close(done)
		return done
	}
	if !s.owners.running() {
		return done
	}
	evt, ch, id := in.Event, in.Channel, in.Event.ID.Hex()
	path := filepath.Join(s.eventDir(id), id+".json")
	if c, found := readClaim(path); found {
		if c == nil || c.Decided != "" || !s.ownsClaim(*c, "") {
			close(done)
			return done
		}
		s.owners.queue(s, ch, ownerTurn{evt: evt, done: done})
		return done
	}
	m := ownerMsg{channel: ch, words: dropMentions(evt.Content), event: evt}
	if d, ok := s.decideOwner(m); ok {
		won, err := writeClaim(path, eventClaim{Name: r.Name, Agent: s.id.Agent, Decided: d.kind})
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "amq-acp: the owner's %s in %s was not taken: %v\n", d.kind, ch, err)
			close(done)
			return done
		case !won:
			return s.Owner(in)
		}
		started := s.owners.spawn(func() {
			defer close(done)
			ack, _ := s.act(ch, d)
			s.postOnce(id, postFinal, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), ch, ack)
		})
		if !started {
			// The workers stopped after the claim was written: the decision
			// is not carried out, and its claim keeps a replay from doing it.
			fmt.Fprintf(os.Stderr, "amq-acp: the owner's %s in %s was not carried out: the machine agent stopped\n", d.kind, ch)
			close(done)
		}
		return done
	}
	// A claim that cannot be written here is written, or refused, by
	// turnBinding when the turn runs.
	if b, err := routeBinding(r); err == nil {
		if won, err := writeClaim(path, eventClaim{Binding: b, Name: b.Name, Agent: s.id.Agent}); err == nil && !won {
			return s.Owner(in)
		}
	}
	s.owners.queue(s, ch, ownerTurn{evt: evt, done: done})
	return done
}

// readClaim reads the event claim at path. found is false when there is
// none; c is nil when one exists but cannot be read.
func readClaim(path string) (c *eventClaim, found bool) {
	raw, err := readSmallRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	var claim eventClaim
	if err != nil || json.Unmarshal(raw, &claim) != nil {
		return nil, true
	}
	return &claim, true
}

// writeClaim creates the event claim c at path; won is false when a claim
// is already there.
func writeClaim(path string, c eventClaim) (won bool, err error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	return createExclusive(path, raw)
}

// ownerTurns are a standalone server's queued owner turns: one FIFO per
// route and, while it holds turns, one worker that runs them in order. The
// head of a FIFO is the turn its worker runs.
type ownerTurns struct {
	mu      sync.Mutex
	quit    <-chan struct{} // the machine workers' quit; nil while they do not run
	queues  map[string][]ownerTurn
	workers sync.WaitGroup
}

// ownerTurn is one owner message to run as a turn; done closes when it has
// run.
type ownerTurn struct {
	evt  nostr.Event
	done chan struct{}
}

// start lets turns run until quit closes.
func (o *ownerTurns) start(quit <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.quit, o.queues = quit, map[string][]ownerTurn{}
}

// stop drops the queued turns and waits for the workers, which end once
// quit has closed and their running turns are cut.
func (o *ownerTurns) stop() {
	o.mu.Lock()
	o.quit, o.queues = nil, nil
	o.mu.Unlock()
	o.workers.Wait()
}

// running reports whether the machine workers run.
func (o *ownerTurns) running() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.quit != nil && !closed(o.quit)
}

// spawn runs fn as a worker that stop waits for. Outside start and stop it
// runs nothing and reports false.
func (o *ownerTurns) spawn(fn func()) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.quit == nil || closed(o.quit) {
		return false
	}
	o.workers.Add(1)
	go func() {
		defer o.workers.Done()
		fn()
	}()
	return true
}

// queue adds t to the FIFO of route ch and starts its worker when none
// runs. Outside start and stop it does nothing.
func (o *ownerTurns) queue(s *Server, ch string, t ownerTurn) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.quit == nil || closed(o.quit) {
		return
	}
	o.queues[ch] = append(o.queues[ch], t)
	if len(o.queues[ch]) == 1 {
		o.workers.Add(1)
		go o.work(s, ch, o.quit)
	}
}

// work runs the turns of route ch in order until its FIFO is empty or quit
// closes. The pop of a run turn and the check for the next are one hold of
// mu, so queue never sees an empty FIFO while this worker still runs.
func (o *ownerTurns) work(s *Server, ch string, quit <-chan struct{}) {
	defer o.workers.Done()
	o.mu.Lock()
	defer o.mu.Unlock()
	for o.quit == quit && !closed(quit) {
		if len(o.queues[ch]) == 0 {
			delete(o.queues, ch)
			return
		}
		t := o.queues[ch][0]
		o.mu.Unlock()
		if !s.runOwnerTurn(ch, t.evt, quit) {
			close(t.done)
		}
		o.mu.Lock()
		if o.quit == quit {
			o.queues[ch] = o.queues[ch][1:]
		}
	}
}

// closed reports whether quit has closed.
func closed(quit <-chan struct{}) bool {
	select {
	case <-quit:
		return true
	default:
		return false
	}
}

// runOwnerTurn runs evt as a turn of route ch's session: its route and
// reply channel are ch, and it has no client to emit to. The
// route's worker is the only one that begins turns on its session, so the
// turn is never refused. When quit closes, the turn is cut as a client
// that left (turnDisconnected), which records nothing; cut reports that.
func (s *Server) runOwnerTurn(ch string, evt nostr.Event, quit <-chan struct{}) (cut bool) {
	sid, id := routeSessionPrefix+ch, evt.ID.Hex()
	s.mu.Lock()
	if _, ok := s.sessions[sid]; !ok {
		s.sessions[sid] = &sessionState{ID: sid, ChannelID: ch, Thread: cockpitThread(sid), route: ch}
	}
	session, turn, _ := s.beginTurnLocked(sid)
	turn.route, turn.channel, turn.event = ch, ch, &evt
	s.mu.Unlock()
	defer s.finishTurn(session, turn)
	defer s.showWorking(ch)()
	t := s.loadTurnTiming()
	turn.timing = &t
	s.mu.Lock()
	close(turn.ready)
	s.mu.Unlock()
	ended, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-quit:
			s.mu.Lock()
			if turn.settleLocked(turnDisconnected) {
				close(turn.done)
				cut = true
			}
			s.mu.Unlock()
		case <-ended:
		}
	}()
	_, _ = s.runRemote(sid, evt.Content, id, turn, nil)
	close(ended)
	<-watched
	return cut
}

// A turn's channel gets a typing pulse every workingEvery, which a Buzz
// client shows for 8 s; workingLongest bounds a turn that never ends.
const (
	workingEvery   = 3 * time.Second
	workingLongest = 30 * time.Minute
)

// working is one route's working indication.
type working struct {
	done, ended chan struct{}
	once        sync.Once
}

// stop ends the indication and returns once no further pulse is sent.
func (w *working) stop() {
	w.once.Do(func() { close(w.done) })
	<-w.ended
}

// showWorking shows the agent as working in route ch until stop is called,
// the route's final text is about to be posted (stopWorking), or
// workingLongest passes. A pulse that fails is not sent again: the
// indication is advisory, and the first failure of the process is said
// once.
func (s *Server) showWorking(ch string) (stop func()) {
	w := &working{done: make(chan struct{}), ended: make(chan struct{})}
	s.working.Store(ch, w)
	go func() {
		defer close(w.ended)
		defer s.working.CompareAndDelete(ch, w)
		tick := time.NewTicker(workingEvery)
		defer tick.Stop()
		longest := time.NewTimer(workingLongest)
		defer longest.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), workingEvery)
			err := s.direct.Working(ctx, ch)
			cancel()
			if err != nil {
				s.workingFailed.Do(func() {
					fmt.Fprintf(os.Stderr, "amq-acp: the working indication is not shown in %s: %v\n", ch, err)
				})
			}
			select {
			case <-w.done:
				return
			case <-longest.C:
				return
			case <-tick.C:
			}
		}
	}()
	return w.stop
}

// stopWorking ends the working indication of route ch, if one runs, so
// that no pulse follows the route's final text.
func (s *Server) stopWorking(ch string) {
	if w, ok := s.working.Load(ch); ok {
		w.(*working).stop()
	}
}
