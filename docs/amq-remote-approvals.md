# Claude Code approvals from Buzz

A Claude Code target with `"approve": true`, shared by a relay share with
owner DM commands, shows a tool approval of a request that the share
submitted in the owner's Buzz DM. ❌ denies that one call; ✅ allows it only
when every check below passes. Installing the hook and the owner-facing
behavior are in [the amq-remote reference](../cmd/amq-remote/README.md#approvals-from-the-buzz-dm).
The harness seam is the `PermissionRequest` row of
[the compatibility manifest](remote-compat.md#33-claude-code).

## The pin

`amq-remote claude install-approval-hook` writes the pin on the hook's
command line, as the manifest and the enrolled keys name the share at
install time: `... claude permission-hook --wait 600 --owner '<64 hex>' --root
'<AMQ root>' --session '<name>' --relay '<relay URL>' --body '<body
pubkey>' --channel '<DM channel>' --target '<target>'`. Claude Code asks
before Claude edits its own settings file, so Claude cannot change the pin
on its own. The hook never reads the manifest: the root and session only
locate the enrolled body secret it signs in with, whose pubkey must be the
pinned body. A pin that misses any flag is reject-only.

## When the hook answers

The hook has no matcher, a 600 second wait, and a 630 second timeout. Its
command starts `amq-remote` only when some session under
`~/.claude/sessions/amq-approve` has a pin file, a test that runs inside
`sh`; on any other machine a tool dialog costs one shell. The installer
writes that path for the home it runs in, and `amq-remote doctor` fails a
hook whose path or timeout differs from what the installer writes now. The
hook exits at once with no output, so the terminal dialog decides, unless
every check passes: the session has a pin that a live `amq-remote serve`
wrote for that share, and the call belongs to a prompt that an AMQ request
delivered as its own turn. A request that Claude absorbed into a running
turn gets no DM approval. The hook never exits 2. It denies for the owner's
❌ on that exact call, and the turn goes on.

The endpoint records each request it sends to a session with approvals as
in flight before it writes the frame; when it cannot, it refuses the
request and sends nothing. When its poller sees the delivery line it
writes the prompt's run marker, and only then drops the in-flight record.
A tool call whose prompt has no run marker waits up to 2 seconds for one
only while a live in-flight record exists for that session, so a prompt
typed in the terminal gets its dialog at once, except within 2 minutes of
a request sent to that session. Known limitation: if Claude takes a
request's prompt more than 2 minutes after it was sent (for example,
queued behind a long turn), a tool call of that run that asks for
permission before the poller writes the run marker gets no Buzz approval
and the terminal decides it; later calls in the run see the marker.

Pin creation, the last unpin's check for live pins, and its cleanup run
under `~/.claude/sessions/amq-approve/lifecycle.lock`, so an old owner's
cleanup never removes a new pin or its records. When the last live pin of
a session goes, the cleanup removes only records no running process can
use: an approval whose hook process is gone or whose deadline passed 30
seconds ago, files of an approval with no request that are older than the
hook's timeout, pins, run markers and in-flight records of a process that
is gone, and expired in-flight records. A hook that is still running keeps
every record of its approval. When an attachment with approvals binds a
session, it removes the pins and in-flight records of processes that are
gone; `amq-remote doctor` lists them.

Any same-user process can write the answer file, so an allow in it proves
nothing by itself. The hook prints allow only when all of these hold:

- its command line pins an owner and a whole share;
- the call is a Bash call with only `command`, `description`, `timeout` and
  `run_in_background`, shown whole: nothing hidden, shortened, or removed
  for display;
- the answer carries the owner's kind 7 reaction ✅, or the owner's kind 9
  typed approve (`yes`, `y`, `approve` or ✅) in the share's DM channel,
  whose id and BIP-340 signature verify under the pinned key, dated from 30
  seconds before the request to its deadline;
- the reaction's last `e` tag, or one of the typed reply's `reply`, `root`
  or unmarked `e` tags, is the approval message, whose id and
  signature verify, which the share's body key signed in the share's DM
  channel, and whose content is the approval text for the hook's own
  rendering of the call under a request of the share's target: the
  command preview, then `Interaction: <id>` and `Action: <sha256 of the
  call>`;
- the hook's own read of the pinned relay, signed in with the share's
  body key as `amq-remote serve` is, each read to the end of stored events
  within 10 seconds, finds every way a Buzz client changes or hides the
  message harmless: each edit (kind 40003, by the body or by the owner,
  whom Buzz lets edit an agent's message) shows the same call with only
  another answer line or outcome, no deletion (kind 5, or the Buzz-native
  kind 9005) names the message or one of its edits, the body sent no
  deletion ever, and the owner sent no deletion since the message, with or
  without an `h` tag. The relay hides a deleted edit, AMQ never deletes,
  and a deletion's time is what its signer claims;
- the approval is still open when the hook checks, both before and after
  it verifies: its deadline has not passed, Claude has not ended the hook,
  and nothing else closed it. One allow applies once.

On a missing pin, any failed check, an error, or a timeout the hook prints
no allow. The endpoint runs the same verification before it passes a ✅ to
the hook. When it fails, nothing is answered and the DM says why:
"The approval message was altered after it was posted; check the
terminal." for an edit that changed the call or a deletion, and "Could not
verify the approval: <reason>. React again to retry." otherwise (a relay
error or timeout, for example). A later ✅ or a ❌ still works. When the
endpoint's check passes and the hook's own check then fails, the hook
records that it refused exactly that proof: the answer never applies, the
DM gives the same reply with the hook's reason, and a later ❌ or a new ✅
replaces it. The hook allows a proof only when no refusal of it is
recorded. While one answer to an approval is checked, a second one waits
for it.

The DM offers ✅ only when the call can pass these checks, the
installed hook pins the share's owner, and the approval is bound to one
tool call in the transcript; otherwise it offers ❌ only.

The pin holds only while Claude cannot change settings or run commands
without a prompt; [the amq-remote reference](../cmd/amq-remote/README.md#approvals-from-the-buzz-dm)
lists the settings that defeat it. The relay is trusted to return the full edit and deletion history of the
message. A same-user process that can replace the installed `amq-remote`
binary, or edit the Claude settings with the owner's consent, defeats this
protection.

## Buzz Desktop

A Claude session that a Buzz Desktop agent (`amq-acp`) drives with
`--native` has no relay share, and the hook holds no Buzz key. The pin then
names the owner, the agent's public key and the agent's DM channel with the
owner: `... claude permission-hook --wait 600 --surface 'desktop' --owner
'<64 hex>' --body '<agent pubkey>' --channel '<DM channel>'`, written by
`install-approval-hook --desktop`, which the owner runs in the terminal with
the agent's public key and its DM channel id. Until the hook pins its DM,
the agent shows that command, with these three values, under each
deny-only approval. A hook holds one pin: a
Desktop pin or a relay-share pin, never both. The agent key never leaves
`amq-acp`. The hook prints allow only when all of these hold:

- the call is a Bash call shown whole, as above;
- the answer carries the owner's kind 7 reaction ✅ whose id and signature
  verify under the pinned owner, dated in the approval's window, whose last
  `e` tag is the approval message;
- the approval message is kind 9, signed by the pinned agent key in the
  pinned DM channel, and its content is exactly the Desktop approval text
  for the hook's own rendering of the call (with `Interaction: <id>` and
  `Action: <sha256 of the call>`);
- the answer carries the agent's grant: a BIP-340 signature by the pinned
  agent key over the message id, the reaction id and the time the agent
  checked, dated in the approval's window. The
  agent signs it only after its own read of the relay finds the message
  stored exactly as it was posted, no edit of it at all (the agent never
  edits an approval message), no deletion of it, no deletion by the agent
  stored after it read the agent's deletions just before posting, no
  deletion by the owner since the message, and no owner ❌ on it.
  buzz-acp signs with the same key and deletes its own 👀 and 💬 after each
  turn; the relay hides what a deletion names, so an earlier deletion
  cannot be told from one that hid an edit, but only one stored after the
  post can have hidden an edit of it. That read reaches back 1800 s: the
  relay accepts a created_at up to 900 s from its clock, and it accepted
  the approval dated by this machine's clock, so a backdated deletion is
  still seen when the clocks differ; a read that does not reach 1800 s
  before the stored message's own date proves nothing. When the read
  before the post finds 500 or more deletions, fails, or falls short, the
  approval is deny-only.

The endpoint can show an approval before it reads the tool call from the
transcript, and then offers allow only once it binds the call. The agent
then posts the approval again with ✅. A ❌ on either message denies; only a
✅ on the new message can allow.

Here the agent, not the hook, reads the relay; a holder of the agent key can
sign a grant. The endpoint accepts this answer from the local socket only
for the Claude approval of a request submitted on that socket, and runs the
same verification before it passes the answer to the hook.

## How the call is shown

The message shows the call whole, or not at all: a call that may contain
a secret anywhere shows "Command hidden: it may contain a secret. Check the
terminal.", and a call too long to show whole shows "Command too long:
check the terminal." ❌ still blocks either; ✅ is never offered. The first
answer wins, in Buzz or in the terminal. When the hook stops before it
records that its decision was written whole, the message says the answer
may not have reached the terminal (`delivery_unknown`). A terminal reject
stops the hook at once.
A terminal approve does not signal the hook, so the endpoint detects it
from the call's `tool_result` in the transcript and edits the message to
say it was answered outside Buzz. When two identical calls are waiting in
one turn, the approval cannot be bound to one of them, ✅ is not offered,
and the first `tool_result` among them closes it. Another PermissionRequest
hook that decides can answer first; AMQ reports only what its own hook
printed.
