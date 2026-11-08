package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/route"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// Follow scopes: the header decides which roots a message can have reached.
const (
	followScopeRoot    = "root"    // no route fields from a session root: delivered in the sender's root
	followScopeProject = "project" // reply_to, or a send from the base root: the base and its sessions
	followScopePeers   = "peers"   // reply_project: every peer base of the sender's .amqrc and its sessions
)

// Searched-root statuses. A peer root that does not exist cannot hold the
// message; a root that cannot be read might, so it makes a result unknown.
const (
	followRootRead       = "read"
	followRootMissing    = string(route.PeerMissingRoot)
	followRootUnreadable = string(route.PeerUnreadable)
)

// followedMessage is what a follow needs from a message header.
type followedMessage struct {
	ID           string
	From         string
	Kind         string
	To           []string
	ReplyTo      string
	ReplyProject string
	// Root is the queue root that holds the sender's outbox copy.
	Root string
}

type traceFollow struct {
	MessageID  string            `json:"message_id"`
	Scope      string            `json:"scope,omitempty"`
	Status     string            `json:"status"` // found, partial, ambiguous, not_found, unknown
	Detail     string            `json:"detail,omitempty"`
	Searched   []followRoot      `json:"searched"`
	Recipients []followRecipient `json:"recipients"`
}

type followRoot struct {
	Root    string `json:"root"`
	Project string `json:"project,omitempty"`
	Session string `json:"session"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// followRecipient is where one recipient's copy is. Delivery is new or cur
// for the inbox copy, and dlq when the consumer wrote (or owes) a dlq
// receipt: DLQ files are named by their envelope, not the message id.
// Delivery is not_found when no searched root holds it and unknown when a
// root that might hold it could not be read. Receipt is drained, dlq, owed
// (consumed, receipt not written yet) or none. Session is that of the root
// that holds the receipt.
type followRecipient struct {
	Handle   string `json:"handle"`
	Root     string `json:"root,omitempty"`
	Project  string `json:"project,omitempty"`
	Session  string `json:"session"`
	Delivery string `json:"delivery"`
	Receipt  string `json:"receipt"`
	// Reply is set when the message is a request (review_request, question
	// or todo).
	Reply *followReply `json:"reply,omitempty"`
}

// followReply says whether a reply to a request came back. Status is
// replied when a message whose refs name the request is in the recipient's
// outbox or, failing that, in the requester's inbox at the destination;
// none when neither holds one; unknown when the route back or a mailbox
// cannot be read, with Error no_return_peer, unreachable,
// malformed_reply_metadata or unreadable. The destination is where amq reply
// from the recipient's root delivers.
type followReply struct {
	Status      string               `json:"status"`
	Error       string               `json:"error,omitempty"`
	Detail      string               `json:"detail,omitempty"`
	Destination *followDestination   `json:"destination,omitempty"`
	Replies     []followReplyMessage `json:"replies"`
}

type followDestination struct {
	Root    string `json:"root"`
	Session string `json:"session"`
	Handle  string `json:"handle"`
}

// followReplyMessage is one reply and its copy in the requester's mailbox,
// in the terms of followRecipient.
type followReplyMessage struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Kind     string `json:"kind"`
	Delivery string `json:"delivery"`
	Receipt  string `json:"receipt"`
}

// follower follows messages across roots. It caches the candidate roots of
// each sender root and the headers of each mailbox it reads, so one process
// follows many messages with one walk and one read per mailbox.
type follower struct {
	walker     route.Walker
	candidates map[string]followCandidates
	headers    map[string][]format.Header
}

type followCandidates struct {
	scope  string
	roots  []followRoot
	detail string
}

func newFollower() *follower {
	return &follower{walker: routeWalker(), candidates: map[string]followCandidates{}, headers: map[string][]format.Header{}}
}

// follow locates msg's copy and receipt for each recipient in the roots its
// header implies. It stats a few fixed names per root and recipient and
// reads no message file.
func (f *follower) follow(msg followedMessage) traceFollow {
	out := traceFollow{MessageID: msg.ID, Searched: []followRoot{}, Recipients: []followRecipient{}}
	cands := f.candidateRoots(msg)
	out.Scope, out.Detail = cands.scope, cands.detail
	out.Searched = append(out.Searched, cands.roots...)
	if cands.roots == nil {
		out.Status = "unknown"
		return out
	}
	located, unknown, ambiguous := 0, 0, false
	seen := map[string]bool{}
	for _, handle := range msg.To {
		if seen[handle] {
			continue
		}
		seen[handle] = true
		hits, unreadable := f.locateAll(out.Searched, handle, msg.ID)
		switch {
		case len(hits) > 0:
			if slices.Contains(thread.OpenerKinds, msg.Kind) {
				for i := range hits {
					hits[i].Reply = f.followReply(msg, hits[i])
				}
			}
			located++
			ambiguous = ambiguous || len(hits) > 1
			out.Recipients = append(out.Recipients, hits...)
		case unreadable:
			unknown++
			out.Recipients = append(out.Recipients, followRecipient{Handle: handle, Delivery: "unknown", Receipt: "none"})
		default:
			out.Recipients = append(out.Recipients, followRecipient{Handle: handle, Delivery: "not_found", Receipt: "none"})
		}
	}
	switch {
	case ambiguous:
		out.Status = "ambiguous"
	case located > 0 && located == len(seen):
		out.Status = "found"
	case located > 0:
		out.Status = "partial"
	case unknown > 0:
		out.Status = "unknown"
	default:
		out.Status = "not_found"
	}
	return out
}

// locateAll finds handle's copy of id in each readable root. A root whose
// stat fails is marked unreadable in place.
func (f *follower) locateAll(roots []followRoot, handle, id string) ([]followRecipient, bool) {
	var hits []followRecipient
	unreadable := false
	for i := range roots {
		if roots[i].Status == followRootMissing {
			continue
		}
		if roots[i].Status == followRootUnreadable {
			unreadable = true
			continue
		}
		delivery, stage, err := locateCopy(roots[i].Root, handle, id)
		if err != nil {
			roots[i].Status, roots[i].Detail = followRootUnreadable, err.Error()
			unreadable = true
			continue
		}
		if delivery == "" && stage == "none" {
			continue
		}
		hits = append(hits, followRecipient{
			Handle: handle, Root: roots[i].Root, Project: roots[i].Project, Session: roots[i].Session,
			Delivery: delivery, Receipt: stage,
		})
	}
	return hits, unreadable
}

// locateCopy stats handle's inbox copy of id and its written or owed receipt
// in root. Both empty ("", "none") means root holds no trace of it.
func locateCopy(root, handle, id string) (delivery, stage string, err error) {
	if err := fsq.ValidateHandle(handle); err != nil {
		return "", "", err
	}
	if _, box, err := fsq.FindMessage(root, handle, id+".md"); err == nil {
		delivery = box
	} else if !os.IsNotExist(err) {
		return "", "", err
	}
	stage = "none"
	owedDLQ := false
	for _, s := range []string{receipt.StageDrained, receipt.StageDLQ} {
		written := filepath.Join(fsq.AgentReceipts(root, handle), receipt.Filename(id, handle, s))
		owed := filepath.Join(root, receipt.PendingPath(receipt.Receipt{MsgID: id, Consumer: handle, Stage: s}))
		if ok, err := statExists(written); err != nil {
			return "", "", err
		} else if ok {
			stage = s
			break
		}
		if ok, err := statExists(owed); err != nil {
			return "", "", err
		} else if ok {
			stage, owedDLQ = "owed", s == receipt.StageDLQ
			break
		}
	}
	if delivery == "" && (stage == receipt.StageDLQ || owedDLQ) {
		delivery = "dlq"
	}
	return delivery, stage, nil
}

func (f *follower) followReply(msg followedMessage, hit followRecipient) *followReply {
	out := &followReply{Status: "unknown", Replies: []followReplyMessage{}}
	dest, code, err := f.replyDestination(msg, hit)
	if err != nil {
		out.Error, out.Detail = code, err.Error()
		return out
	}
	out.Destination = dest
	found, err := f.repliesIn(msg.ID, filepath.Join(hit.Root, "agents", hit.Handle, "outbox", "sent"))
	if err == nil && len(found) == 0 {
		inbox := filepath.Join(dest.Root, "agents", dest.Handle, "inbox")
		found, err = f.repliesIn(msg.ID, filepath.Join(inbox, "new"), filepath.Join(inbox, "cur"))
	}
	if err != nil {
		out.Error, out.Detail = followRootUnreadable, err.Error()
		return out
	}
	out.Status = "none"
	if len(found) > 0 {
		out.Status = "replied"
	}
	for _, h := range found {
		delivery, stage, err := locateCopy(dest.Root, dest.Handle, h.ID)
		switch {
		case err != nil:
			delivery, stage, out.Detail = "unknown", "none", err.Error()
		case delivery == "" && stage == "none":
			delivery = "not_found"
		}
		out.Replies = append(out.Replies, followReplyMessage{ID: h.ID, From: h.From, Kind: h.Kind, Delivery: delivery, Receipt: stage})
	}
	return out
}

// replyDestination applies amq reply's route from the recipient's root: a
// request with neither reply_to nor reply_project is answered in that root.
func (f *follower) replyDestination(msg followedMessage, hit followRecipient) (*followDestination, string, error) {
	if msg.ReplyProject == "" && msg.ReplyTo == "" {
		return &followDestination{Root: hit.Root, Session: hit.Session, Handle: msg.From}, "", nil
	}
	_, session, err := route.ParseReplyTo(msg.ReplyTo, msg.ReplyProject != "")
	if err != nil {
		return nil, "malformed_reply_metadata", err
	}
	root, handle, err := f.walker.ResolveReplyRoute(hit.Root, msg.ReplyProject, msg.ReplyTo)
	switch {
	case errors.Is(err, route.ErrPeerRootUnreachable):
		return nil, "unreachable", err
	case err != nil:
		return nil, "no_return_peer", err
	}
	return &followDestination{Root: root, Session: session, Handle: handle}, "", nil
}

// repliesIn returns the headers in dirs whose refs name id.
func (f *follower) repliesIn(id string, dirs ...string) ([]format.Header, error) {
	var found []format.Header
	for _, dir := range dirs {
		headers, err := f.mailboxHeaders(dir)
		if err != nil {
			return nil, err
		}
		for _, h := range headers {
			if h.ID != id && slices.Contains(h.Refs, id) {
				found = append(found, h)
			}
		}
	}
	return found, nil
}

// mailboxHeaders reads the parsable headers in one mailbox directory once.
// A missing directory holds none; a file that does not parse is no reply.
func (f *follower) mailboxHeaders(dir string) ([]format.Header, error) {
	if headers, ok := f.headers[dir]; ok {
		return headers, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var headers []format.Header
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		if h, err := format.ReadHeaderFile(filepath.Join(dir, name)); err == nil {
			headers = append(headers, h)
		}
	}
	f.headers[dir] = headers
	return headers, nil
}

func statExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, err
	}
}

// candidateRoots are the roots msg can have reached, by its header: reply
// project names a peer project, reply_to another session of this project,
// and neither the sender's own root (or, from the base, a session of it).
// Roots are nil when they cannot be known.
func (f *follower) candidateRoots(msg followedMessage) followCandidates {
	source := threadOpenScanRoot(absPath(msg.Root))
	scope := followScopeRoot
	switch {
	case msg.ReplyProject != "":
		scope = followScopePeers
	case msg.ReplyTo != "" || source.Session == "":
		scope = followScopeProject
	}
	key := scope + "\x00" + source.Root
	if cached, ok := f.candidates[key]; ok {
		cached.roots = append([]followRoot(nil), cached.roots...)
		return cached
	}
	cands := followCandidates{scope: scope}
	switch scope {
	case followScopeRoot:
		cands.roots = []followRoot{{Root: source.Root, Session: source.Session, Status: followRootRead}}
	case followScopeProject:
		cands.roots = projectRoots(absPath(baseRootOf(source.Root)), "")
	case followScopePeers:
		graph, err := f.walker.Peers(source.Root, 1)
		if err != nil {
			cands.detail = fmt.Sprintf("peer projects of %s are unknown: %v", source.Root, err)
			break
		}
		cands.roots = []followRoot{}
		for _, edge := range graph.Peers {
			switch edge.Status {
			case route.PeerMissingRoot, route.PeerUnreadable:
				cands.roots = append(cands.roots, followRoot{Root: edge.BaseRoot, Project: edge.Name, Status: string(edge.Status), Detail: edge.Detail})
			default:
				cands.roots = append(cands.roots, projectRoots(edge.BaseRoot, edge.Name)...)
			}
		}
	}
	f.candidates[key] = cands
	cands.roots = append([]followRoot(nil), cands.roots...)
	return cands
}

// projectRoots are base and its canonical sessions.
func projectRoots(base, project string) []followRoot {
	roots := []followRoot{{Root: base, Project: project, Status: followRootRead}}
	sessions, err := canonicalSessionRoots(base)
	switch {
	case os.IsNotExist(err):
		roots[0].Status, roots[0].Detail = followRootMissing, err.Error()
	case err != nil:
		roots[0].Status, roots[0].Detail = followRootUnreadable, err.Error()
	}
	for _, s := range sessions {
		roots = append(roots, followRoot{Root: s.Root, Project: project, Session: s.Session, Status: followRootRead})
	}
	return roots
}

func writeFollowText(follow traceFollow) error {
	line := fmt.Sprintf("  %-12s %s", "follow", follow.Status)
	if follow.Scope != "" {
		line += " [" + follow.Scope + "]"
	}
	if follow.Detail != "" {
		line += ": " + follow.Detail
	}
	if err := writeStdoutLine(line); err != nil {
		return err
	}
	for _, r := range follow.Recipients {
		where := "no searched root"
		if r.Root != "" {
			where = followWhere(r.Project, r.Session, r.Root)
		}
		if err := writeStdout("    - %s in %s: delivery %s, receipt %s\n", r.Handle, where, r.Delivery, r.Receipt); err != nil {
			return err
		}
		if r.Reply == nil {
			continue
		}
		line := "      reply " + r.Reply.Status
		if r.Reply.Error != "" {
			line += " (" + r.Reply.Error + ": " + r.Reply.Detail + ")"
		}
		if d := r.Reply.Destination; d != nil {
			line += fmt.Sprintf(" to %s in %s", d.Handle, followWhere("", d.Session, d.Root))
		}
		if err := writeStdoutLine(line); err != nil {
			return err
		}
		for _, reply := range r.Reply.Replies {
			if err := writeStdout("      - %s %s from %s: delivery %s, receipt %s\n", reply.Kind, reply.ID, reply.From, reply.Delivery, reply.Receipt); err != nil {
				return err
			}
		}
	}
	for _, root := range follow.Searched {
		if root.Status == followRootRead {
			continue
		}
		if err := writeStdout("    - searched %s: %s (%s)\n", root.Root, root.Status, root.Detail); err != nil {
			return err
		}
	}
	return nil
}

func followWhere(project, session, root string) string {
	parts := []string{}
	if project != "" {
		parts = append(parts, "project "+project)
	}
	parts = append(parts, sessionLabel(session))
	return strings.Join(parts, ", ") + " (" + root + ")"
}
