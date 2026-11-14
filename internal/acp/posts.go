package acp

import (
	"context"
	"errors"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzcli"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// posts are the Buzz writes and reads of dialogs, the session mirror and the
// hook warnings, behind one interface so a transport other than the buzz
// CLI can serve them. A ctx deadline bounds a call; a ctx without one gets
// the server's PostTimeout.
type posts interface {
	Send(ctx context.Context, m buzzop.Message) buzzop.Result
	Edit(ctx context.Context, key buzzop.Key, channel, eventID, content string) buzzop.Result
	Reactions(ctx context.Context, eventID string) ([]buzzop.Reaction, error)
	Media(ctx context.Context, url string, limit int) ([]byte, error)
}

// cliPosts are posts through the buzz CLI. The CLI chooses each event, so
// keys, mentions, replies and the channel of an edit are not used.
type cliPosts struct{ s *Server }

// budget is the time left to ctx's deadline, else the server's PostTimeout
// as it is at the call.
func (c cliPosts) budget(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return c.s.cfg.PostTimeout
}

func (c cliPosts) Send(ctx context.Context, m buzzop.Message) buzzop.Result {
	id, err := postAnswer(m.Channel, m.Content, c.budget(ctx))
	return cliResult(id, err)
}

func (c cliPosts) Edit(ctx context.Context, _ buzzop.Key, _, eventID, content string) buzzop.Result {
	return cliResult("", editMessage(eventID, content, c.budget(ctx)))
}

func (c cliPosts) Reactions(ctx context.Context, eventID string) ([]buzzop.Reaction, error) {
	rs, err := readReactions(eventID, c.budget(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]buzzop.Reaction, len(rs))
	for i, r := range rs {
		out[i] = buzzop.Reaction(r)
	}
	return out, nil
}

func (c cliPosts) Media(ctx context.Context, url string, limit int) ([]byte, error) {
	return mediaGet(c.budget(ctx), limit, url)
}

// cliResult is the outcome of a CLI write that returned event id and err:
// Accepted on no error, else what the error proves, with its text as Reason.
func cliResult(id string, err error) buzzop.Result {
	if err != nil {
		return buzzcli.Classify(buzzcli.ShapeAccepted, nil, err)
	}
	return buzzop.Result{Outcome: buzzop.Accepted, EventID: id}
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
