package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// A machine agent is the server in machine mode on a relay transport. The
// relay lets the agent choose each channel id and key each write, so one
// intention is one signed event, and lets it read the relay-signed state
// of a channel. Only an observation of that state proves a channel usable;
// an acknowledged write never does.

// Transport is the relay transport of a standalone machine agent.
// *buzzdirect.Port satisfies it.
type Transport interface {
	CreateChannel(context.Context, buzzop.ChannelSpec) buzzop.Created
	AddMember(ctx context.Context, key buzzop.Key, channel, pubkey, role string) buzzop.Result
	SetArchived(ctx context.Context, key buzzop.Key, channel string, archived bool) buzzop.Result
	Send(context.Context, buzzop.Message) buzzop.Result
	Edit(ctx context.Context, key buzzop.Key, channel, eventID, content string) buzzop.Result
	Reactions(ctx context.Context, eventID string) ([]buzzop.Reaction, error)
	ChannelFacts(ctx context.Context, channel string) (buzzop.ChannelFacts, error)
	Replay(ctx context.Context, key buzzop.Key) buzzop.Result
	Lookup(ctx context.Context, eventID string) (bool, error)
	Working(ctx context.Context, channel string) error
}

// Standalone is what a standalone machine agent runs with.
type Standalone struct {
	// Identity is the agent's public identity; Agent, Owner, RelayURL and
	// RelayKey must be set.
	Identity buzzop.Identity
	// Transport must not be nil.
	Transport Transport
	// Online reports whether Transport has a relay connection; nil means
	// it is not known.
	Online func() bool
}

// NewStandalone builds a machine agent's server on sa.Transport. Its state
// directory is BindingStateDir, beside the binding file. The marker it
// writes records this process as the worker.
func NewStandalone(sa Standalone, version string) *Server {
	s := NewServer(Config{RemoteBinding: true, Machine: true, Identity: sa.Identity}, version)
	s.buzz = directOps{s: s, t: sa.Transport}
	s.posts = boundedPosts{s: s, t: sa.Transport}
	s.direct, s.online = sa.Transport, sa.Online
	workerIdentity.self, _ = launch.HolderOf(os.Getpid())
	return s
}

// StartMachine takes the lead of the machine agent's identity
// (_machine/<agent>.lock) and starts its machine, dialog, mirror and sweep
// workers. ok is false, and nothing starts, while another process leads.
// stop ends the workers and releases every lock they took. Call it again
// only after stop.
func (s *Server) StartMachine() (stop func(), ok bool) {
	if s.id.Agent == "" || !s.lead() {
		return nil, false
	}
	return s.startLateReplySweep(), true
}

// Routes are the channel ids of this agent's active channel records on its
// relay, in name order.
func (s *Server) Routes() []string {
	var out []string
	for _, r := range s.activeRecords() {
		if routable(r, s.id) {
			out = append(out, r.ChannelID)
		}
	}
	return out
}

// bound is ctx with the server's PostTimeout when it has no deadline: a
// relay round trip otherwise waits only on ctx or the connection.
func (s *Server) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.cfg.PostTimeout)
}

// boundedPosts are posts on the transport, each bounded by bound.
type boundedPosts struct {
	s *Server
	t Transport
}

func (b boundedPosts) Send(ctx context.Context, m buzzop.Message) buzzop.Result {
	ctx, cancel := b.s.bound(ctx)
	defer cancel()
	return b.t.Send(ctx, m)
}

func (b boundedPosts) Edit(ctx context.Context, key buzzop.Key, channel, eventID, content string) buzzop.Result {
	ctx, cancel := b.s.bound(ctx)
	defer cancel()
	return b.t.Edit(ctx, key, channel, eventID, content)
}

func (b boundedPosts) Reactions(ctx context.Context, eventID string) ([]buzzop.Reaction, error) {
	ctx, cancel := b.s.bound(ctx)
	defer cancel()
	return b.t.Reactions(ctx, eventID)
}

// lifecycle are the machine lifecycle's Buzz operations.
type lifecycle interface {
	CreateChannel(context.Context, buzzop.ChannelSpec) buzzop.Created
	AddMember(ctx context.Context, key buzzop.Key, channel, pubkey, role string) buzzop.Result
	SetArchived(ctx context.Context, key buzzop.Key, channel string, archived bool) buzzop.Result
	Send(context.Context, buzzop.Message) buzzop.Result
}

// directOps are the lifecycle operations on the transport. Each is bounded
// by PostTimeout and is not sent once ctx, the machine worker's, has ended.
type directOps struct {
	s *Server
	t Transport
}

var stoppedResult = buzzop.Result{Outcome: buzzop.NotSent, Reason: errMachineStopped.Error()}

func (d directOps) CreateChannel(ctx context.Context, spec buzzop.ChannelSpec) buzzop.Created {
	if ctx.Err() != nil {
		return buzzop.Created{Result: stoppedResult}
	}
	ctx, cancel := d.s.bound(ctx)
	defer cancel()
	return d.t.CreateChannel(ctx, spec)
}

func (d directOps) AddMember(ctx context.Context, key buzzop.Key, channel, pubkey, role string) buzzop.Result {
	if ctx.Err() != nil {
		return stoppedResult
	}
	ctx, cancel := d.s.bound(ctx)
	defer cancel()
	return d.t.AddMember(ctx, key, channel, pubkey, role)
}

func (d directOps) SetArchived(ctx context.Context, key buzzop.Key, channel string, archived bool) buzzop.Result {
	if ctx.Err() != nil {
		return stoppedResult
	}
	ctx, cancel := d.s.bound(ctx)
	defer cancel()
	return d.t.SetArchived(ctx, key, channel, archived)
}

func (d directOps) Send(ctx context.Context, m buzzop.Message) buzzop.Result {
	if ctx.Err() != nil {
		return stoppedResult
	}
	ctx, cancel := d.s.bound(ctx)
	defer cancel()
	return d.t.Send(ctx, m)
}

// offline reports whether the transport is known to have no relay
// connection, so no step can be sent or observed.
func (s *Server) offline() bool { return s.online != nil && !s.online() }

// Lines a record shows while the relay's state does not prove a step.
const (
	waitingForState = "waiting for the relay's channel state"
	unreadableState = "could not read the relay's channel state"
)

// newChannelID is a channel id for a create: a lowercase UUIDv4, the form
// of a Buzz channel id.
func newChannelID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// observe reads the relay-signed state of channel. An error proves nothing.
func (s *Server) observe(channel string) (buzzop.ChannelFacts, error) {
	ctx := s.lifeCtx()
	if ctx.Err() != nil {
		return buzzop.ChannelFacts{}, errMachineStopped
	}
	ctx, cancel := s.bound(ctx)
	defer cancel()
	return s.direct.ChannelFacts(ctx, channel)
}

// ownerSeated reports whether f shows the owner as an admin or the owner.
func ownerSeated(f buzzop.ChannelFacts, id buzzop.Identity) bool {
	role := f.Roles[id.Owner]
	return role == "admin" || role == "owner"
}

// routeReady reports whether f shows a channel a route may use: private,
// the agent its owner, the owner an admin or owner, and not archived.
func routeReady(f buzzop.ChannelFacts, id buzzop.Identity) bool {
	return f.Private == buzzop.TriYes && f.Roles[id.Agent] == "owner" && ownerSeated(f, id) && f.Archived == buzzop.TriNo
}

// findChosen finds the channel a create of r made: the record's chosen
// channel when its relay-signed state names this agent as the creator,
// "" when it does not. ok is false when the state could not be read,
// which proves nothing.
func (s *Server) findChosen(r channelRecord) (id string, ok bool) {
	if r.Chosen == "" {
		return "", true
	}
	f, err := s.observe(r.Chosen)
	if err != nil {
		return "", false
	}
	if f.Creator == s.id.Agent {
		return r.Chosen, true
	}
	return "", true
}

// lookupChosen resolves an uncertain create of r. The observation comes
// first: the chosen channel, when the relay shows it,
// is adopted. Otherwise the create is made again under the same key: the
// transport sends the event it stored for that key, or signs the first one
// when none was prepared, so a registration never has two creates. A
// refusal fails r only when no event was signed or the relay does not hold
// it; an accepted create waits for the observation; any other outcome
// proves nothing and waits for the next pass.
func (s *Server) lookupChosen(r *channelRecord) {
	id, ok := s.findChosen(*r)
	switch {
	case !ok:
		r.LastError = unreadableState
		return
	case id != "":
		r.ChannelID, r.State, r.LastError = id, channelCreating, ""
		return
	}
	res := s.buzz.CreateChannel(s.lifeCtx(), s.createSpec(*r)).Result
	switch res.Outcome {
	case buzzop.Accepted:
		r.LastError = waitingForState
	case buzzop.Rejected:
		if res.EventID == "" {
			r.State, r.LastError = channelFailed, res.Reason
			return
		}
		ctx := s.lifeCtx()
		if ctx.Err() != nil {
			return
		}
		ctx, cancel := s.bound(ctx)
		defer cancel()
		found, err := s.direct.Lookup(ctx, res.EventID)
		switch {
		case err != nil:
			r.LastError = res.Reason
		case found:
			r.LastError = waitingForState
		default:
			r.State, r.LastError = channelFailed, res.Reason
		}
	default:
		r.LastError = res.Reason
	}
}

// noteKey names one post that is sent at most once: a fresh random key,
// minted before the send and never reused.
func noteKey() buzzop.Key { return buzzop.Key("note/" + rand.Text()) }

// finalKey names the final post of eventID, so a resend is the same signed
// event; noteKey when there is no event id.
func finalKey(eventID string) buzzop.Key {
	if eventID == "" {
		return noteKey()
	}
	return buzzop.Key("final/" + eventID)
}
