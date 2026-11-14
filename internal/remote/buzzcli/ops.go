package buzzcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// Exec runs one buzz CLI invocation and returns its stdout.
type Exec func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)

// Ops are the typed Buzz operations over one Exec. Each operation runs one
// fixed buzz CLI argv. The CLI makes its own channel and event ids, so every
// Key is ignored.
type Ops struct{ Exec Exec }

// uuidRe is a channel id as the buzz CLI prints it.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// write classifies a write that prints the write response. Reason, for an
// outcome other than Accepted, is the run's error text when the run failed,
// else the *NotAcceptedError text of its output.
func write(out []byte, err error) buzzop.Result {
	r := Classify(ShapeAccepted, out, err)
	if r.Outcome != buzzop.Accepted && err == nil {
		r.Reason = (&NotAcceptedError{out: r.Reason}).Error()
	}
	return r
}

// CreateChannel creates a private stream channel named spec.Name with the
// description spec.About. The CLI chooses the channel id, so a spec with an
// ID is NotSent. Only an outcome that proves no channel was made is NotSent
// or Rejected: a run refused before or by the relay, or accepted:false.
// Accepted needs accepted:true and a channel id, which is lowercased. A run
// cut off, any other failure, output without the flag, or accepted:true
// without a channel id may have made one, so it is Unknown.
func (o Ops) CreateChannel(ctx context.Context, spec buzzop.ChannelSpec) buzzop.Created {
	if spec.ID != "" {
		return buzzop.Created{Result: buzzop.Result{Outcome: buzzop.NotSent,
			Reason: "buzz channels create: the CLI cannot create a channel with a chosen id"}}
	}
	out, err := o.Exec(ctx, nil, "channels", "create", "--name", spec.Name, "--type", "stream",
		"--visibility", "private", "--description", spec.About)
	r := Classify(ShapeAccepted, out, err)
	if err != nil {
		return buzzop.Created{Result: r}
	}
	var created struct {
		ChannelID string `json:"channel_id"`
	}
	switch {
	case r.Outcome == buzzop.Unknown || json.Unmarshal(out, &created) != nil:
		r.Outcome, r.Reason = buzzop.Unknown, "buzz channels create: unreadable output"
	case r.Outcome == buzzop.Rejected:
		r.Reason = "buzz channels create: the relay refused it: " + r.Reason
	case !uuidRe.MatchString(created.ChannelID):
		r.Outcome, r.Reason = buzzop.Unknown, "buzz channels create: no channel id in its output"
	default:
		return buzzop.Created{Result: r, ChannelID: strings.ToLower(created.ChannelID)}
	}
	return buzzop.Created{Result: r}
}

// AddMember adds pubkey to channel with role.
func (o Ops) AddMember(ctx context.Context, _ buzzop.Key, channel, pubkey, role string) buzzop.Result {
	return write(o.Exec(ctx, nil, "channels", "add-member", "--channel", channel, "--pubkey", pubkey, "--role", role))
}

// SetArchived archives channel, or unarchives it when archived is false.
// Reason keeps the relay's words, which say when a channel is not archived.
func (o Ops) SetArchived(ctx context.Context, _ buzzop.Key, channel string, archived bool) buzzop.Result {
	verb := "unarchive"
	if archived {
		verb = "archive"
	}
	return write(o.Exec(ctx, nil, "channels", verb, "--channel", channel))
}

// Send posts m.Content in m.Channel, mentioning m.Mention when it is set. The
// CLI port does not send a reply, so a message with ReplyTo is NotSent.
func (o Ops) Send(ctx context.Context, m buzzop.Message) buzzop.Result {
	if m.ReplyTo != "" {
		return buzzop.Result{Outcome: buzzop.NotSent, Reason: "buzz messages send: the CLI port does not send a reply"}
	}
	args := []string{"messages", "send", "--channel", m.Channel, "--content", m.Content}
	if m.Mention != "" {
		args = append(args, "--mention", m.Mention)
	}
	return write(o.Exec(ctx, nil, args...))
}

// FindChannels lists the private channels the agent is a member of and
// returns the ids of those called name whose id is a UUID, in list order and
// as the CLI printed them. A failed run or unreadable output is an error,
// which proves nothing.
func (o Ops) FindChannels(ctx context.Context, name string) ([]string, error) {
	out, err := o.Exec(ctx, nil, "channels", "list", "--member", "--visibility", "private")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		ChannelID string `json:"channel_id"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("buzz channels list: unreadable output: %w", err)
	}
	var ids []string
	for _, c := range listed {
		if c.Name == name && uuidRe.MatchString(c.ChannelID) {
			ids = append(ids, c.ChannelID)
		}
	}
	return ids, nil
}

// ChannelFacts reads channel's name, description and member roles. The CLI
// does not say whether a channel is private or archived, so both stay
// TriUnknown. A failed run or unreadable output is an error, which proves
// nothing.
func (o Ops) ChannelFacts(ctx context.Context, channel string) (buzzop.ChannelFacts, error) {
	out, err := o.Exec(ctx, nil, "channels", "get", "--channel", channel)
	if err != nil {
		return buzzop.ChannelFacts{}, err
	}
	var got struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return buzzop.ChannelFacts{}, fmt.Errorf("buzz channels get: unreadable output: %w", err)
	}
	out, err = o.Exec(ctx, nil, "channels", "members", "--channel", channel)
	if err != nil {
		return buzzop.ChannelFacts{}, err
	}
	var members []struct {
		PubKey string `json:"pubkey"`
		Role   string `json:"role"`
	}
	if err := json.Unmarshal(out, &members); err != nil {
		return buzzop.ChannelFacts{}, fmt.Errorf("buzz channels members: unreadable output: %w", err)
	}
	f := buzzop.ChannelFacts{Channel: channel, Name: got.Name, About: got.Description, Roles: map[string]string{}}
	for _, m := range members {
		f.Roles[m.PubKey] = m.Role
	}
	return f, nil
}
