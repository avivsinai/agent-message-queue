# Approvals from Buzz

Every dialog a Claude Code session raises in its terminal also appears in
Buzz, and the owner answers it there or in the terminal. For Codex this holds
for a session on the Codex app-server. pi shows its prompts in Buzz with
"answer in the terminal". An `amq-acp host` agent shows each permission
request with every option the agent offered.

## One-time setup

1. `brew install avivsinai/tap/amq`.
2. Install Buzz Desktop and the Buzz phone app.
3. `amq-acp setup --machine`, then import the printed agent file in Buzz
   Desktop and start it.
4. `amq-remote claude install-approval-hook`.

That is all a Claude desktop-app user needs. Every Claude Code session on the
machine (terminal, desktop app Code tab, IDE) then mirrors its dialogs to
Buzz, with no `/amq-remote`. Each session posts in its own channel, as a
connected session does, or in its DM for a pinned agent. A session that was
never connected gets its channel at its first dialog: the hook binds the
session as `/amq-remote` does, and the machine agent makes its channel,
named like the session, and archives it when the session ends. An AMQ
session (`AM_ROOT` and `AM_ME` set) gets its own binding (the mailbox one
`/amq-remote` writes), so a message in the channel reaches its AMQ inbox. Any other session (the desktop app, a
plain terminal) gets the native binding of `attach --self --native`, named
`claude · <project> @ <machine>`; a message there is not submitted until
`/amq-remote --native` runs in the session. A later `/amq-remote` keeps the
same binding and channel. `/amq-remote off` stops it for that session's
process: the hook connects that process again only after `/amq-remote`; a
new Claude session is connected as usual. A binding that the session's
Claude process holds keeps serving it after `/clear`, as attach wrote it.
`amq-remote claude uninstall-approval-hook` removes the hook.

## The Buzz message

The agent posts each dialog once:

```text
claude · myrepo wants to run Bash:
$ go test ./...
(Run tests)

1. Allow once
2. Allow this session: Bash(go test:*)
3. Deny

React ✅ = 1, ❌ = Deny, or reply a number / yes / no.
```

The first line names the session that asks and the tool. The body is the
dialog text as the harness shows it: the command, diff, URL or MCP
arguments. A body over 3500 bytes is cut and ends with "… full text in the
terminal"; the options are still offered. A dialog that asks for text adds
"or reply with your answer". A dialog with no options ends with "Answer in
the terminal."

When the dialog closes, the message is edited with one more line:
`→ <option label> (Buzz)`, `→ Answered in the terminal`, or `→ Expired`.

## Answers

Only the agent's owner answers. The agent reads the owner's reactions on an
open dialog every 3 seconds. A typed reply answers the dialog whose message
it replies to in a thread (a NIP-10 root or reply tag; a mention does not
count), or, when it replies to no message, the one dialog open in the
channel. A typed answer must be signed no earlier than its dialog opened
(with 60 seconds of clock skew) and answers one dialog only.

A typed answer counts only as its signed event. The agent fetches the
reply's event from the relay as itself, and the answer is the event's signed
content and thread tags, signed by the owner's key; the prompt text buzz-acp
wrote never decides. When the event cannot be verified (the relay does not
serve it, it is not a message in this channel, or the relay is unreachable),
the dialog stays open and the channel gets one line saying why, at most once
every 10 minutes for the same open dialogs.

| Owner's answer | Picks |
| --- | --- |
| ✅ reaction | option 1 |
| ❌ reaction | the deny option; ❌ beats ✅ |
| `1`, `2`, ... | that option |
| `yes`, `y`, `approve`, `✅` | option 1 |
| `no`, `n`, `reject`, `❌` | the deny option |
| any other text | the answer, for a dialog that asks for text |

Case and a trailing `.` or `!` are ignored. A reply that is no answer, or
that matches no single open dialog, goes to the session as a normal message.
The first answer wins, in Buzz or in the terminal; a later typed answer gets
"Already answered."

## Per harness

- **Claude Code:** the `PermissionRequest` hook shows every dialog of every
  session: "Allow once", a session-wide option when Claude suggests one or
  the tool edits files, and "Deny". The session-wide option applies Claude's
  own suggestion for this session only, and its label says what it allows,
  each path as Claude gives it with the home directory written `~`: "Allow
  this session: Bash(rm -rf ~/other/node_modules)", "Allow edits in ~/proj
  this session", "Stop allowing edits in ~/proj this session", or "Accept
  edits this session". After `/clear` the hook finds the binding its Claude
  process holds, so the session keeps its binding and channel. It waits until
  Buzz or the terminal answers, at most the installed hook timeout of one
  day. A terminal answer, or the end of its Claude process, closes the Buzz
  message.
- **Codex:** only a session on the app-server (`codex app-server daemon
  start`, then `codex --remote unix://`). Every approval shows Codex's own
  `availableDecisions`; a user-input or elicitation request takes the
  owner's typed answer. Codex runs `PermissionRequest` hooks before it draws
  the TUI prompt, so AMQ installs no Codex hook, and a plain `codex` TUI
  session answers its approvals in the terminal.
- **pi:** pi has no extension API that answers a prompt from outside. In a
  session connected with `/amq-remote`, the bridge posts each prompt with
  "Answer in the terminal", and the message closes when the prompt ends.
- **`amq-acp host`:** every `session/request_permission` shows all the
  options the agent offered, and the agent receives the chosen one.

## Slash commands

In Buzz, the owner's message that is one line starting with `/` is typed
into a Claude Code session in mailbox mode like Remote Control: the session's
wake types exactly that line and Enter, by the same injection as its
doorbell, and Buzz gets "Typed /compact into the terminal." Any command is
typed, `/clear` and `/exit` too; the Buzz link survives `/clear`, and
`/exit` ends the session. The line counts only as the
owner's signed event, at most 5 minutes old; a `/` line that is not verified
as the owner's goes to the session as an ordinary message, and only in the
channel made for that session. The queued request carries the signed
message, and the wake verifies it again before it types: the session it was
taken for, the owner key and channel that the Buzz agent recorded for the
share, and the exact command line. While a dialog is open the command
waits, oldest first, and Buzz gets "Queued"; it is typed when the dialog
closes, and Buzz is told again. A command still waiting 10 minutes after
the owner sent it is not typed: Buzz gets "Not typed: expired while a
dialog was open." At most 16 commands (16 KiB) wait at once. The wake types
each signed message once. If a dialog opens between the text and its
Enter, the text waits in the composer, and the Enter follows when the
dialog closes only if the command still verifies; otherwise Buzz gets "Not
typed" with the reason. After each command the wake waits
one full tick with no dialog; after `/model`, `/config`, `/permissions` or
`/resume`, which open a picker, it waits for the owner's next message or 60
seconds. Buzz
says "typed", never "ran": the wake cannot see what the command did. A
session with no live wake, a wake with inject mode `none` or native
delivery, a native share, another harness, or a line with a control
character or more than 512 bytes gets one line saying why it was not typed.

## Limits

- Buzz shows commands as they are. The relay can read every dialog, so a
  command that holds a secret is visible to it.
- In a channel the machine agent made, a typed answer may need an @mention
  of the agent before Buzz delivers it to the agent. Reactions always work.
- A Codex thread that no binding serves answers its dialogs in the terminal:
  it runs in the app-server daemon and has no process of its own that would
  keep a channel of its own alive.
- One Claude process with two bindings (two roots) posts its dialogs in the
  first of them by name.
- A `/amq-remote off` at the same moment as the session's first dialog can
  race: the dialog may connect the session again.

## Mechanism

Each seam writes a request file under
`~/.amq/remote/dialogs/<binding>/requests/`. The Claude hook takes, in
order: the binding of the session's AMQ handle or session id; a binding, not
proven ended, whose holder is exactly the Claude process it runs under (the
nearest ancestor whose Claude session registry entry names the session, else
the nearest with any entry), used as it is; else a binding it writes with the code `amq-remote attach
--self` uses, without registering a target or needing the Stop hook: the
mailbox binding of `AM_ME` in `AM_ROOT` for an AMQ session, else the native
binding in `AM_ROOT` when it is an absolute existing directory, else in the
machine root `~/.amq/remote/root`. The native binding needs only a registry
entry for the Claude process, no messaging socket. It writes nothing for a
process whose binding the owner detached; with no binding, the terminal
decides. The Buzz agent (`amq-acp`) posts it and turns
the owner's reaction or reply into `answers/<id>.json`; the waiting seam
applies it. The first create-exclusive write of `resolved/<id>.json` decides
how the dialog ended. See [the compatibility manifest](remote-compat.md) for
the harness seams and
[the amq-remote reference](../cmd/amq-remote/README.md#approvals-from-the-buzz-dm)
for commands.
