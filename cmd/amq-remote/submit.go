package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/sender"
)

func submit(args []string, stdin io.Reader, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	text := fs.String("text", "", "prompt text")
	textFile := fs.String("text-file", "", "read prompt text from a file")
	useStdin := fs.Bool("stdin", false, "read prompt text from stdin")
	busy := fs.String("busy", string(protocol.BusyReject), "when the runtime is busy: reject (queue is refused in v1)")
	deliver := fs.String("deliver", string(protocol.DeliverTurn), "turn (steer is refused in v1)")
	requestID := fs.String("request-id", "", "caller-supplied UUID so a retry reconciles instead of resubmitting")
	window := fs.Duration("admit-within", 2*time.Minute, "latest admission time relative to now (max 24h)")
	minEvidence := fs.String("min-evidence", "", "minimum submit evidence class to require (admitted or submitted; omitted = legacy)")
	epochFlag := fs.String("epoch", "", "registration epoch (offline enqueue: previously-verified epoch; skips live inspect)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "TARGET is required")
	}
	sources := 0
	for _, set := range []bool{*text != "", *textFile != "", *useStdin} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "exactly one of --text, --text-file, --stdin is required")
	}
	body := *text
	switch {
	case *textFile != "":
		data, err := os.ReadFile(*textFile)
		if err != nil {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "read --text-file: %v", err)
		}
		body = string(data)
	case *useStdin:
		data, err := io.ReadAll(io.LimitReader(stdin, protocol.MaxInputBytes+1))
		if err != nil {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "read stdin: %v", err)
		}
		body = string(data)
	}
	if strings.TrimSpace(body) == "" {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "prompt is empty")
	}
	if *window <= 0 || *window > 24*time.Hour {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--admit-within must be within (0, 24h]")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	target := pos[0]
	// Resolve target+epoch. A live inspect is the default; --epoch supplies a
	// previously-verified epoch for offline enqueue (the companion is not
	// running, or the target was inspected earlier). The spool never
	// retargets: it stores exactly the epoch the caller supplied, so an
	// offline enqueue cannot silently bind to a different session.
	epoch := *epochFlag
	if epoch == "" {
		session, err := inspectTarget(stateDir, target)
		if err != nil {
			return nil, 0, err
		}
		epoch = session.Epoch
	}
	id := *requestID
	if id == "" {
		id, err = newUUID()
		if err != nil {
			return nil, 0, err
		}
	}
	cmd := &protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: id,
		TargetID:  target,
		Epoch:     epoch,
		NotAfter:  protocol.FormatTime(time.Now().Add(*window)),
		Input:     &protocol.SubmitInput{Text: body, Busy: protocol.Busy(*busy), Deliver: protocol.Deliver(*deliver), MinEvidence: *minEvidence},
	}
	// Durable sender: persist the exact command, identity, destination,
	// target, epoch and expiry atomically BEFORE returning submitted. If the
	// endpoint is unreachable or crashes mid-dispatch, the companion's drainer
	// replays this envelope after restart with the same identity and bytes.
	spool, err := sender.Open(stateDir)
	if err != nil {
		return nil, 0, err
	}
	env := &sender.Envelope{
		RequestID:   id,
		CreatorHost: ipc.LocalHost,
		TargetID:    target,
		Epoch:       epoch,
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:" + stateDir,
	}
	if err := spool.Create(env); err != nil {
		return nil, 0, err
	}
	// Attempt live dispatch. If the endpoint is up, return its reply and mark
	// the envelope dispatched. If it is unreachable, return a `submitted`
	// receipt from the spool: the intent is durable and the drainer will
	// dispatch it when the companion runs.
	rep, derr := callReply(stateDir, cmd)
	if derr != nil {
		// A duplicate (request_conflict) means a prior submit for this id
		// already reached the endpoint; surface the stored reply instead of
		// a spool receipt.
		if isDuplicateConflict(derr) {
			// The receipt is NOT a Snapshot: it never mints a request ref
			// or revision the endpoint did not assign.
			return env.Receipt("duplicate_conflict"), exitForState(protocol.StateDispatching), nil
		}
		if isEndpointUnreachable(derr) {
			// The intent is persisted; the caller is told `submitted`, not
			// an error. The drainer replays after restart. The receipt is a
			// sender-side SpoolReceipt, not a fabricated Snapshot.
			return env.Receipt("endpoint_unreachable"), protocol.ExitSuccess, nil
		}
		return nil, 0, derr
	}
	// The endpoint returns refusals (stale_epoch, expired, unsupported,
	// etc.) as a Reply with Outcome.Code and nil error. A non-empty code
	// means the endpoint REFUSED the command — the envelope must NOT be
	// marked dispatched. Mark it failed (terminal) so the caller learns the
	// truth via status; a transient code (busy) is left pending for the
	// drainer to retry.
	if rep.Outcome.Code != "" {
		code := rep.Outcome.Code
		switch code {
		case protocol.CodeBusy, protocol.CodeDraining, protocol.CodeStorageFull:
			// Transient: leave pending, drainer retries next tick.
			_ = spool.MarkAttempt(sender.Key{CreatorHost: ipc.LocalHost, RequestID: id}, string(code))
		default:
			// Terminal refusal: mark failed with the code.
			_ = spool.MarkFailed(sender.Key{CreatorHost: ipc.LocalHost, RequestID: id}, string(code))
		}
		return rep, exitForOutcome(rep), nil
	}
	_ = spool.MarkDispatched(sender.Key{CreatorHost: ipc.LocalHost, RequestID: id})
	return rep, exitForOutcome(rep), nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// exitForState maps a spool-side state to the exit code for a `submitted`
// receipt. A pending/dispatching request is success for submit.
func exitForState(s protocol.State) int {
	switch s {
	case protocol.StateRejected, protocol.StateUncertain:
		return protocol.ExitActionRequired
	case protocol.StateFailed, protocol.StateCancelled:
		return protocol.ExitError
	}
	return protocol.ExitSuccess
}
