package linkio

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// grantedCommandAhead bounds a granted command's not_after: at most this far
// ahead of the machine's clock.
const grantedCommandAhead = 3 * time.Minute

// noticeTitle is the title of the owner's desktop notices.
const noticeTitle = "AMQ Remote"

// grantKeysUnread is why no grant admits a task while the link's key list
// cannot be read. Such a read ends no grant.
const grantKeysUnread = "the link's key list cannot be read now"

// handleGrantRequest answers the four grant requests. grant_revoke takes
// effect here on the reader, before the next frame is read; every disk read
// and write runs on the dispatcher. grant_info, signed_grant and grant_revoke
// queue per binding, granted_submit per request id, so a cancel of a granted
// request runs after its submit.
func (c *Carrier) handleGrantRequest(sess *session, f Frame) {
	switch bodySchema(f.Body) {
	case SchemaGrantInfo:
		var m GrantInfoRequest
		if err := decodeStrict(f.Body, SchemaGrantInfo, &m); err != nil || m.Binding == "" {
			c.refuse(sess, f.ID, ErrorBody{Code: string(protocol.CodeInvalid), Message: "grant_info needs one binding"})
			return
		}
		c.enqueue(sess, f, "grant:"+m.Binding, func() (any, error) { return c.grantInfo(m.Binding) })
	case SchemaSignedGrant:
		var m SignedGrant
		if err := decodeStrict(f.Body, SchemaSignedGrant, &m); err != nil {
			c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: err.Error()})
			return
		}
		// The queue key is the binding, read before verification. It orders
		// the work and nothing else; verification decides.
		binding := signedGrantBinding(m)
		if binding == "" {
			c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: "the signed grant names no binding"})
			return
		}
		c.enqueue(sess, f, "grant:"+binding, func() (any, error) { return c.acceptGrant(m, binding) })
	case SchemaGrantedSubmit:
		var m GrantedSubmit
		if err := decodeStrict(f.Body, SchemaGrantedSubmit, &m); err != nil {
			c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: err.Error()})
			return
		}
		cmd, err := protocol.DecodeCommand(m.Command)
		switch {
		case err != nil:
			c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: "the command is not valid: " + err.Error()})
			return
		case cmd.Op != protocol.OpRequestSubmit:
			c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: "a grant carries only request.submit"})
			return
		}
		c.enqueue(sess, f, cmd.RequestID, func() (any, error) { return c.admitGranted(m.GrantDigest, cmd) })
	case SchemaGrantRevoke:
		var m GrantRevokeRequest
		if err := decodeStrict(f.Body, SchemaGrantRevoke, &m); err != nil || m.Binding == "" {
			c.refuse(sess, f.ID, ErrorBody{Code: string(protocol.CodeInvalid), Message: "grant_revoke needs one binding"})
			return
		}
		if shared := c.shared.Load(); shared == nil || !(*shared)[m.Binding] {
			c.refuse(sess, f.ID, ErrorBody{Code: string(protocol.CodeUnshared), Message: fmt.Sprintf("binding %q is not shared with this link", m.Binding)})
			return
		}
		// The grant stops counting now, before any later frame is read; the
		// new generation is written on the dispatcher, then the reply.
		mark := c.markBindingRevoked(m.Binding)
		c.enqueue(sess, f, "grant:"+m.Binding, func() (any, error) { return c.revokeGrant(m.Binding, mark) })
	}
}

// signedGrantBinding reads the binding a signed grant names, unverified.
func signedGrantBinding(m SignedGrant) string {
	var d struct {
		Binding string `json:"binding"`
	}
	doc, err := b64.DecodeString(m.DocumentB64)
	if err != nil || json.Unmarshal(doc, &d) != nil {
		return ""
	}
	return d.Binding
}

// grantInfo answers grant_info: whether the binding takes a grant now, and
// if it does, its generation and the limits.
func (c *Carrier) grantInfo(binding string) (any, error) {
	view := c.view()
	b, ok := view.Bindings[binding]
	if !ok {
		return nil, refuse(string(protocol.CodeUnshared), "binding %q is not shared with this link", binding)
	}
	info := GrantInfo{Binding: binding, Now: c.cfg.Now().UTC().Format(time.RFC3339)}
	available, reason, maxText := c.grantAvailable(b)
	if !available {
		info.Reason = reason
		return GrantInfoReply{GrantInfo: info}, nil
	}
	if mark, revoked := c.bindingRevoked(binding); revoked {
		// Revoked here while the new value was not yet written.
		if _, err := c.mintRevoked(binding, mark); err != nil {
			return nil, err
		}
	}
	c.gmu.Lock()
	defer c.gmu.Unlock()
	gen, err := c.cfg.Grants.Generation(binding)
	c.refreshGrantsLocked()
	if err != nil {
		return nil, err
	}
	info.Available, info.GrantGeneration = true, gen
	info.MaxLifetimeS, info.MaxTextBytes = int64(GrantMaxLifetime/time.Second), maxText
	return GrantInfoReply{GrantInfo: info}, nil
}

// grantUnsupported reports whether a reason grant_info gives ends a grant
// rather than asking for a signature on one task: a share that takes no
// grant, or a session that can write the grant state with no dialog.
func grantUnsupported(reason string) bool {
	return reason == GrantReasonConsentLocal || reason == GrantReasonStateInWorkspace
}

// TakesGrant reports whether a share with this consent takes grants: only
// passkey consent does.
func TakesGrant(consent string) bool { return consent == "passkey" }

// grantAvailable reports whether b's session takes a grant now.
func (c *Carrier) grantAvailable(b Binding) (bool, string, int) {
	switch {
	case !TakesGrant(b.Consent):
		return false, GrantReasonConsentLocal, 0
	case c.cfg.GrantInfoFor == nil:
		return false, GrantReasonHarnessUnsupported, 0
	}
	available, reason, maxText := c.cfg.GrantInfoFor(b)
	if !available && reason == "" {
		reason = GrantReasonModeUnreadable
	}
	return available, reason, maxText
}

// acceptGrant verifies a signed grant against the binding's state read from
// the store now, stores it, and tells the owner. The stored grant presented
// again is answered unchanged, with no write and no notice, unless a check
// of this run saw it end: a grant that ended stays refused, in this run by
// the ended set and after it by the store's issued floor.
func (c *Carrier) acceptGrant(m SignedGrant, binding string) (any, error) {
	view := c.view()
	var st GrantState
	if b, ok := view.Bindings[binding]; ok {
		st.Available, st.Reason, _ = c.grantAvailable(b)
	}
	c.gmu.Lock()
	defer c.gmu.Unlock()
	rec := c.refreshGrantsLocked()[binding]
	st.Generation, st.Stored = rec.Generation, rec.Grant
	if _, revoked := c.bindingRevoked(binding); revoked {
		st.Generation, st.Stored = "", nil
	}
	sg, err := VerifySignedGrant(view, m, st, c.cfg.Now())
	if err != nil {
		return nil, err
	}
	if e, ended := c.endedGrant(sg.Digest); ended {
		// A grant this run saw end is never accepted again, whatever the
		// store holds: with how it ended, or grant_stale when another grant
		// is stored for the binding.
		if st.Stored != nil && st.Stored.Digest != sg.Digest {
			return nil, refuse(CodeGrantStale, "binding %q holds another grant; sign again", sg.Binding)
		}
		return nil, refuse(e.code, "%s; ask grant_info and sign again", e.why)
	}
	reply := GrantReply{Grant: GrantAccepted{Digest: sg.Digest, GrantID: sg.GrantID, Binding: sg.Binding, NotAfter: sg.NotAfter}}
	if st.Stored != nil && st.Stored.Digest == sg.Digest {
		return reply, nil
	}
	if err := c.cfg.Grants.Store(sg); err != nil {
		switch {
		case errors.Is(err, ErrGrantGeneration):
			return nil, refuse(CodeGrantRevoked, "binding %q has another grant generation now; ask grant_info and sign again", sg.Binding)
		case errors.Is(err, ErrGrantNotNewer):
			return nil, refuse(CodeGrantStale, "binding %q: %v; sign a grant issued later, or revoke the binding's grant", sg.Binding, err)
		}
		return nil, err
	}
	if old := st.Stored; old != nil {
		// Replaced by the newer grant: it ends with no notice of its own.
		delete(c.grantSeen, old.Digest)
		c.markEnded(old.Digest, CodeGrantRevoked, "a newer grant replaced it")
	}
	c.refreshGrantsLocked()
	until := sg.NotAfter
	if t, err := time.Parse(time.RFC3339, sg.NotAfter); err == nil {
		until = t.Local().Format("15:04")
	}
	c.notify(fmt.Sprintf("link %s may send tasks to session %s until %s", c.cfg.Name, sg.Labels.Session, until))
	return reply, nil
}

// admitGranted runs a granted task: the checks of the stored grant, the
// binding now and the command, then the endpoint, with the grant's native
// session, signing key and digest on the source. It never holds a task for
// a local confirmation.
func (c *Carrier) admitGranted(digest string, cmd *protocol.Command) (any, error) {
	if c.cfg.Handle == nil {
		return nil, refuse(string(protocol.CodeUnsupported), "this endpoint runs no commands")
	}
	now := c.cfg.Now()
	c.gmu.Lock()
	rec, sg := findGrant(c.refreshGrantsLocked(), cmd.TargetID, digest)
	c.gmu.Unlock()
	// The view is read after the grant, so its key list is never older than
	// the one the grant was accepted under, and the grant is compared with
	// this request's own read of the key list.
	view := c.view()
	if sg == nil {
		return nil, c.noGrant(view, cmd.TargetID, digest)
	}
	if code, why := c.grantEnd(rec, sg, now, viewKeyList(view)); code != "" {
		return nil, refuse(code, "%s; ask grant_info and sign again", why)
	}
	until, _ := time.Parse(time.RFC3339, sg.NotAfter) // grantEnd parsed it
	b, err := matchBinding(view, sg.Binding, sg.TargetID, sg.Epoch, sg.Labels, sg.NativeSessionID)
	switch {
	case err != nil:
		var refusal *Refusal
		if errors.As(err, &refusal) && (refusal.Code == string(protocol.CodeStaleEpoch) || refusal.Code == CodeSessionChanged) {
			// Only the binding check may end a session grant: an absent
			// binding (unshared) has no such evidence. No endpoint lock is
			// held here; gmu orders refresh's exact-digest clear with tick reads.
			c.markEnded(digest, CodeGrantRevoked, sessionGrantChangedReason(sg.Binding))
			c.gmu.Lock()
			c.refreshGrantsLocked()
			c.gmu.Unlock()
		}
		return nil, err
	case cmd.Epoch != sg.Epoch:
		return nil, refuse(string(protocol.CodeStaleEpoch), "the binding changed since you signed; read it again and sign again")
	}
	// The share's consent and the harness's own state decide before the
	// command's bounds; the mode the session asks in is read once, here.
	available, reason, maxText := c.grantAvailable(b)
	if !available && grantUnsupported(reason) {
		return nil, refuse(CodeGrantUnsupported, "%s: binding %q takes no grant now", reason, sg.Binding)
	}
	in := cmd.Input
	if in.Busy != protocol.BusyReject || in.Deliver != protocol.DeliverTurn || in.MinEvidence != sg.MinEvidence {
		return nil, refuse(CodeConsentInvalid, "a granted task has the grant's input class: busy reject, deliver turn, min_evidence %s", sg.MinEvidence)
	}
	cmdEnd, err := time.Parse(time.RFC3339, cmd.NotAfter)
	switch {
	case err != nil:
		return nil, refuse(CodeConsentInvalid, "not_after is not RFC 3339")
	case !now.Before(cmdEnd):
		return nil, refuse(string(protocol.CodeExpired), "the task expired at %s", cmd.NotAfter)
	case cmdEnd.After(now.Add(grantedCommandAhead)) || cmdEnd.After(until):
		return nil, refuse(CodeConsentInvalid, "a granted task's not_after is at most %s ahead and within the grant", grantedCommandAhead)
	}
	switch {
	case !available:
		return nil, refuse(CodeGrantNeedsSignature, "%s: the session does not ask for permissions now; sign this task", reason)
	case len(in.Text) > maxText:
		return nil, refuse(CodeGrantNeedsSignature, "the text is %d bytes, more than the %d a grant carries; sign this task", len(in.Text), maxText)
	}
	src := c.source(view, sg.NativeSessionID, sg.CredentialID, sg.Binding)
	src.GrantDigest = sg.Digest
	out, err := c.cfg.Handle(cmd, src)
	if err != nil {
		return nil, err
	}
	reply, ok := out.(protocol.Reply)
	if !ok {
		return nil, errors.New("the endpoint returned no submit reply")
	}
	return outcomeReply{Outcome: reply.Outcome}, nil
}

// noGrant is the refusal of a granted task whose grant is not stored: how
// the grant ended when a check of this run saw it end, grant_revoked when
// the server revoked the binding of its target in this run, else
// grant_unknown.
func (c *Carrier) noGrant(view *ConsentView, targetID, digest string) error {
	if e, ended := c.endedGrant(digest); ended {
		return refuse(e.code, "%s; ask grant_info and sign again", e.why)
	}
	for name, b := range view.Bindings {
		if _, revoked := c.bindingRevoked(name); revoked && b.TargetID == targetID {
			return refuse(CodeGrantRevoked, "binding %q has another grant generation now; ask grant_info and sign again", name)
		}
	}
	return refuse(CodeGrantUnknown, "no grant with this digest is stored for target %s", targetID)
}

// revokeGrant writes the binding's new generation after the reader already
// stopped its grant, and answers it. The write takes the store's own lock.
// When it fails the answer is storage_full and the binding stays stopped:
// the server sends the revoke again, and the carrier's tick writes the
// generation until a write succeeds.
func (c *Carrier) revokeGrant(binding string, mark uint64) (any, error) {
	gen, err := c.mintRevoked(binding, mark)
	if err != nil {
		return nil, refuse(string(protocol.CodeStorageFull), "the new grant generation of binding %q is not written: %v; send grant_revoke again", binding, err)
	}
	return RevokedReply{Revoked: GrantRevoked{Binding: binding, GrantGeneration: gen}}, nil
}

// markBindingRevoked stops the binding's grant at once, with no I/O: the
// binding joins the revoked set, copy-on-write, under a new mark, which it
// returns.
func (c *Carrier) markBindingRevoked(binding string) uint64 {
	c.revokedMu.Lock()
	defer c.revokedMu.Unlock()
	next := map[string]uint64{}
	if old := c.revokedBindings.Load(); old != nil {
		next = maps.Clone(*old)
	}
	c.revokeMark++
	next[binding] = c.revokeMark
	c.revokedBindings.Store(&next)
	return c.revokeMark
}

// bindingRevoked reports whether the server revoked the binding's grant in
// this run and this process has not yet written its new generation, and the
// revoke's mark.
func (c *Carrier) bindingRevoked(binding string) (uint64, bool) {
	revoked := c.revokedBindings.Load()
	if revoked == nil {
		return 0, false
	}
	mark, ok := (*revoked)[binding]
	return mark, ok
}

// mintRevoked writes a new generation for a binding revoked under mark and
// answers the generation the store holds after it. It holds the binding's
// mint lock throughout, so no other mint of the binding runs meanwhile, and
// mints only while the binding is in the revoked set: a binding left the
// set only after a mint that came after its revoke. It reads the store into
// the snapshot, and only then lets the binding leave the set, unless a
// later revoke marked it again.
func (c *Carrier) mintRevoked(binding string, mark uint64) (string, error) {
	mu := c.mintLock(binding)
	mu.Lock()
	defer mu.Unlock()
	var minted string
	var err error
	if _, revoked := c.bindingRevoked(binding); revoked {
		minted, err = c.cfg.Grants.Mint(binding)
	}
	c.gmu.Lock()
	defer c.gmu.Unlock()
	c.logStore("write the new grant generation of "+binding, err)
	if err != nil {
		return "", err
	}
	gen := c.refreshGrantsLocked()[binding].Generation
	if gen == "" {
		gen = minted // written, but the store cannot be read back now
	}
	if gen == "" {
		return "", fmt.Errorf("the grant generation of binding %q cannot be read now", binding)
	}
	c.revokedMu.Lock()
	defer c.revokedMu.Unlock()
	if old := c.revokedBindings.Load(); old != nil && (*old)[binding] == mark {
		next := maps.Clone(*old)
		delete(next, binding)
		c.revokedBindings.Store(&next)
	}
	return gen, nil
}

// mintLock is the lock around the generation mints of one binding.
func (c *Carrier) mintLock(binding string) *sync.Mutex {
	c.mintLocksMu.Lock()
	defer c.mintLocksMu.Unlock()
	mu, ok := c.mintLocks[binding]
	if !ok {
		mu = &sync.Mutex{}
		c.mintLocks[binding] = mu
	}
	return mu
}

// writeRevokedGenerations writes the new generation of each binding still
// in the revoked set: a revoke whose write failed, or whose job has not run
// yet. The carrier's tick runs it, so a failed write is tried again at every
// tick until it succeeds.
func (c *Carrier) writeRevokedGenerations() {
	revoked := c.revokedBindings.Load()
	if c.cfg.Grants == nil || revoked == nil {
		return
	}
	for binding, mark := range *revoked {
		_, _ = c.mintRevoked(binding, mark)
	}
}

// logStore logs a failed store write once per distinct error, by what it
// wrote, and forgets the error once that write succeeds.
func (c *Carrier) logStore(what string, err error) {
	if err == nil {
		delete(c.storeErr, what)
		return
	}
	if msg := err.Error(); msg != c.storeErr[what] {
		c.storeErr[what] = msg
		c.cfg.Logf("link %s: %s: %v", c.cfg.Name, what, err)
	}
}

// RefreshGrants reads the grant records into the snapshot GrantLive reads,
// and tells the owner about every grant that ended since the last read.
func (c *Carrier) RefreshGrants() {
	if c.cfg.Grants == nil {
		return
	}
	c.gmu.Lock()
	defer c.gmu.Unlock()
	c.refreshGrantsLocked()
}

// refreshGrantsLocked reads the store, stores the snapshot and returns it.
// An unreadable store admits no grant and ends none. A grant first read now
// starts its remaining life on the monotonic clock. A stored grant that has
// ended (grantEndSeen: it expired, its key list or generation is not the
// link's, or a check of this run saw it end) is cleared from the store,
// with the binding's generation kept; a failed clear is logged once per
// distinct error and tried again at the next read. A grant live at the
// last read that is no longer live has ended, and the owner is told:
// revoked, replaced or expired. A key list that could not be read ends no
// grant.
func (c *Carrier) refreshGrantsLocked() Grants {
	g, ok := c.loadGrantsLocked()
	if !ok {
		c.grants.Store(&g)
		return g
	}
	now, mono := c.cfg.Now(), c.cfg.Mono()
	var known map[string]time.Duration
	if p := c.grantLife.Load(); p != nil {
		known = *p
	}
	life := map[string]time.Duration{}
	for _, r := range g {
		sg := r.Grant
		if sg == nil {
			continue
		}
		if deadline, ok := known[sg.Digest]; ok {
			life[sg.Digest] = deadline
		} else if end, err := time.Parse(time.RFC3339, sg.NotAfter); err == nil {
			life[sg.Digest] = mono + end.Sub(now)
		}
	}
	c.grantLife.Store(&life)
	kl := c.keyListNow()
	cleared := false
	for binding, r := range g {
		if sg := r.Grant; sg != nil {
			if code, _ := c.grantEndSeen(r, sg, now, kl); code != "" {
				err := c.cfg.Grants.Clear(binding, sg.Digest)
				c.logStore("clear the ended grant of "+binding, err)
				cleared = cleared || err == nil
			}
		}
	}
	if cleared {
		if g, ok = c.loadGrantsLocked(); !ok {
			c.grants.Store(&g)
			return g
		}
	}
	c.grants.Store(&g)
	live := map[string]StoredGrant{}
	for _, r := range g {
		if sg := r.Grant; sg != nil {
			if code, _ := c.grantEndSeen(r, sg, now, kl); code == "" {
				live[sg.Digest] = *sg
			}
		}
	}
	for d, sg := range c.grantSeen {
		if _, ok := live[d]; ok {
			continue
		}
		e, ended := c.endedGrant(d)
		if !ended {
			// It left the store with no check seeing it end: the owner
			// revoked it, or it expired.
			e = grantEndRec{code: CodeGrantRevoked, why: "the grant is no longer stored"}
			if c.grantExpired(&sg, now) {
				e = grantEndRec{code: CodeGrantExpired, why: "the grant expired at " + sg.NotAfter}
			}
			c.markEnded(d, e.code, e.why)
		}
		why := "revoked"
		if e.code == CodeGrantExpired {
			why = "expired"
		} else if e.why == sessionGrantChangedReason(sg.Binding) {
			why = "session changed"
		}
		delete(c.grantSeen, d)
		c.notify(fmt.Sprintf("the grant of link %s for session %s ended (%s)", c.cfg.Name, sg.Labels.Session, why))
	}
	for d, sg := range live {
		if _, ok := c.grantSeen[d]; !ok {
			c.grantSeen[d] = sg
		}
	}
	return g
}

// loadGrantsLocked reads the store; an unreadable store is no grant and not
// ok, and its error is logged once.
func (c *Carrier) loadGrantsLocked() (Grants, bool) {
	g, err := c.cfg.Grants.Load()
	if err != nil {
		if msg := err.Error(); msg != c.grantsErr {
			c.grantsErr = msg
			c.cfg.Logf("link %s: read grants: %v", c.cfg.Name, err)
		}
		return Grants{}, false
	}
	c.grantsErr = ""
	return g, true
}

// findGrant returns the record whose stored grant has digest and is for
// targetID, or a nil grant.
func findGrant(g Grants, targetID, digest string) (GrantRecord, *StoredGrant) {
	for _, r := range g {
		if sg := r.Grant; sg != nil && sg.Digest == digest && sg.TargetID == targetID {
			return r, sg
		}
	}
	return GrantRecord{}, nil
}

// grantEnd is "" while the grant sg stored in record r still admits a task
// under the key list kl: no end is known (grantEndSeen), and kl is the
// latest read of the key list. Else it is the grant code and why. It reads
// only snapshots and does no I/O.
func (c *Carrier) grantEnd(r GrantRecord, sg *StoredGrant, now time.Time, kl keyListNow) (code, why string) {
	if code, why = c.grantEndSeen(r, sg, now, kl); code == "" && !kl.current {
		return CodeGrantRevoked, grantKeysUnread
	}
	return code, why
}

// grantEndSeen is the end of a grant: the end a check of this run already
// saw, or else the end grantEndKnown finds now, which it records, so the
// grant stays ended for the rest of the run whatever the files say later.
// It does no I/O.
func (c *Carrier) grantEndSeen(r GrantRecord, sg *StoredGrant, now time.Time, kl keyListNow) (code, why string) {
	if e, ended := c.endedGrant(sg.Digest); ended {
		return e.code, e.why
	}
	if code, why = c.grantEndKnown(r, sg, now, kl); code != "" {
		c.markEnded(sg.Digest, code, why)
	}
	return code, why
}

// grantEndKnown is the end of a grant known to have ended: its generation is
// not the record's or the server revoked its binding in this run, it
// expired, or the key list ended it (keyListEnded). A key list that could
// not be read leaves the grant unadmitted (grantEnd) but not ended.
func (c *Carrier) grantEndKnown(r GrantRecord, sg *StoredGrant, now time.Time, kl keyListNow) (code, why string) {
	_, revoked := c.bindingRevoked(sg.Binding)
	switch {
	case r.Generation != sg.Generation || revoked:
		return CodeGrantRevoked, fmt.Sprintf("binding %q has another grant generation now", sg.Binding)
	case c.grantExpired(sg, now):
		return CodeGrantExpired, "the grant expired at " + sg.NotAfter
	case keyListEnded(kl, sg):
		return CodeGrantRevoked, core.GrantKeysGone
	}
	return "", ""
}

// keyListNow is the key list a check compares a grant with: list is the
// value of the last successful read, known is false before any, and
// current is false when the latest read failed.
type keyListNow struct {
	list           string
	known, current bool
}

// keyListEnded reports whether the key list ended sg: the list was read and
// is not the one sg was accepted under. A list that could not be read ends
// nothing.
func keyListEnded(kl keyListNow, sg *StoredGrant) bool { return kl.known && kl.list != sg.KeyList }

// viewKeyList is the key list of a view, read for one request.
func viewKeyList(v *ConsentView) keyListNow {
	list, read := v.keyList()
	return keyListNow{list: list, known: read, current: read}
}

// grantEndRec is how a grant ended: its grant code and why.
type grantEndRec struct{ code, why string }

// endedGrant reports how the grant with digest ended, when a check of this
// run saw it end. It does no I/O.
func (c *Carrier) endedGrant(digest string) (grantEndRec, bool) {
	ended := c.ended.Load()
	if ended == nil {
		return grantEndRec{}, false
	}
	e, ok := (*ended)[digest]
	return e, ok
}

// markEnded records how the grant with digest ended, copy-on-write, with no
// I/O. The first end recorded stays.
func (c *Carrier) markEnded(digest, code, why string) {
	c.endedMu.Lock()
	defer c.endedMu.Unlock()
	next := map[string]grantEndRec{}
	if old := c.ended.Load(); old != nil {
		if _, ok := (*old)[digest]; ok {
			return
		}
		next = maps.Clone(*old)
	}
	next[digest] = grantEndRec{code: code, why: why}
	c.ended.Store(&next)
}

// sessionGrantChangedReason is the recorded reason of an end proven by
// matchBinding, and the same reason the owner notice identifies.
func sessionGrantChangedReason(binding string) string {
	return "the session behind binding " + binding + " changed"
}

// grantExpired reports whether sg has expired: the wall clock is at or
// after its not_after, or the life it had left when this run first read it
// is used up on the monotonic clock, so a wall clock set back extends no
// grant.
func (c *Carrier) grantExpired(sg *StoredGrant, now time.Time) bool {
	end, err := time.Parse(time.RFC3339, sg.NotAfter)
	if err != nil || !now.Before(end) {
		return true
	}
	life := c.grantLife.Load()
	if life == nil {
		return false
	}
	deadline, ok := (*life)[sg.Digest]
	return ok && c.cfg.Mono() >= deadline
}

// notify shows the owner a notice off every lock and off the reader.
func (c *Carrier) notify(body string) {
	if c.cfg.Notify != nil {
		go c.cfg.Notify(noticeTitle, body)
	}
}

// GrantLive is core's handoff check of a granted request: "" while the grant
// with that digest for that target still admits it, else the reason, which
// starts with the grant code. It also names the grant's binding. Core asks
// it under its own lock, so it reads only snapshots: the grant records, the
// remaining lives, the revoked bindings, the ended grants and the key list.
func (c *Carrier) GrantLive(targetID, digest string) (binding, reason string) {
	var r GrantRecord
	var sg *StoredGrant
	if g := c.grants.Load(); g != nil {
		r, sg = findGrant(*g, targetID, digest)
	}
	if sg == nil {
		if e, ended := c.endedGrant(digest); ended {
			return "", e.code + ": " + e.why
		}
		return "", CodeGrantRevoked + ": the grant is no longer stored"
	}
	if code, why := c.grantEnd(r, sg, c.cfg.Now(), c.keyListNow()); code != "" {
		return sg.Binding, code + ": " + why
	}
	return sg.Binding, ""
}
