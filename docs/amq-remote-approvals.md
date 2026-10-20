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

The hook has no matcher, a 600 second wait, and a 630 second timeout. It
exits at once with no output, so the terminal dialog decides, unless every
check passes: the session has a pin that a live `amq-remote serve` wrote for
that share, and the call belongs to a prompt that an AMQ request delivered
as its own turn. A request that Claude absorbed into a running turn gets no
DM approval. The hook never exits 2. It denies for the owner's ❌ on that
exact call, and the turn goes on.

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
