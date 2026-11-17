package acp

import (
	"context"
	"errors"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// posts are the Buzz writes and reads of dialogs, the session mirror and the
// hook warnings. A ctx deadline bounds a call; a ctx without one gets the
// server's PostTimeout.
type posts interface {
	Send(ctx context.Context, m buzzop.Message) buzzop.Result
	Edit(ctx context.Context, key buzzop.Key, channel, eventID, content string) buzzop.Result
	Reactions(ctx context.Context, eventID string) ([]buzzop.Reaction, error)
}

// writeError is a Buzz write that was not accepted, as an error. Its text is
// the result's Reason.
type writeError struct{ buzzop.Result }

func (e *writeError) Error() string { return e.Reason }

// resultError is nil for an accepted write, else its *writeError.
func resultError(r buzzop.Result) error {
	if r.Outcome == buzzop.Accepted {
		return nil
	}
	return &writeError{r}
}

// notSent reports whether err is a Buzz write proved not sent.
func notSent(err error) bool {
	var w *writeError
	return errors.As(err, &w) && w.Outcome == buzzop.NotSent
}
