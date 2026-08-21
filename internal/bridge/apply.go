package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// ApplyResult is the durable local outcome of one envelope.
type ApplyResult struct {
	Path     string
	Replayed bool
}

// ApplyEnvelope commits the payload into the local agent's inbox under a
// stable transfer filename keyed by (source_host, transfer_id). The same
// digest is idempotent; a different digest for that key is a conflict.
func ApplyEnvelope(root *fsq.DeliveryRoot, localHost, localAgent string, env Envelope) (ApplyResult, error) {
	if err := ValidateEnvelope(env); err != nil {
		return ApplyResult{}, err
	}
	destHost, destAgent, err := ParseAlias(env.DestAlias)
	if err != nil {
		return ApplyResult{}, err
	}
	if destHost != localHost {
		return ApplyResult{}, fmt.Errorf("bridge dest_alias host %q is not local host %q", destHost, localHost)
	}
	if destAgent != localAgent {
		return ApplyResult{}, fmt.Errorf("bridge dest_alias agent %q is not local agent %q", destAgent, localAgent)
	}
	filename := TransferFilename(env.SourceHost, env.TransferID)
	rel := filepath.Join("agents", localAgent, "inbox", "new", filename)
	_, existedErr := root.Stat(rel)
	path, err := fsq.DeliverToExistingInbox(root, localAgent, filename, env.Payload)
	if err == nil {
		return ApplyResult{Path: path, Replayed: existedErr == nil}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return ApplyResult{}, err
	}
	existing, readErr := root.ReadRegularNoFollow(rel)
	if readErr != nil {
		return ApplyResult{}, err
	}
	if string(existing) == string(env.Payload) {
		return ApplyResult{Path: root.DisplayPath(rel), Replayed: true}, nil
	}
	return ApplyResult{}, err
}
