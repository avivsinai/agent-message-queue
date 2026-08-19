package fsq

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DLQSchemaVersion = "amq/dlq/v1"
	MaxRetries       = 3

	RetryStateReady         = "ready"
	RetryStatePending       = "pending"
	RetryStateDelivered     = "delivered"
	RetryStateIndeterminate = "indeterminate"
)

var (
	// ErrDLQRetryDelivered marks a terminal retry audit. The original delivery
	// may since have been consumed, so filesystem absence does not make this
	// envelope retryable again.
	ErrDLQRetryDelivered = errors.New("DLQ envelope retry already delivered")

	// ErrDLQRetryIndeterminate marks a crash-recovery state where the retry was
	// recorded as pending but its destination is no longer visible. AMQ cannot
	// safely distinguish a never-committed delivery from one already consumed.
	ErrDLQRetryIndeterminate = errors.New("DLQ envelope retry outcome is indeterminate")
)

// DLQEnvelope wraps a failed message with failure metadata.
type DLQEnvelope struct {
	Schema         string `json:"schema"`
	ID             string `json:"id"`
	OriginalID     string `json:"original_id"`
	OriginalFile   string `json:"original_file"`
	FailureReason  string `json:"failure_reason"`
	FailureDetail  string `json:"failure_detail"`
	FailureTime    string `json:"failure_time"`
	RetryCount     int    `json:"retry_count"`
	RetryState     string `json:"retry_state,omitempty"`
	RetryPending   bool   `json:"retry_pending,omitempty"`
	RetryDelivered bool   `json:"retry_delivered,omitempty"`
	SourceDir      string `json:"source_dir"`
}

// normalizeRetryState makes retry_state the durable authority and exposes the
// older boolean fields as a compatibility view. Released v1 envelopes did not
// have retry_state, and a positive count was persisted before delivery: it can
// mean either a successful delivery or a known pre-commit failure. Preserve
// that ambiguity explicitly until a visible destination proves delivery.
func normalizeRetryState(env *DLQEnvelope) error {
	if env == nil {
		return fmt.Errorf("nil DLQ envelope")
	}
	if env.RetryCount < 0 {
		return fmt.Errorf("retry_count must not be negative")
	}

	state := env.RetryState
	if state == "" {
		switch {
		case env.RetryPending && env.RetryDelivered:
			return fmt.Errorf("legacy retry markers are mutually exclusive")
		case env.RetryPending:
			state = RetryStatePending
		case env.RetryDelivered:
			state = RetryStateDelivered
		case env.RetryCount > 0:
			state = RetryStateIndeterminate
		default:
			state = RetryStateReady
		}
	} else {
		expectedPending, expectedDelivered := false, false
		switch state {
		case RetryStateReady:
		case RetryStatePending:
			expectedPending = true
		case RetryStateDelivered:
			expectedDelivered = true
		case RetryStateIndeterminate:
		default:
			return fmt.Errorf("unknown retry_state %q", state)
		}
		if state != RetryStateReady && env.RetryCount == 0 {
			return fmt.Errorf("retry_state %q requires retry_count > 0", state)
		}
		if env.RetryPending != expectedPending || env.RetryDelivered != expectedDelivered {
			return fmt.Errorf(
				"retry_state %q disagrees with retry_pending=%t retry_delivered=%t",
				state,
				env.RetryPending,
				env.RetryDelivered,
			)
		}
	}

	env.RetryState = state
	env.RetryPending = state == RetryStatePending
	env.RetryDelivered = state == RetryStateDelivered
	return nil
}

func setRetryState(env *DLQEnvelope, state string) {
	env.RetryState = state
	env.RetryPending = state == RetryStatePending
	env.RetryDelivered = state == RetryStateDelivered
}

var readRandom = rand.Read

// GenerateDLQID creates a unique ID for a DLQ envelope.
func GenerateDLQID() (string, error) {
	b := make([]byte, 6)
	if _, err := readRandom(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("dlq_%d_%d_%s", time.Now().UnixNano(), os.Getpid(), hex.EncodeToString(b)), nil
}

var removeDLQSource = func(root *DeliveryRoot, path string) error {
	return root.root.Remove(path)
}

// MoveToDLQ moves a failed message from inbox/new to dlq/new with envelope.
func MoveToDLQ(root *DeliveryRoot, agent, filename, originalID, failureReason, failureDetail string) (string, error) {
	claimErr := MoveNewToCur(root, agent, filename)
	var committedClaim *CommittedDurabilityError
	if claimErr != nil && !errors.As(claimErr, &committedClaim) {
		return "", fmt.Errorf("claim original: %w", claimErr)
	}
	return moveClaimedCurToDLQ(
		root,
		agent,
		BoxNew,
		filename,
		originalID,
		failureReason,
		failureDetail,
		claimErr,
	)
}

// MoveCurToDLQ moves an already-claimed inbox/cur message to dlq/new.
func MoveCurToDLQ(root *DeliveryRoot, agent, filename, originalID, failureReason, failureDetail string) (string, error) {
	return moveInboxMessageToDLQ(root, agent, BoxCur, BoxCur, filename, originalID, failureReason, failureDetail)
}

// MoveClaimedCurToDLQ moves an inbox/cur message to dlq/new while reconciling
// a durability-indeterminate claim that already made the message visible in
// cur. Once the source is removed, any committed error names the visible DLQ
// envelope rather than the no-longer-present cur artifact.
func MoveClaimedCurToDLQ(
	root *DeliveryRoot,
	agent, filename, originalID, failureReason, failureDetail string,
	claimErr error,
) (string, error) {
	if claimErr == nil {
		return MoveCurToDLQ(root, agent, filename, originalID, failureReason, failureDetail)
	}
	var committedClaim *CommittedDurabilityError
	if !errors.As(claimErr, &committedClaim) {
		return "", fmt.Errorf("claim original: %w", claimErr)
	}
	if err := ValidateHandle(agent); err != nil {
		return "", fmt.Errorf("claim recipient: %w", err)
	}
	if err := ValidateMessageFilename(filename); err != nil {
		return "", fmt.Errorf("claim filename: %w", err)
	}
	if err := root.VerifyBase(); err != nil {
		return "", err
	}
	wantFinalPath := root.displayPath(filepath.Join("agents", agent, "inbox", "cur", filename))
	if committedClaim.Recipient != agent ||
		filepath.Clean(committedClaim.FinalPath) != filepath.Clean(wantFinalPath) {
		return "", fmt.Errorf(
			"claim original provenance mismatch: got recipient %q at %q, want recipient %q at %q: %w",
			committedClaim.Recipient,
			committedClaim.FinalPath,
			agent,
			wantFinalPath,
			claimErr,
		)
	}
	return moveClaimedCurToDLQ(
		root,
		agent,
		BoxCur,
		filename,
		originalID,
		failureReason,
		failureDetail,
		claimErr,
	)
}

func moveClaimedCurToDLQ(
	root *DeliveryRoot,
	agent, envelopeSourceDir, filename, originalID, failureReason, failureDetail string,
	claimErr error,
) (string, error) {
	dlqPath, transitionErr := moveInboxMessageToDLQ(
		root,
		agent,
		BoxCur,
		envelopeSourceDir,
		filename,
		originalID,
		failureReason,
		failureDetail,
	)
	if claimErr == nil {
		return dlqPath, transitionErr
	}
	if transitionErr != nil {
		var partialTransition *DLQTransitionError
		var committedTransition *CommittedDurabilityError
		if errors.As(transitionErr, &partialTransition) ||
			dlqPath == "" ||
			!errors.As(transitionErr, &committedTransition) {
			return dlqPath, errors.Join(fmt.Errorf("claim original: %w", claimErr), transitionErr)
		}
	}

	// A committed claim is recoverable only after the terminal DLQ transition
	// and both sides of the original inbox rename are synced again. Attempt both
	// directories even when one fails so the returned state is maximally healed.
	syncErr := syncInboxClaimDirs(root, agent)
	if syncErr == nil {
		return dlqPath, nil
	}
	return dlqPath, &CommittedDurabilityError{
		FinalPath: dlqPath,
		Recipient: agent,
		Err: errors.Join(
			fmt.Errorf("claim original: %w", claimErr),
			transitionErr,
			syncErr,
		),
	}
}

func moveInboxMessageToDLQ(root *DeliveryRoot, agent, readDir, envelopeSourceDir, filename, originalID, failureReason, failureDetail string) (string, error) {
	if err := ValidateMessageFilename(filename); err != nil {
		return "", err
	}
	srcDir, err := inboxSourceDir(agent, readDir)
	if err != nil {
		return "", err
	}
	srcPath := filepath.Join(srcDir, filename)

	// Read original content
	content, err := root.ReadRegularNoFollow(srcPath)
	if err != nil {
		return "", fmt.Errorf("read original: %w", err)
	}

	dlqID, err := GenerateDLQID()
	if err != nil {
		return "", fmt.Errorf("generate dlq id: %w", err)
	}

	// Create envelope
	envelope := DLQEnvelope{
		Schema:        DLQSchemaVersion,
		ID:            dlqID,
		OriginalID:    originalID,
		OriginalFile:  filename,
		FailureReason: failureReason,
		FailureDetail: failureDetail,
		FailureTime:   time.Now().UTC().Format(time.RFC3339),
		RetryCount:    0,
		SourceDir:     envelopeSourceDir,
	}

	// Serialize envelope + original content
	data, err := serializeDLQMessage(envelope, content)
	if err != nil {
		return "", fmt.Errorf("serialize dlq: %w", err)
	}

	// Write to DLQ using atomic delivery (tmp -> new)
	dlqFilename := envelope.ID + ".md"
	dlqPath, err := deliverToDLQ(root, agent, dlqFilename, data)
	if err != nil {
		var committed *CommittedDurabilityError
		if errors.As(err, &committed) {
			sourcePath := root.displayPath(srcPath)
			return dlqPath, &DLQTransitionError{
				EnvelopePath:   dlqPath,
				SourcePath:     sourcePath,
				SourceRetained: true,
				Err:            err,
			}
		}
		return "", fmt.Errorf("deliver to dlq: %w", err)
	}

	sourcePath := root.displayPath(srcPath)
	if err := removeDLQSource(root, srcPath); err != nil && !os.IsNotExist(err) {
		return dlqPath, &DLQTransitionError{
			EnvelopePath:   dlqPath,
			SourcePath:     sourcePath,
			SourceRetained: true,
			Err:            fmt.Errorf("remove original: %w", err),
		}
	}
	if err := root.syncDir(srcDir); err != nil {
		return dlqPath, &CommittedDurabilityError{
			FinalPath: dlqPath,
			Recipient: agent,
			Err:       fmt.Errorf("sync removed source dir: %w", err),
		}
	}

	return dlqPath, nil
}

func syncInboxClaimDirs(root *DeliveryRoot, agent string) error {
	var syncErr error
	for _, dir := range []string{
		filepath.Join("agents", agent, "inbox", "new"),
		filepath.Join("agents", agent, "inbox", "cur"),
	} {
		if err := root.syncDir(dir); err != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("resync %s: %w", dir, err))
		}
	}
	return syncErr
}

func inboxSourceDir(agent, sourceDir string) (string, error) {
	switch sourceDir {
	case BoxNew:
		return filepath.Join("agents", agent, "inbox", "new"), nil
	case BoxCur:
		return filepath.Join("agents", agent, "inbox", "cur"), nil
	default:
		return "", fmt.Errorf("unsupported inbox source dir %q", sourceDir)
	}
}

// deliverToDLQ writes a DLQ message using Maildir semantics (tmp -> new).
func deliverToDLQ(root *DeliveryRoot, agent, filename string, data []byte) (string, error) {
	if err := root.VerifyBase(); err != nil {
		return "", err
	}
	tmpDir := filepath.Join("agents", agent, "dlq", "tmp")
	newDir := filepath.Join("agents", agent, "dlq", "new")

	if err := root.root.MkdirAll(tmpDir, 0o700); err != nil {
		return "", err
	}
	if err := root.root.MkdirAll(newDir, 0o700); err != nil {
		return "", err
	}

	tmpPath, err := uniqueAttemptTmpPath(tmpDir, filename)
	if err != nil {
		return "", err
	}
	newPath := filepath.Join(newDir, filename)

	if err := root.writeAndSync(tmpPath, data, 0o600); err != nil {
		return "", err
	}
	if err := root.syncDir(tmpDir); err != nil {
		return "", root.cleanupTemp(tmpPath, err)
	}
	if err := root.publishTmpNoReplace(tmpPath, newPath, data); err != nil {
		return "", err
	}
	committedPath := root.displayPath(newPath)
	if err := root.syncDir(newDir); err != nil {
		return committedPath, &CommittedDurabilityError{
			FinalPath: committedPath,
			Recipient: agent,
			Err:       fmt.Errorf("sync dlq new dir: %w", err),
		}
	}
	_ = root.syncDir(tmpDir)

	return committedPath, nil
}

// ReadDLQEnvelope reads and parses a DLQ message.
func ReadDLQEnvelope(root *DeliveryRoot, path string) (*DLQEnvelope, []byte, error) {
	data, err := root.ReadRegularNoFollow(path)
	if err != nil {
		return nil, nil, err
	}

	envelope, body, err := parseDLQMessage(data)
	if err != nil {
		return nil, nil, err
	}

	return envelope, body, nil
}

// ReadDLQEnvelopePath is the legacy pathname reader used only by non-mutating
// listing code. Mutating DLQ flows must use ReadDLQEnvelope with a capability.
func ReadDLQEnvelopePath(path string) (*DLQEnvelope, []byte, error) {
	data, err := ReadRegularNoFollow(path)
	if err != nil {
		return nil, nil, err
	}
	return parseDLQMessage(data)
}

// InspectDLQEnvelope reads one DLQ envelope and marks a new envelope inspected
// while holding the same per-envelope lock used by retry and purge. The
// returned envelope, body, and box therefore describe one serialized state.
// When the new-to-cur rename committed but directory durability is
// indeterminate, box is cur and err is a CommittedDurabilityError.
func InspectDLQEnvelope(root *DeliveryRoot, agent, filename string) (
	envelope *DLQEnvelope,
	originalContent []byte,
	box string,
	err error,
) {
	err = root.WithDLQEnvelopeLock(agent, filename, func(batch *DeliveryRoot) error {
		path, foundBox, findErr := FindDLQMessage(batch, agent, filename)
		if findErr != nil {
			return findErr
		}
		envelope, originalContent, findErr = ReadDLQEnvelope(batch, path)
		if findErr != nil {
			return fmt.Errorf("read DLQ message: %w", findErr)
		}
		box = foundBox
		if foundBox == BoxCur {
			_, reconcileErr := reconcileDLQCurAuthorityLocked(batch, agent, filename)
			return reconcileErr
		}
		moveErr := moveDLQNewToCurLocked(batch, agent, filename)
		var committed *CommittedDurabilityError
		if moveErr == nil || errors.As(moveErr, &committed) {
			box = BoxCur
		}
		return moveErr
	})
	return envelope, originalContent, box, err
}

// RetryFromDLQ moves a message from DLQ back to inbox/new for reprocessing.
// Returns error if retry_count >= MaxRetries and force is false.
func RetryFromDLQ(root *DeliveryRoot, agent, dlqFilename string, force bool) error {
	return root.WithDLQEnvelopeLock(agent, dlqFilename, func(batch *DeliveryRoot) error {
		return retryFromDLQLocked(batch, agent, dlqFilename, force)
	})
}

func retryFromDLQLocked(root *DeliveryRoot, agent, dlqFilename string, force bool) error {
	// Find in dlq/new or dlq/cur
	dlqPath, box, err := FindDLQMessage(root, agent, dlqFilename)
	if err != nil {
		return err
	}

	envelope, originalContent, err := ReadDLQEnvelope(root, dlqPath)
	if err != nil {
		return fmt.Errorf("read dlq envelope: %w", err)
	}
	if box == BoxCur {
		if _, err := reconcileDLQCurAuthorityLocked(root, agent, dlqFilename); err != nil {
			return err
		}
	}

	if err := ValidateMessageFilename(envelope.OriginalFile); err != nil {
		return fmt.Errorf("invalid original_file %q: %w", envelope.OriginalFile, err)
	}
	if envelope.RetryState == RetryStateDelivered {
		return fmt.Errorf(
			"%w: %s (retry already delivered)",
			ErrDLQRetryDelivered,
			envelope.OriginalFile,
		)
	}

	// Refuse every retained original before mutating the envelope. Checking cur
	// as well as new prevents a partial DLQ transition from being retried into a
	// duplicate inbox/new copy while the claimed source remains recoverable.
	originalPresentBox := ""
	for _, box := range []string{BoxNew, BoxCur} {
		path := filepath.Join("agents", agent, "inbox", box, envelope.OriginalFile)
		if _, err := root.Stat(path); err == nil {
			originalPresentBox = box
			break
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("stat inbox/%s original: %w", box, err)
		}
	}

	if envelope.RetryState == RetryStatePending || envelope.RetryState == RetryStateIndeterminate {
		if originalPresentBox == "" {
			recovered, recoverErr := recoverPendingInboxTmp(root, agent, envelope.OriginalFile, originalContent)
			if recoverErr != nil {
				return recoverErr
			}
			if !recovered {
				return fmt.Errorf(
					"%w: %s retry for %s has no visible inbox destination; do not retry blindly",
					ErrDLQRetryIndeterminate,
					envelope.RetryState,
					envelope.OriginalFile,
				)
			}
			originalPresentBox = BoxNew
		}
		setRetryState(envelope, RetryStateDelivered)
		updatedData, err := serializeDLQMessage(*envelope, originalContent)
		if err != nil {
			return fmt.Errorf("serialize recovered dlq envelope: %w", err)
		}
		terminalErr := fmt.Errorf(
			"%w: original file exists in inbox/%s: %s (retry already delivered)",
			ErrDLQRetryDelivered,
			originalPresentBox,
			envelope.OriginalFile,
		)
		if err := updateRetriedDLQEnvelope(root, agent, dlqFilename, dlqPath, box, updatedData); err != nil {
			// A delivery may exist, but the recovery audit did not finish. Keep
			// this operational failure distinct from a clean terminal outcome so
			// retry --all does not silently suppress it as idempotent.
			return fmt.Errorf("retry delivery exists but finalize recovered dlq envelope: %w", err)
		}
		return terminalErr
	}

	if originalPresentBox != "" {
		return fmt.Errorf("original file already exists in inbox/%s: %s (refusing retry)", originalPresentBox, envelope.OriginalFile)
	}

	if envelope.RetryCount >= MaxRetries && !force {
		return fmt.Errorf("max retries (%d) exceeded; use --force to override", MaxRetries)
	}
	envelope.RetryCount++
	setRetryState(envelope, RetryStatePending)
	updatedData, err := serializeDLQMessage(*envelope, originalContent)
	if err != nil {
		return fmt.Errorf("serialize updated dlq envelope: %w", err)
	}
	if err := updateRetriedDLQEnvelope(root, agent, dlqFilename, dlqPath, box, updatedData); err != nil {
		return err
	}
	// The envelope now lives in cur, even when the source was dlq/new.
	dlqPath = filepath.Join("agents", agent, "dlq", BoxCur, dlqFilename)
	box = BoxCur

	// Deliver original content back to inbox only after the DLQ state transition
	// succeeds, so metadata failures cannot duplicate retry delivery.
	inboxPath, deliveryErr := DeliverToInbox(root, agent, envelope.OriginalFile, originalContent)
	if deliveryErr != nil {
		var committed *CommittedDurabilityError
		if !errors.As(deliveryErr, &committed) {
			setRetryState(envelope, RetryStateReady)
			updatedData, err := serializeDLQMessage(*envelope, originalContent)
			if err != nil {
				return errors.Join(
					fmt.Errorf("redeliver to inbox: %w", deliveryErr),
					fmt.Errorf("serialize reset dlq envelope: %w", err),
				)
			}
			if err := updateRetriedDLQEnvelope(root, agent, dlqFilename, dlqPath, box, updatedData); err != nil {
				return errors.Join(
					fmt.Errorf("redeliver to inbox: %w", deliveryErr),
					fmt.Errorf("reset retried dlq envelope: %w", err),
				)
			}
			return fmt.Errorf("redeliver to inbox: %w", deliveryErr)
		}
		// The inbox rename is already visible. Finish the retry audit before
		// returning the durability error so later retries do not have to heal a
		// logically completed delivery.
		inboxPath = committed.FinalPath
	}
	setRetryState(envelope, RetryStateDelivered)
	updatedData, err = serializeDLQMessage(*envelope, originalContent)
	if err != nil {
		return fmt.Errorf("serialize completed dlq envelope: %w", err)
	}
	if err := updateRetriedDLQEnvelope(root, agent, dlqFilename, dlqPath, box, updatedData); err != nil {
		if deliveryErr != nil {
			return errors.Join(
				fmt.Errorf("redeliver to inbox: %w", deliveryErr),
				fmt.Errorf("finalize retried dlq envelope: %w", err),
			)
		}
		return &CommittedDurabilityError{
			FinalPath: inboxPath,
			Recipient: agent,
			Err:       fmt.Errorf("finalize retried dlq envelope: %w", err),
		}
	}
	if deliveryErr != nil {
		return fmt.Errorf("redeliver to inbox: %w", deliveryErr)
	}

	return nil
}

func updateRetriedDLQEnvelope(root *DeliveryRoot, agent, dlqFilename, dlqPath, box string, updatedData []byte) error {
	curDir := filepath.Join("agents", agent, "dlq", "cur")
	if err := root.root.MkdirAll(curDir, 0o700); err != nil {
		return fmt.Errorf("prepare dlq envelope cur dir: %w", err)
	}

	if box == BoxNew {
		// Source is dlq/new: write to dlq/cur atomically, then remove from dlq/new
		if _, err := root.WriteFileAtomic(curDir, dlqFilename, updatedData, 0o600); err != nil {
			return fmt.Errorf("write updated dlq envelope to cur: %w", err)
		}
		if err := root.root.Remove(dlqPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old dlq envelope from new: %w", err)
		}
		if err := root.syncDir(filepath.Dir(dlqPath)); err != nil {
			return fmt.Errorf("sync old dlq envelope dir: %w", err)
		}
		return root.syncDir(curDir)
	}

	// Source is dlq/cur: update in place atomically (same location)
	if _, err := root.WriteFileAtomic(curDir, dlqFilename, updatedData, 0o600); err != nil {
		return fmt.Errorf("update dlq envelope in cur: %w", err)
	}
	return root.syncDir(curDir)
}

// FindDLQMessage locates a DLQ message in dlq/new or dlq/cur.
//
// A same-name file in both boxes is the recoverable residue of an envelope
// update: the new state is written to cur before the old new copy is removed.
// cur is therefore authoritative whenever both exist. Prefer it so a stale
// pre-update envelope cannot hide a completed retry audit or a newer retry
// count.
func FindDLQMessage(root *DeliveryRoot, agent, filename string) (string, string, error) {
	if err := ValidateMessageFilename(filename); err != nil {
		return "", "", err
	}
	curPath := filepath.Join("agents", agent, "dlq", "cur", filename)
	if _, err := root.Stat(curPath); err == nil {
		return curPath, BoxCur, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	newPath := filepath.Join("agents", agent, "dlq", "new", filename)
	if _, err := root.Stat(newPath); err == nil {
		return newPath, BoxNew, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return "", "", os.ErrNotExist
}

// MoveDLQNewToCur moves a DLQ message from new to cur (marks as inspected).
var beforeMoveDLQNewToCurRenameForTest func(*DeliveryRoot, string, string)

func MoveDLQNewToCur(root *DeliveryRoot, agent, filename string) error {
	return root.WithDLQEnvelopeLock(agent, filename, func(batch *DeliveryRoot) error {
		return moveDLQNewToCurLocked(batch, agent, filename)
	})
}

func moveDLQNewToCurLocked(root *DeliveryRoot, agent, filename string) error {
	if err := ValidateMessageFilename(filename); err != nil {
		return err
	}
	if err := root.VerifyBase(); err != nil {
		return err
	}
	newPath := filepath.Join("agents", agent, "dlq", "new", filename)
	curDir := filepath.Join("agents", agent, "dlq", "cur")
	curPath := filepath.Join(curDir, filename)
	if err := root.root.MkdirAll(curDir, 0o700); err != nil {
		return err
	}
	if reconciled, err := reconcileDLQCurAuthorityLocked(root, agent, filename); err != nil {
		return err
	} else if reconciled {
		return nil
	}
	if beforeMoveDLQNewToCurRenameForTest != nil {
		beforeMoveDLQNewToCurRenameForTest(root, newPath, curPath)
	}
	if err := root.renameNoReplace(newPath, curPath); err != nil {
		return err
	}
	var durabilityErr error
	if err := root.syncDir(curDir); err != nil {
		durabilityErr = errors.Join(durabilityErr, fmt.Errorf("sync dlq/cur dir: %w", err))
	}
	if err := root.syncDir(filepath.Dir(newPath)); err != nil {
		durabilityErr = errors.Join(durabilityErr, fmt.Errorf("sync dlq/new dir: %w", err))
	}
	if durabilityErr != nil {
		return &CommittedDurabilityError{
			FinalPath: root.displayPath(curPath),
			Recipient: agent,
			Err:       durabilityErr,
		}
	}
	return nil
}

// reconcileDLQCurAuthorityLocked removes a stale new copy only when an
// authoritative same-name regular envelope already exists in cur. The caller
// must hold the per-envelope lock. It returns true when it observed and
// reconciled both boxes; cur-only and new-only states are left untouched.
func reconcileDLQCurAuthorityLocked(root *DeliveryRoot, agent, filename string) (bool, error) {
	newPath := filepath.Join("agents", agent, "dlq", "new", filename)
	if _, err := root.root.Lstat(newPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	curDir := filepath.Join("agents", agent, "dlq", "cur")
	curPath := filepath.Join(curDir, filename)
	curInfo, err := root.root.Lstat(curPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !curInfo.Mode().IsRegular() || curInfo.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("existing dlq/cur envelope is not a regular file: %s", root.displayPath(curPath))
	}

	// updateRetriedDLQEnvelope writes the new state to cur before removing the
	// old new copy. Never rename over cur here: that would replace the newer
	// retry audit and resurrect the stale source state.
	if err := root.root.Remove(newPath); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("remove stale dlq/new envelope: %w", err)
	}
	var durabilityErr error
	if err := root.syncDir(curDir); err != nil {
		durabilityErr = errors.Join(durabilityErr, fmt.Errorf("sync authoritative dlq/cur dir: %w", err))
	}
	if err := root.syncDir(filepath.Dir(newPath)); err != nil {
		durabilityErr = errors.Join(durabilityErr, fmt.Errorf("sync reconciled dlq/new dir: %w", err))
	}
	if durabilityErr != nil {
		return true, &CommittedDurabilityError{
			FinalPath: root.displayPath(curPath),
			Recipient: agent,
			Err:       durabilityErr,
		}
	}
	return true, nil
}

func recoverPendingInboxTmp(root *DeliveryRoot, agent, filename string, want []byte) (bool, error) {
	tmpDir := filepath.Join("agents", agent, "inbox", "tmp")
	entries, err := root.ReadDir(tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("list retained inbox tmp: %w", err)
	}

	var match string
	for _, entry := range entries {
		if entry.IsDir() || !attemptTmpMatches(filename, entry.Name()) {
			continue
		}
		tmpPath := filepath.Join(tmpDir, entry.Name())
		got, readErr := root.ReadRegularNoFollow(tmpPath)
		if readErr != nil {
			return false, fmt.Errorf("read retained inbox tmp: %w", readErr)
		}
		if bytes.Equal(got, want) {
			match = tmpPath
			break
		}
	}
	if match == "" {
		return false, nil
	}

	newDir := filepath.Join("agents", agent, "inbox", "new")
	newPath := filepath.Join(newDir, filename)
	if err := root.publishTmpNoReplace(match, newPath, want); err != nil {
		return false, fmt.Errorf("complete retained inbox tmp: %w", err)
	}
	_ = root.syncDir(newDir)
	_ = root.syncDir(tmpDir)
	return true, nil
}

// serializeDLQMessage creates a DLQ file with JSON frontmatter and original content.
func serializeDLQMessage(env DLQEnvelope, originalContent []byte) ([]byte, error) {
	if err := normalizeRetryState(&env); err != nil {
		return nil, fmt.Errorf("normalize retry state: %w", err)
	}
	header, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	var buf strings.Builder
	buf.WriteString("---\n")
	buf.Write(header)
	buf.WriteString("\n---\n")
	buf.Write(originalContent)
	return []byte(buf.String()), nil
}

// parseDLQMessage parses a DLQ file into envelope and original content.
func parseDLQMessage(data []byte) (*DLQEnvelope, []byte, error) {
	// Normalize CRLF to LF for cross-platform compatibility
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, nil, fmt.Errorf("missing frontmatter start")
	}
	rest := data[4:]
	endIdx := bytes.Index(rest, []byte("\n---\n"))
	if endIdx < 0 {
		return nil, nil, fmt.Errorf("missing frontmatter end")
	}

	headerJSON := rest[:endIdx]
	body := rest[endIdx+5:]

	var env DLQEnvelope
	if err := json.Unmarshal(headerJSON, &env); err != nil {
		return nil, nil, fmt.Errorf("parse envelope: %w", err)
	}
	if err := normalizeRetryState(&env); err != nil {
		return nil, nil, fmt.Errorf("normalize retry state: %w", err)
	}

	return &env, body, nil
}
