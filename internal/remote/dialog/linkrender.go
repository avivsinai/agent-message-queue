package dialog

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// The publication bounds of a link dialog: the prompt and every option
// label must carry whole within them, and the dialog id within 128 bytes.
const (
	LinkMaxPromptBytes = 16384
	LinkMaxOptionBytes = 256
	LinkMaxIDBytes     = 128
)

// RenderLinkInteraction renders one read dialog record as its interaction,
// without the answer state. It is the one render the publication and the
// answer write share, so a record whose text or options changed since
// publication renders a different dialog and the write's digest check
// refuses it. ok=false when the record offers no legal link shape: the
// role scan, the label distinctness, and the prompt, option and id bounds.
func RenderLinkInteraction(req Request) (*protocol.Interaction, bool) {
	var (
		allow, deny Option
		haveAllow   bool
		haveDeny    bool
	)
	for _, o := range req.Options {
		if o.Scope != ScopeOnce {
			continue
		}
		if o.Deny {
			if haveDeny {
				return nil, false
			}
			deny, haveDeny = o, true
			continue
		}
		if haveAllow {
			return nil, false
		}
		allow, haveAllow = o, true
	}
	if !haveAllow && !haveDeny {
		return nil, false // nothing the link may choose once
	}
	if haveAllow && haveDeny && allow.Label == deny.Label {
		return nil, false // roles must stay distinguishable
	}
	prompt := req.Tool + "\n" + req.Text
	if len(prompt) > LinkMaxPromptBytes || len(req.ID) > LinkMaxIDBytes {
		return nil, false
	}
	in := &protocol.Interaction{
		InteractionID: req.ID,
		Kind:          "approval",
		Prompt:        prompt,
		Options:       []string{},
	}
	for _, o := range req.Options {
		if (o.Deny && o == deny) || (!o.Deny && o == allow) {
			if o.Label == "" || len(o.Label) > LinkMaxOptionBytes {
				return nil, false
			}
			in.Options = append(in.Options, o.Label)
		}
	}
	if haveAllow {
		in.ApproveOption = allow.Label
	}
	if haveDeny {
		in.RejectOption = deny.Label
	}
	if len(in.Options) == 0 {
		return nil, false
	}
	return in, true
}

// InteractionDigest is the value a server signs for one interaction:
// "sha256:" + hex of the JCS bytes of the identity keys — exactly
// interaction_id, kind, options, prompt and, when present, approve_option
// and reject_option. The remote_answer flag is part of the revision,
// never of the digest: an answer file appearing must not make signed
// documents stale. Publication, admission and the answer write all
// compute it through this one function.
func InteractionDigest(in *protocol.Interaction) (string, error) {
	cp := *in
	cp.RemoteAnswer = false
	canon, err := jcs.Marshal(cp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
