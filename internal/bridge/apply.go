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
	// DurabilityIndeterminate marks a CommittedDurabilityError path: the
	// payload IS published at Path (rename succeeded or matching destination
	// bytes were proven), but the directory-sync guarantee could not be
	// verified. The caller records the distinct published_durability_unknown
	// state — never committed-on-faith and never a retryable rejection
	// (review-827-r2 P0, tightened per review-827-r3 P1).
	DurabilityIndeterminate bool
}

// ErrCollisionUnproven marks an os.ErrExist whose existing bytes could not
// be read: the transfer may or may not conflict. The owning layer records
// uncertain for this class — never a terminal conflict and never a retryable
// non-delivery (codex r2-r2 finding 2).
var ErrCollisionUnproven = errors.New("transfer collision unproven")

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
	if err != nil {
		var committed *fsq.CommittedDurabilityError
		if errors.As(err, &committed) {
			// review-827-r2 P0 (tightened per review-827-r3 P1): the
			// publication SUCCEEDED (rename done, or matching destination
			// bytes proven) but the destination directory sync did not
			// complete. Durability is INDETERMINATE — the repo-wide
			// CommittedDurabilityError contract treats this as delivered for
			// non-ledgered senders, but the ledgered bridge must not record
			// durable success on faith. Surface the path so the ledger
			// records published_durability_unknown: never a retryable
			// rejection that would re-apply and duplicate, and never a
			// committed record whose receipt/ACK flow before the carrier's
			// durability is verified.
			return ApplyResult{Path: committed.FinalPath, DurabilityIndeterminate: true}, nil
		}
	}
	if err == nil {
		return ApplyResult{Path: path, Replayed: existedErr == nil}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return ApplyResult{}, err
	}
	existing, readErr := root.ReadRegularNoFollow(rel)
	if readErr != nil {
		// review-827-r2 (codex r2-r2 finding 2): a collision whose existing
		// bytes cannot be read is post-publication ambiguity, not a proven
		// conflict. Distinguish it so the owning layer can record uncertain
		// instead of terminal conflict.
		return ApplyResult{}, fmt.Errorf("%w (collision bytes unreadable: %w)", ErrCollisionUnproven, readErr)
	}
	if string(existing) == string(env.Payload) {
		return ApplyResult{Path: root.DisplayPath(rel), Replayed: true}, nil
	}
	return ApplyResult{}, err
}
