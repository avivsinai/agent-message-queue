package buzzcli

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// NotAcceptedError is a write the Buzz CLI reported with accepted:false,
// or, for the NIP-IA archive/unarchive commands, a CLI run that did not
// print their ok:true body.
// The relay refused the operation: nothing was published by this call.
// Callers must not advance lifecycle markers on this error.
type NotAcceptedError struct{ out string }

func (e *NotAcceptedError) Error() string { return "not accepted: " + e.out }

// Shape is what a buzz CLI write prints when it ran (block/buzz
// f3b127e buzz-cli).
type Shape int

const (
	// ShapeAccepted is the write response {event_id, accepted, message}.
	ShapeAccepted Shape = iota
	// ShapeOK is {ok, event_id, action, target}, printed by agents archive
	// and agents unarchive after the submit returned without error. They do
	// not print the relay's accepted flag; exit 0 with ok:true is the
	// strongest signal they give.
	ShapeOK
)

// Classify classifies what one buzz CLI write returned: its stdout, and
// the error of the run or of the wrapper that made it.
//
//   - NotSent: no Buzz identity, or a run that failed before the CLI
//     started (*NotSentError). Nothing reached the relay.
//   - Rejected: the relay refused this attempt: exit 1 (bad input), exit 3
//     (auth), a *NotAcceptedError, or exit 0 with accepted:false (ok:false
//     for ShapeOK).
//   - Accepted: exit 0 with accepted:true (ok:true for ShapeOK). An event id
//     alone is no acceptance: the CLI prints the event id of a refused write
//     too.
//   - Unknown: anything else may have taken effect: a run cut off (a timeout
//     or a signal), any other failure after the CLI started, output past its
//     limit, or exit 0 whose output does not carry the flag.
//
// Reason is the error's text, else the trimmed stdout. EventID is the
// event_id the output names, if any.
func Classify(shape Shape, out []byte, err error) buzzop.Result {
	if err != nil {
		r := buzzop.Result{Outcome: buzzop.Unknown, Reason: err.Error()}
		var refused *NotAcceptedError
		var exit *ExitError
		switch {
		case NeverSent(err):
			r.Outcome = buzzop.NotSent
		case errors.As(err, &refused), errors.As(err, &exit) && (exit.Code == 1 || exit.Code == 3):
			r.Outcome = buzzop.Rejected
		}
		return r
	}
	r := buzzop.Result{Outcome: buzzop.Unknown, Reason: strings.TrimSpace(string(out))}
	var flag *bool
	if shape == ShapeOK {
		var res struct {
			OK *bool `json:"ok"`
		}
		if json.Unmarshal(out, &res) == nil {
			flag = res.OK
		}
	} else {
		var res struct {
			Accepted *bool `json:"accepted"`
		}
		if json.Unmarshal(out, &res) == nil {
			flag = res.Accepted
		}
	}
	switch {
	case flag == nil:
		return r
	case !*flag:
		r.Outcome = buzzop.Rejected
	default:
		r.Outcome, r.Reason = buzzop.Accepted, ""
	}
	var named struct {
		EventID string `json:"event_id"`
	}
	if json.Unmarshal(out, &named) == nil {
		r.EventID = named.EventID
	}
	return r
}

// NeverSent reports that err proves the write did not reach the relay: no
// Buzz identity, or a run that failed before the CLI started.
func NeverSent(err error) bool {
	var notSent *NotSentError
	return errors.Is(err, ErrNoIdentity) || errors.As(err, &notSent)
}

// WriteError is a write's error for a caller that acts only on success: nil
// when it was accepted, the run's own error when it failed, else a
// *NotAcceptedError with the output. A refusal and an output without the
// flag both are not accepted.
func WriteError(shape Shape, out []byte, err error) error {
	switch r := Classify(shape, out, err); {
	case r.Outcome == buzzop.Accepted:
		return nil
	case err != nil:
		return err
	default:
		return &NotAcceptedError{out: r.Reason}
	}
}
