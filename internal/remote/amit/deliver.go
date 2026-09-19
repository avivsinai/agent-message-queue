package amit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// deliverPath returns the delivery inbox the adapter writes submits into:
// <root>/agents/<extension>/extensions/remote-inbox. The Amit-side
// amq-bridge extension polls this directory, delivers each submit through
// pi's sendUserMessage, and appends the correlated user_message entry to
// the event log. The adapter writes only here — the event log is
// extension-owned and never written by this package.
func deliverPath(root, extension string) string {
	return filepath.Join(root, "agents", extension, "extensions", "remote-inbox")
}

// deliver writes one submit into the extension inbox as a JSON file named
// by the request ref. The write is create-new: an existing file with the
// same ref means the request was already delivered (the endpoint retries
// with the same key only on uncertain records; a fresh send would double-
// fire the prompt). O_EXCL makes the duplicate a positive refusal.
func (a *Attachment) deliver(req core.BoundRequest) (err error) {
	dir := a.deliverDir
	if dir == "" {
		return fmt.Errorf("amit: no delivery inbox configured")
	}
	ref := clientRef(req.Key)
	path := filepath.Join(dir, refSanitize(ref)+".json")
	payload, err := json.Marshal(deliverRequest{
		Ref:       ref,
		Text:      req.Input.Text,
		DeliverAs: string(req.Input.Deliver),
		Busy:      string(req.Input.Busy),
		CreatedAt: protocol.FormatTime(a.now()),
	})
	if err != nil {
		// Marshal failure is pre-send: the text never left. Refusal.
		return fmt.Errorf("%w: marshal submit: %v", ErrNotSent, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: prepare inbox: %v", ErrNotSent, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Already delivered under this ref: positive, pre-send refusal
			// to double-fire.
			return fmt.Errorf("%w: submit %s already delivered", ErrNotSent, ref)
		}
		return fmt.Errorf("%w: write inbox: %v", ErrNotSent, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("%w: close inbox: %v", ErrNotSent, cerr)
		}
	}()
	if _, err := f.Write(payload); err != nil {
		return fmt.Errorf("%w: write inbox: %v", ErrNotSent, err)
	}
	return err
}

// deliverRequest is the JSON contract between this adapter and the
// Amit-side amq-bridge extension.
type deliverRequest struct {
	Ref       string `json:"ref"`
	Text      string `json:"text"`
	DeliverAs string `json:"deliver_as,omitempty"`
	Busy      string `json:"busy,omitempty"`
	CreatedAt string `json:"created_at"`
}

// refSanitize makes a request ref safe as a filename. protocol.EncodeRef
// output is base32 lowercase + a fixed prefix; only the prefix's colon
// needs replacing.
func refSanitize(ref string) string {
	out := make([]rune, 0, len(ref))
	for _, r := range ref {
		if r == '/' || r == ':' || r == '\\' {
			out = append(out, '_')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
