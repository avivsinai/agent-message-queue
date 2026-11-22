package linkio

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// grantedCommandAhead bounds a granted command's not_after: at most this far
// ahead of the machine's clock.
const grantedCommandAhead = 3 * time.Minute

// noticeTitle is the title of the owner's desktop notices.
const noticeTitle = "AMQ Remote"

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
		c.endGeneration(m.Binding)
		c.enqueue(sess, f, "grant:"+m.Binding, func() (any, error) { return c.revokeGrant(m.Binding) })
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
	c.gmu.Lock()
	defer c.gmu.Unlock()
	gen, err := c.cfg.Grants.Generation(binding)
	if err == nil && c.deadGeneration(gen) {
		// Revoked here while the new value was not yet written.
		gen, err = c.cfg.Grants.Mint(binding)
	}
	c.refreshGrantsLocked()
	if err != nil {
		return nil, err
	}
	info.Available, info.GrantGeneration = true, gen
	info.MaxLifetimeS, info.MaxTextBytes = int64(GrantMaxLifetime/time.Second), maxText
	return GrantInfoReply{GrantInfo: info}, nil
}

// grantAvailable reports whether b's session takes a grant now. Only a
// share whose consent is passkey does.
func (c *Carrier) grantAvailable(b Binding) (bool, string, int) {
	switch {
	case b.Consent != "passkey":
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
// again is answered unchanged, with no write and no notice.
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
	if c.deadGeneration(st.Generation) {
		st.Generation, st.Stored = "", nil
	}
	sg, err := VerifySignedGrant(view, m, st, c.cfg.Now())
	if err != nil {
		return nil, err
	}
	reply := GrantReply{Grant: GrantAccepted{Digest: sg.Digest, GrantID: sg.GrantID, Binding: sg.Binding, NotAfter: sg.NotAfter}}
	if st.Stored != nil && st.Stored.Digest == sg.Digest {
		return reply, nil
	}
	if err := c.cfg.Grants.Store(sg); err != nil {
		if errors.Is(err, ErrGrantGeneration) {
			return nil, refuse(CodeGrantRevoked, "binding %q has another grant generation now; ask grant_info and sign again", sg.Binding)
		}
		return nil, err
	}
	if old := st.Stored; old != nil {
		// Replaced by the newer grant: it ends with no notice of its own.
		delete(c.grantSeen, old.Digest)
		c.grantEnded[old.Digest] = CodeGrantRevoked
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
	view := c.view()
	now := c.cfg.Now()
	c.gmu.Lock()
	var rec GrantRecord
	var sg *StoredGrant
	for _, r := range c.refreshGrantsLocked() {
		if r.Grant != nil && r.Grant.Digest == digest && r.Grant.TargetID == cmd.TargetID {
			rec, sg = r, r.Grant
			break
		}
	}
	ended := c.grantEnded[digest]
	c.gmu.Unlock()
	if sg == nil {
		if ended != "" {
			return nil, refuse(ended, "the grant ended; ask grant_info and sign again")
		}
		return nil, refuse(CodeGrantUnknown, "no grant with this digest is stored for target %s", cmd.TargetID)
	}
	grantEnd, err := time.Parse(time.RFC3339, sg.NotAfter)
	switch {
	case rec.Generation != sg.Generation || c.deadGeneration(sg.Generation):
		return nil, refuse(CodeGrantRevoked, "binding %q has another grant generation now; ask grant_info and sign again", sg.Binding)
	case err != nil || !now.Before(grantEnd):
		return nil, refuse(CodeGrantExpired, "the grant expired at %s", sg.NotAfter)
	}
	if _, ok := view.Keys[sg.CredentialID]; !ok {
		return nil, refuse(CodeGrantRevoked, "the key that signed the grant is no longer accepted")
	}
	b, ok := view.Bindings[sg.Binding]
	switch {
	case !ok:
		return nil, refuse(string(protocol.CodeUnshared), "binding %q is not shared with this link", sg.Binding)
	case b.TargetID != sg.TargetID || b.Epoch != sg.Epoch || b.Labels != sg.Labels || cmd.Epoch != sg.Epoch:
		return nil, refuse(string(protocol.CodeStaleEpoch), "the binding changed since the grant was signed; read it again")
	case b.NativeSessionID != sg.NativeSessionID:
		return nil, refuse(CodeSessionChanged, "the session behind %s changed since the grant was signed", sg.Binding)
	case b.Consent != "passkey":
		return nil, refuse(CodeGrantUnsupported, "%s: binding %q takes no grant: its consent is %q", GrantReasonConsentLocal, sg.Binding, b.Consent)
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
	case cmdEnd.After(now.Add(grantedCommandAhead)) || cmdEnd.After(grantEnd):
		return nil, refuse(CodeConsentInvalid, "a granted task's not_after is at most %s ahead and within the grant", grantedCommandAhead)
	}
	available, reason, maxText := c.grantAvailable(b)
	switch {
	case !available:
		return nil, refuse(CodeGrantNeedsSignature, "%s: the session does not ask for permissions now; sign this task", reason)
	case len(in.Text) > maxText:
		return nil, refuse(CodeGrantNeedsSignature, "the text is %d bytes, more than the %d a grant carries; sign this task", len(in.Text), maxText)
	}
	src := c.source(view, sg.NativeSessionID, sg.CredentialID)
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

// revokeGrant writes the binding's new generation after the reader already
// stopped its grant, and answers it. The write takes the store's own lock;
// a granted task checked meanwhile is refused by the dead generation.
func (c *Carrier) revokeGrant(binding string) (any, error) {
	gen, err := c.cfg.Grants.Mint(binding)
	c.RefreshGrants()
	if err != nil {
		return nil, err
	}
	return RevokedReply{Revoked: GrantRevoked{Binding: binding, GrantGeneration: gen}}, nil
}

// endGeneration stops the binding's grant at once: its generation joins the
// dead set, copy-on-write, with no I/O. Generations are never reused, so a
// later read of the store never makes it live again.
func (c *Carrier) endGeneration(binding string) {
	g := c.grants.Load()
	if g == nil {
		return
	}
	gen := (*g)[binding].Generation
	if gen == "" {
		return
	}
	c.deadMu.Lock()
	defer c.deadMu.Unlock()
	next := map[string]bool{gen: true}
	if old := c.deadGens.Load(); old != nil {
		for k := range *old {
			next[k] = true
		}
	}
	c.deadGens.Store(&next)
}

func (c *Carrier) deadGeneration(gen string) bool {
	dead := c.deadGens.Load()
	return dead != nil && (*dead)[gen]
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
// An unreadable store is no grant. A grant live at the last read that is no
// longer live has ended: revoked, replaced or expired.
func (c *Carrier) refreshGrantsLocked() Grants {
	g, err := c.cfg.Grants.Load()
	if err != nil {
		if msg := err.Error(); msg != c.grantsErr {
			c.grantsErr = msg
			c.cfg.Logf("link %s: read grants: %v", c.cfg.Name, err)
		}
		g = Grants{}
	} else {
		c.grantsErr = ""
	}
	c.grants.Store(&g)
	now := c.cfg.Now()
	live := map[string]StoredGrant{}
	for _, r := range g {
		if sg := r.Grant; sg != nil && sg.Generation == r.Generation && !c.deadGeneration(sg.Generation) && !grantExpired(sg, now) {
			live[sg.Digest] = *sg
		}
	}
	for d, sg := range c.grantSeen {
		if _, ok := live[d]; ok {
			continue
		}
		code, why := CodeGrantRevoked, "revoked"
		if grantExpired(&sg, now) {
			code, why = CodeGrantExpired, "expired"
		}
		delete(c.grantSeen, d)
		c.grantEnded[d] = code
		c.notify(fmt.Sprintf("the grant of link %s for session %s ended (%s)", c.cfg.Name, sg.Labels.Session, why))
	}
	for d, sg := range live {
		if _, ok := c.grantSeen[d]; !ok {
			c.grantSeen[d] = sg
		}
	}
	return g
}

func grantExpired(sg *StoredGrant, now time.Time) bool {
	end, err := time.Parse(time.RFC3339, sg.NotAfter)
	return err != nil || !now.Before(end)
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
// dead generations and the live consent keys.
func (c *Carrier) GrantLive(targetID, digest string) (binding, reason string) {
	g := c.grants.Load()
	if g == nil {
		return "", CodeGrantRevoked + ": this link holds no grant"
	}
	for _, r := range *g {
		sg := r.Grant
		if sg == nil || sg.Digest != digest || sg.TargetID != targetID {
			continue
		}
		switch {
		case r.Generation != sg.Generation || c.deadGeneration(sg.Generation):
			return sg.Binding, fmt.Sprintf("%s: binding %q has another grant generation now", CodeGrantRevoked, sg.Binding)
		case !c.ConsentLive(sg.CredentialID):
			return sg.Binding, CodeGrantRevoked + ": the key that signed the grant is no longer accepted"
		case grantExpired(sg, c.cfg.Now()):
			return sg.Binding, fmt.Sprintf("%s: the grant expired at %s", CodeGrantExpired, sg.NotAfter)
		}
		return sg.Binding, ""
	}
	return "", CodeGrantRevoked + ": the grant is no longer stored"
}
