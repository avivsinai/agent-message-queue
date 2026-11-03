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
never connected gets its channel at its first dialog: the hook records the
session, and the machine agent makes the channel, named like the session
(`claude · <project> @ <machine>`), while the session's Claude process runs.
That channel shows approvals only; a message that answers no dialog gets
"This session shows approvals only. Run /amq-remote in it to chat from
Buzz." `/amq-remote` in the session keeps the same channel and makes it a
chat. The channel is archived when the Claude process exits.
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
it replies to in a thread, or else the one dialog open in the channel.

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
  own suggestion for this session only, and its label says what it allows
  without a full path: "Allow this session: Bash(rm:*)", "Allow edits in
  <directory> this session", or "Accept edits this session". A session keeps
  its channel when its session id changes (`/clear`): the hook finds the
  binding its Claude process holds. It waits up to 600 seconds and then
  leaves the dialog to the terminal. A terminal answer closes the Buzz message.
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

## Limits

- Buzz shows commands as they are. The relay can read every dialog, so a
  command that holds a secret is visible to it.
- In a channel the machine agent made, a typed answer may need an @mention
  of the agent before Buzz delivers it to the agent. Reactions always work.
- A Codex thread that no binding serves answers its dialogs in the terminal:
  it runs in the app-server daemon and has no process of its own that would
  keep a channel of its own alive.

## Mechanism

Each seam writes a request file under
`~/.amq/remote/dialogs/<binding>/requests/`. The Claude hook whose session
no binding serves writes it under `dialogs/~<harness>-<project>/` instead,
beside `session.json`, the session record: harness, session id, label, and
the Claude process as its holder. The machine agent gives each record whose
process runs a channel, retires it when the process is gone, and hands it to
the binding whose holder is that process once the session attaches. The Buzz
agent (`amq-acp`) posts it and turns
the owner's reaction or reply into `answers/<id>.json`; the waiting seam
applies it. The first create-exclusive write of `resolved/<id>.json` decides
how the dialog ended. See [the compatibility manifest](remote-compat.md) for
the harness seams and
[the amq-remote reference](../cmd/amq-remote/README.md#approvals-from-the-buzz-dm)
for commands.
