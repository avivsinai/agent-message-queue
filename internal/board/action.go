package board

import (
	"regexp"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Action is a copy-only command. The board never runs it. Argv is built only
// by the constructors below from validated values; a value that fails
// validation yields no action, never a weakened one.
type Action struct {
	Label   string   `json:"label"`
	Argv    []string `json:"argv"`
	Command string   `json:"command"`
}

// userLiteral is the fixed shell expansion ClaimBead appends unquoted. It is
// never data: Quote renders every argv element, this string included, literally.
const userLiteral = `"$USER"`

const reservedHuman = "user"

var (
	messageIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
	labelRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	shellSafeRe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
)

func newAction(label string, argv ...string) Action {
	return Action{Label: label, Argv: argv, Command: Quote(argv)}
}

// Quote renders argv as one POSIX shell line. Elements outside a safe
// character set are single-quoted, with ' written as '\”. Validation rejects
// control characters before any value reaches argv, so no line ends inside a
// quote.
func Quote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if shellSafeRe.MatchString(a) {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(parts, " ")
}

func validHandle(h string) bool { return fsq.ValidateHandle(h) == nil }

func validMessageID(id string) bool {
	return messageIDRe.MatchString(id) && !strings.Contains(id, "..")
}

func validThread(t string) bool {
	return t != "" && len(t) <= 256 && !hasControl(t) && !strings.HasPrefix(t, "-")
}

// AnswerActions builds "complete" and "acknowledge" for an obligation owed by
// the reserved human (design 9.2). The route is the obligation's reply
// destination, never its own session. peers is the project's .amqrc peer set.
func AnswerActions(o *ObligationFacts, peers map[string]bool) []Action {
	if o == nil || o.Destination == nil {
		return nil
	}
	d := o.Destination
	if !validHandle(d.Handle) || !validMessageID(o.OpenerID) || !validThread(o.Thread) {
		return nil
	}
	route := []string{}
	switch {
	case d.Project != "":
		if !peers[d.Project] {
			return nil
		}
		route = append(route, "--project", d.Project)
		if d.Session != "" {
			if !validHandle(d.Session) {
				return nil
			}
			route = append(route, "--session", d.Session)
		}
	case d.Session == "":
		// A local destination at the base root: --session cannot name it.
		return nil
	default:
		if !validHandle(d.Session) {
			return nil
		}
		route = append(route, "--session", d.Session)
	}
	build := func(label, kind string, labels []string) (Action, bool) {
		if !format.IsValidKind(kind) {
			return Action{}, false
		}
		argv := append([]string{"amq", "send"}, route...)
		argv = append(argv, "--me", reservedHuman, "--to", d.Handle, "--thread", o.Thread, "--refs", o.OpenerID, "--kind", kind)
		if len(labels) > 0 {
			for _, l := range labels {
				if !labelRe.MatchString(l) {
					return Action{}, false
				}
			}
			argv = append(argv, "--labels", strings.Join(labels, ","))
		}
		argv = append(argv, "--body", "-")
		return newAction(label, argv...), true
	}
	var out []Action
	if a, ok := build("complete: answer "+d.Handle, o.CompletionKind, o.CompletionLabels); ok {
		out = append(out, a)
	}
	if a, ok := build("acknowledge: the card stays", o.AcknowledgeKind, nil); ok {
		out = append(out, a)
	}
	return out
}

// MessageAgent builds a send to one agent of a session of this project.
func MessageAgent(session, handle string) (Action, bool) {
	if !validHandle(session) || !validHandle(handle) {
		return Action{}, false
	}
	return newAction("message "+handle, "amq", "send", "--session", session, "--me", reservedHuman, "--to", handle, "--body", "-"), true
}

// ShowThread builds the read-only thread inspection command.
func ShowThread(sessionRoot, thread string) (Action, bool) {
	if sessionRoot == "" || hasControl(sessionRoot) || !validThread(thread) {
		return Action{}, false
	}
	return newAction("show thread", "amq", "thread", "--root", sessionRoot, "--id", thread, "--include-body"), true
}

var repoSlugActionRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// OpenPR builds `gh pr view` for a GitHub pull request ref.
func OpenPR(ref Ref) (Action, bool) {
	rest, ok := strings.CutPrefix(ref.ID, "github.com/")
	if ref.Kind != RefForge || !ok {
		return Action{}, false
	}
	repo, n, ok := strings.Cut(rest, "#")
	if !ok || !repoSlugActionRe.MatchString(repo) || strings.Trim(n, "0123456789") != "" || n == "" {
		return Action{}, false
	}
	return newAction("open PR", "gh", "pr", "view", n, "--repo", repo, "--web"), true
}

// ShowBead builds `bd show` for an exact member of the bead vocabulary.
func ShowBead(id string, known map[string]string) (Action, bool) {
	if _, ok := known[id]; !ok || !beadShapeRe.MatchString(id) {
		return Action{}, false
	}
	return newAction("show bead", "bd", "show", id), true
}

// ClaimBead builds the claim command for an open, unowned bead.
func ClaimBead(id string, known map[string]string) (Action, bool) {
	if _, ok := known[id]; !ok || !beadShapeRe.MatchString(id) {
		return Action{}, false
	}
	argv := []string{"bd", "update", id, "--assignee"}
	return Action{Label: "claim bead", Argv: append(argv, userLiteral), Command: Quote(argv) + " " + userLiteral}, true
}
